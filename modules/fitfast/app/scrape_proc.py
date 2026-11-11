"""Scrape a FitGirl page in a throwaway subprocess with a hard timeout.

Same reasoning as app/resolve_proc.py, and the same failure it exists to
prevent: Camoufox's browser launch has no timeout of its own, so a wedged
launch parks the calling thread forever. In the TUI that is worse than for a
resolve — the fetch runs on self._fetch_thread, and action_fetch skips when
that thread is still alive. A wedged daemon thread never exits, so Fetch goes
quiet for the rest of the process's life with nothing logged and no error in
the UI: the button simply stops working until a restart.

There is no reliable in-process cure (the hang is inside a synchronous
Playwright call, and tearing the context down from a watchdog can deadlock on
the same lock), so the scrape runs as its own process and the OS guarantees
termination where the library cannot.
"""
from __future__ import annotations

import json
import subprocess
import sys
import threading
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent

if __package__ in (None, ""):
    sys.path.insert(0, str(REPO_ROOT))
    from app.resolve_proc import _kill_tree, _popen_kwargs
else:
    from .resolve_proc import _kill_tree, _popen_kwargs

# Generous: a Cloudflare challenge plus an ad-heavy page is slow. Past this it
# is a hang, not a slow page.
SCRAPE_TIMEOUT_S = 180.0


class ScrapeTimeout(Exception):
    """The scrape subprocess had to be killed."""


def _worker_argv(page_url: str) -> list[str]:
    """Command that scrapes one page and prints JSON on stdout."""
    return [sys.executable, "-m", "app.scrape_proc", page_url]


class SubprocessScraper:
    """Runs scrape_fitgirl_page out of process so it cannot wedge the caller."""

    def __init__(self, timeout: float = SCRAPE_TIMEOUT_S) -> None:
        self.timeout = timeout
        self._closed = False
        self._proc: subprocess.Popen | None = None
        self._lock = threading.Lock()

    def close(self) -> None:
        self._closed = True
        with self._lock:
            proc = self._proc
        if proc is not None and proc.poll() is None:
            _kill_tree(proc)

    def scrape(self, page_url: str, fast: bool = True):
        """Return a ScrapeResult. Raises ScrapeError-compatible errors.

        With ``fast`` (the default) a browserless fetch is tried first; the
        browser subprocess only runs if that fails.
        """
        if __package__ in (None, ""):
            from app.scraper import ScrapeError, ScrapeResult, fast_scrape
        else:
            from .scraper import ScrapeError, ScrapeResult, fast_scrape

        if self._closed:
            raise ScrapeError("scraper has been closed")
        if fast:
            try:
                return fast_scrape(page_url)
            except ScrapeError:
                pass  # fall through to the browser

        proc = subprocess.Popen(
            _worker_argv(page_url),
            stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
            cwd=str(REPO_ROOT), **_popen_kwargs()
        )
        with self._lock:
            closed = self._closed
            if not closed:
                self._proc = proc
        if closed:
            _kill_tree(proc)
            raise ScrapeError("scraper was closed mid-scrape")

        try:
            try:
                out, err = proc.communicate(timeout=self.timeout)
            except subprocess.TimeoutExpired:
                _kill_tree(proc)
                raise ScrapeTimeout(
                    f"fetch exceeded {self.timeout:.0f}s and was killed — the "
                    "browser wedged; try again")
        finally:
            with self._lock:
                if self._proc is proc:
                    self._proc = None

        if self._closed:
            raise ScrapeError("scraper was closed mid-scrape")

        if proc.returncode != 0:
            tail = (err or "").strip().splitlines()
            raise ScrapeError(tail[-1] if tail else f"worker exit {proc.returncode}")

        for line in (out or "").splitlines():
            line = line.strip()
            if not line.startswith("{"):
                continue  # ignore library chatter on stdout
            try:
                data = json.loads(line)
            except ValueError:
                continue
            if isinstance(data.get("links"), list):
                return ScrapeResult(data["links"], data.get("title") or None)
        raise ScrapeError("worker produced no result")


def _worker_main(argv: list[str]) -> int:
    """Child entry point: scrape one page, print JSON, exit."""
    if not argv:
        print("usage: scrape_proc <page_url>", file=sys.stderr)
        return 2
    sys.path.insert(0, str(REPO_ROOT))
    from app.scraper import scrape_fitgirl_page

    try:
        # One attempt: the parent enforces a hard kill, and retrying the whole
        # subprocess beats retrying inside a process that may be the wedged one.
        result = scrape_fitgirl_page(argv[0], attempts=1)
    except Exception as e:
        # Bare message, not a traceback: the parent surfaces our last stderr
        # line straight to the user.
        print(str(e), file=sys.stderr)
        return 1
    # A bare marker line keeps us immune to whatever the browser prints.
    print(json.dumps({"links": result.links, "title": result.title}), flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(_worker_main(sys.argv[1:]))
