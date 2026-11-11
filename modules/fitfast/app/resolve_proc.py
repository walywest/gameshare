"""Resolve a link in a throwaway subprocess, with a hard timeout.

Why this exists
---------------
``Resolver.resolve()`` drives Camoufox/Playwright, and that call can block
*forever*: the browser launch itself has no timeout, so if it wedges (or the
browser process dies in a way the sync API never notices) the calling thread
is stuck with no way out. Observed in the wild twice — it froze the TUI's
resolver thread and a headless driver, in both cases leaving the download
dead while the process still looked alive and busy.

There is no reliable in-process cure: the hang happens inside a synchronous
Playwright call, so no amount of "close the browser from a watchdog thread"
is guaranteed to unblock it, and tearing down a half-initialised context can
deadlock on the same lock the hung call holds.

So resolution runs in a *separate process* and the parent enforces the
timeout. A hung browser dies with its process — the OS guarantees what the
library cannot — and the parent carries on. The whole process tree is killed
so no orphan Firefox is left behind.

``SubprocessResolver`` is API-compatible with ``Resolver`` (``resolve()`` /
``close()``), so callers swap one for the other. The cost is a browser launch
per link (~7-15s) instead of a reused one; against a 500 MB download that is
noise, and it buys a resolver that cannot wedge the app.
"""
from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import threading
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent

# Generous: real resolves take ~7-35s (Cloudflare challenge + two clicks).
# Anything past this is a hang, not a slow page.
RESOLVE_TIMEOUT_S = 180.0


class ResolveTimeout(Exception):
    """The resolve subprocess had to be killed."""


def _worker_argv(url: str) -> list[str]:
    """Command that resolves one URL and prints JSON on stdout."""
    return [sys.executable, "-m", "app.resolve_proc", url]


def _popen_kwargs() -> dict:
    """Detach the child into its own session so we can kill its whole tree."""
    return {"start_new_session": True}


def _kill_tree(proc: subprocess.Popen) -> None:
    """Kill the worker *and* any browser it spawned."""
    try:
        os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
    except (ProcessLookupError, PermissionError, OSError):
        try:
            proc.kill()
        except OSError:
            pass
    try:
        proc.wait(timeout=10)
    except Exception:
        pass


class SubprocessResolver:
    """Drop-in replacement for :class:`~app.resolver.Resolver` that cannot hang."""

    def __init__(self, timeout: float = RESOLVE_TIMEOUT_S) -> None:
        self.timeout = timeout
        self._closed = False
        # The in-flight worker, so close() can actually stop it. Without this
        # a cancelled resolve keeps its Camoufox browser alive until the
        # worker finishes or hits the full timeout — minutes of CPU, memory
        # and network competing with the batch the user still wants.
        self._proc: subprocess.Popen | None = None
        self._lock = threading.Lock()

    def close(self) -> None:
        self._closed = True
        with self._lock:
            proc = self._proc
        if proc is not None and proc.poll() is None:
            _kill_tree(proc)

    def resolve(self, url: str, retries: int = 1, fast: bool = True) -> tuple[str, str]:
        """Return (signed_url, filename). Raises ResolveError-compatible errors.

        With ``fast`` (the default) a browserless HTTP resolve is tried first;
        the browser subprocess only runs if that fails for a reason it might
        get past (a dead file fails straight away).
        """
        from .resolver import DeadLinkError, ResolveError, fast_resolve

        if self._closed:
            raise ResolveError("resolver has been closed")
        if fast:
            try:
                return fast_resolve(url)
            except DeadLinkError:
                raise
            except ResolveError:
                pass  # fall through to the browser

        last = "unknown error"
        for _attempt in range(retries + 1):
            # Re-check per attempt: a close() landing mid-resolve must not be
            # followed by another browser launch for a cancelled job.
            if self._closed:
                raise ResolveError("resolver has been closed")
            try:
                return self._resolve_once(url)
            except ResolveTimeout as e:
                last = str(e)
            except Exception as e:
                last = str(e)
        raise ResolveError(f"resolve failed after retries: {last}")

    def _resolve_once(self, url: str) -> tuple[str, str]:
        from .resolver import ResolveError

        proc = subprocess.Popen(
            _worker_argv(url),
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
            cwd=str(REPO_ROOT), **_popen_kwargs()
        )
        # Register and re-check under one lock: otherwise a close() landing
        # between Popen and registration finds _proc still None, kills nothing,
        # and the worker it missed runs to the full timeout. Either close()
        # sees the handle, or registration sees the flag — no ordering leaks.
        with self._lock:
            closed = self._closed
            if not closed:
                self._proc = proc
        if closed:
            # Decide under the lock, kill outside it: _kill_tree ends in a
            # wait(timeout=10), and holding the lock across that would stall a
            # concurrent close() just trying to read the handle.
            _kill_tree(proc)
            raise ResolveError("resolver was closed mid-resolve")
        try:
            try:
                out, err = proc.communicate(timeout=self.timeout)
            except subprocess.TimeoutExpired:
                _kill_tree(proc)
                raise ResolveTimeout(
                    f"resolve exceeded {self.timeout:.0f}s and was killed")
        finally:
            with self._lock:
                if self._proc is proc:
                    self._proc = None
        if self._closed:
            raise ResolveError("resolver was closed mid-resolve")

        if proc.returncode != 0:
            tail = (err or "").strip().splitlines()
            raise ResolveError(tail[-1] if tail else f"worker exit {proc.returncode}")

        for line in (out or "").splitlines():
            line = line.strip()
            if not line.startswith("{"):
                continue  # ignore library chatter on stdout
            try:
                data = json.loads(line)
            except ValueError:
                continue
            if data.get("signed"):
                return data["signed"], data.get("filename") or ""
        raise ResolveError("worker produced no result")


def _worker_main(argv: list[str]) -> int:
    """Child entry point: resolve one URL, print JSON, exit."""
    if not argv:
        print("usage: resolve_proc <url>", file=sys.stderr)
        return 2
    sys.path.insert(0, str(REPO_ROOT))
    from app.resolver import Resolver

    r = Resolver()
    try:
        signed, filename = r.resolve(argv[0], retries=0)
        # A bare marker line keeps us immune to whatever the browser prints.
        print(json.dumps({"signed": signed, "filename": filename}), flush=True)
        return 0
    finally:
        try:
            r.close()
        except Exception:
            pass


if __name__ == "__main__":
    raise SystemExit(_worker_main(sys.argv[1:]))
