"""FitFast web UI: turn a FitGirl page (or FuckingFast links) into direct links.

The web UI never downloads anything. It does the part that needs a stealth
browser — getting past Cloudflare on fitgirl-repacks.site and fuckingfast.co —
and hands back the signed https://dl.fuckingfast.co/dl/<token> URLs. Those need
no cookies, so the visitor pastes them into JDownloader, aria2, a browser, or
whatever they like.

Design notes
------------
* stdlib only (http.server): no extra dependency to keep alive in devenv.
* Every browser launch (fallback only) goes through the existing subprocess wrappers
  (SubprocessScraper / SubprocessResolver), so a wedged Camoufox is killed by
  the OS instead of parking a request thread forever.
* Links are resolved over plain HTTP first (curl_cffi, ~0.3s each, see
  resolver.fast_resolve); the browser is only a fallback. A global semaphore
  caps concurrent fallback browsers (FITFAST_WEB_PARALLEL): each one is a full
  Firefox, and letting an internet-facing page launch them unbounded is how the
  host falls over.
* HTTP Basic auth guards everything. The password is PASS from the repo's
  .env (gitignored); the server refuses to start without one.
* Jobs live in memory only and expire after JOB_TTL_S.

Run:  python fitfast_web.py      (inside `devenv shell`)
"""
from __future__ import annotations

import base64
import hmac
import json
import os
import re
import secrets
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from .bootstrap import browser_ready, browser_staleness_warning
from .config import APP_VERSION
from .log import get_logger, setup_logging
from .resolve_proc import SubprocessResolver
from .resolver import (DeadLinkError, ResolveError, fast_resolve,
                       filename_from_fragment, valid_url)
from .scrape_proc import SubprocessScraper
from .scraper import ScrapeError, fast_scrape, game_title_from_links, is_fitgirl_url

STATIC_DIR = Path(__file__).resolve().parent / "web_static"
ENV_FILE = Path(__file__).resolve().parent.parent / ".env"

MAX_BODY = 256 * 1024
MAX_LINKS = 400
MAX_ACTIVE_JOBS = 3          # queued/running jobs at once, across all users
MAX_STORED_JOBS = 30
JOB_TTL_S = 6 * 3600
LINK_SPLIT_RE = re.compile(r"\s+")


def _env_int(name: str, default: int) -> int:
    try:
        return max(1, int(os.environ.get(name, default)))
    except ValueError:
        return default


# Browserless resolves are ~0.3s HTTP calls; a few at once is plenty and stays
# polite to the host. Browser fallbacks are capped separately: each is a Firefox.
FAST_PARALLEL = _env_int("FITFAST_WEB_FAST_PARALLEL", 4)
PARALLEL = _env_int("FITFAST_WEB_PARALLEL", 2)
_browser_slots = threading.BoundedSemaphore(PARALLEL)
_pool = ThreadPoolExecutor(max_workers=FAST_PARALLEL, thread_name_prefix="resolve")


# ── jobs ──────────────────────────────────────────────────────────────────────

class Item:
    __slots__ = ("source", "filename", "status", "url", "error", "resolved_at")

    def __init__(self, source: str) -> None:
        self.source = source
        self.filename = filename_from_fragment(source) or ""
        self.status = "queued"         # queued | resolving | done | error | cancelled
        self.url: str | None = None
        self.error: str | None = None
        self.resolved_at: float | None = None

    def as_dict(self) -> dict:
        return {"source": self.source, "filename": self.filename,
                "status": self.status, "url": self.url, "error": self.error,
                "resolved_at": self.resolved_at}


class Job:
    def __init__(self, text: str) -> None:
        self.id = secrets.token_urlsafe(12)
        self.text = text
        self.created = time.time()
        self.finished: float | None = None
        self.phase = "queued"          # queued | scraping | resolving | done | failed | cancelled
        self.title: str | None = None
        self.error: str | None = None
        self.items: list[Item] = []
        self.cancelled = False
        self.lock = threading.Lock()
        self._active: set = set()      # in-flight scraper/resolver handles
        self._pending = 0

    # Handles are registered so cancel() can kill their browser processes.
    def _track(self, handle) -> bool:
        with self.lock:
            if self.cancelled:
                return False
            self._active.add(handle)
            return True

    def _untrack(self, handle) -> None:
        with self.lock:
            self._active.discard(handle)

    def cancel(self) -> None:
        with self.lock:
            self.cancelled = True
            handles = list(self._active)
            for it in self.items:
                if it.status in ("queued", "resolving"):
                    it.status = "cancelled"
            if self.phase not in ("done", "failed"):
                self.phase = "cancelled"
                self.finished = time.time()
        for h in handles:
            try:
                h.close()
            except Exception:
                pass

    @property
    def active(self) -> bool:
        return self.phase in ("queued", "scraping", "resolving")

    def as_dict(self) -> dict:
        with self.lock:
            items = [it.as_dict() for it in self.items]
            counts = {s: 0 for s in ("queued", "resolving", "done", "error", "cancelled")}
            for it in self.items:
                counts[it.status] += 1
            return {"id": self.id, "phase": self.phase, "title": self.title,
                    "error": self.error, "created": self.created,
                    "finished": self.finished, "counts": counts, "items": items}


class JobStore:
    def __init__(self) -> None:
        self._jobs: dict[str, Job] = {}
        self._lock = threading.Lock()

    def _purge(self) -> None:
        now = time.time()
        for jid, job in list(self._jobs.items()):
            if not job.active and now - (job.finished or job.created) > JOB_TTL_S:
                del self._jobs[jid]
        # Still too many: drop the oldest finished ones.
        done = sorted((j for j in self._jobs.values() if not j.active),
                      key=lambda j: j.created)
        while len(self._jobs) > MAX_STORED_JOBS and done:
            del self._jobs[done.pop(0).id]

    def add(self, job: Job) -> None:
        with self._lock:
            self._purge()
            if sum(1 for j in self._jobs.values() if j.active) >= MAX_ACTIVE_JOBS:
                raise BusyError("server is busy with other requests — try again in a few minutes")
            self._jobs[job.id] = job

    def get(self, jid: str) -> Job | None:
        with self._lock:
            return self._jobs.get(jid)


class BusyError(Exception):
    pass


class InputError(Exception):
    pass


JOBS = JobStore()


def parse_input(text: str) -> tuple[str | None, list[str]]:
    """Return (fitgirl_page_url, []) or (None, fuckingfast_links)."""
    tokens = [t for t in LINK_SPLIT_RE.split(text.strip()) if t]
    if not tokens:
        raise InputError("paste a fitgirl-repacks.site page URL or fuckingfast.co links")
    fitgirl = [t for t in tokens if is_fitgirl_url(t)]
    if fitgirl:
        if len(tokens) > 1:
            raise InputError("paste one FitGirl page at a time (or only fuckingfast.co links)")
        return fitgirl[0], []
    links, seen = [], set()
    for t in tokens:
        if not valid_url(t):
            raise InputError(f"not a fitgirl-repacks.site or fuckingfast.co link: {t[:80]}")
        key = t.split("#", 1)[0]
        if key not in seen:
            seen.add(key)
            links.append(t)
    if len(links) > MAX_LINKS:
        raise InputError(f"too many links ({len(links)}); max is {MAX_LINKS}")
    return None, links


def _scrape(job: Job, page_url: str) -> list[str] | None:
    try:
        result = fast_scrape(page_url)
    except ScrapeError as e:
        get_logger().info("web: job %s fast scrape failed (%s); using the browser", job.id, e)
        scraper = SubprocessScraper()
        if not job._track(scraper):
            return None
        try:
            with _browser_slots:
                if job.cancelled:
                    return None
                result = scraper.scrape(page_url, fast=False)
        finally:
            job._untrack(scraper)
    with job.lock:
        job.title = result.title
    return result.links


def _browser_resolve(job: Job, url: str) -> tuple[str, str] | None:
    """Slow fallback: a Camoufox resolve, capped by the browser semaphore."""
    with _browser_slots:
        resolver = SubprocessResolver()
        if job.cancelled or not job._track(resolver):
            return None
        try:
            return resolver.resolve(url, retries=1, fast=False)
        finally:
            job._untrack(resolver)


def _resolve_item(job: Job, item: Item) -> None:
    log = get_logger()
    try:
        with job.lock:
            if job.cancelled:
                return
            item.status = "resolving"
            item.error = None
        try:
            try:
                res = fast_resolve(item.source)
            except DeadLinkError:
                raise
            except ResolveError as e:
                log.info("web: job %s fast resolve failed (%s); using the browser", job.id, e)
                res = _browser_resolve(job, item.source)
        except Exception as e:
            if job.cancelled:
                return
            with job.lock:
                if item.status != "cancelled":
                    item.status = "error"
                    item.error = str(e)[:300] or "resolve failed"
            log.warning("web: resolve failed for job %s: %s", job.id, e)
            return
        if res is None:
            return
        signed, filename = res
        with job.lock:
            if item.status == "cancelled":
                return
            item.url = signed
            item.filename = filename or item.filename
            item.status = "done"
            item.resolved_at = time.time()
    finally:
        _item_finished(job)


def _item_finished(job: Job) -> None:
    with job.lock:
        job._pending -= 1
        if job._pending > 0 or job.phase != "resolving":
            return
        job.phase = "done"
        job.finished = time.time()
        ok = sum(1 for it in job.items if it.status == "done")
        total = len(job.items)
    get_logger().info("web: job %s finished: %d/%d resolved", job.id, ok, total)


def _submit(job: Job, items: list[Item]) -> None:
    with job.lock:
        job._pending += len(items)
        job.phase = "resolving"
        job.finished = None
    for it in items:
        _pool.submit(_resolve_item, job, it)


def run_job(job: Job) -> None:
    log = get_logger()
    try:
        page_url, links = parse_input(job.text)
        if page_url:
            with job.lock:
                job.phase = "scraping"
            log.info("web: job %s scraping a FitGirl page", job.id)
            links = _scrape(job, page_url)
            if links is None:
                return
            links = links[:MAX_LINKS]
        if job.cancelled:
            return
        with job.lock:
            job.items = [Item(l) for l in links]
            if not job.title:
                job.title = game_title_from_links(links)
        log.info("web: job %s resolving %d links", job.id, len(links))
        if not job.items:
            raise InputError("no links to resolve")
        _submit(job, list(job.items))
    except Exception as e:
        with job.lock:
            if job.phase != "cancelled":
                job.phase = "failed"
                job.error = str(e)[:400] or type(e).__name__
                job.finished = time.time()
        log.warning("web: job %s failed: %s", job.id, e)


def retry_failed(job: Job) -> int:
    with job.lock:
        if job.active or job.cancelled:
            return 0
        failed = [it for it in job.items if it.status == "error"]
        for it in failed:
            it.status = "queued"
            it.error = None
    if failed:
        _submit(job, failed)
    return len(failed)


# ── auth ──────────────────────────────────────────────────────────────────────

def _read_dotenv(path: Path) -> dict[str, str]:
    """Minimal KEY=VALUE parser: comments, blank lines, `export `, quotes."""
    out: dict[str, str] = {}
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError:
        return out
    for line in lines:
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, _, val = line.removeprefix("export ").partition("=")
        val = val.strip()
        if len(val) >= 2 and val[0] == val[-1] and val[0] in "'\"":
            val = val[1:-1]
        out[key.strip()] = val
    return out


def load_password() -> str:
    """The PASS value from the repo's .env (an exported PASS overrides it)."""
    pw = os.environ.get("PASS", "").strip() or _read_dotenv(ENV_FILE).get("PASS", "").strip()
    if not pw:
        raise SystemExit(f"!! no password: put PASS=<password> in {ENV_FILE}")
    return pw


# ── HTTP ──────────────────────────────────────────────────────────────────────

_JOB_PATH = re.compile(r"^/api/jobs/([A-Za-z0-9_-]{8,40})(/retry)?$")
_SECURITY_HEADERS = {
    "X-Content-Type-Options": "nosniff",
    "X-Frame-Options": "DENY",
    "Referrer-Policy": "no-referrer",
    "Cache-Control": "no-store",
    "Content-Security-Policy": (
        "default-src 'self'; style-src 'self' 'unsafe-inline'; "
        "script-src 'self' 'unsafe-inline'; img-src 'self' data:; "
        "connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"),
}


class Handler(BaseHTTPRequestHandler):
    server_version = f"FitFast/{APP_VERSION}"
    sys_version = ""
    password: str = ""

    # Quiet the default per-request stderr line; we log what matters ourselves.
    def log_message(self, fmt, *args):  # noqa: D401
        pass

    def _send(self, status: int, body: bytes, ctype: str, extra: dict | None = None) -> None:
        self.send_response(status)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        for k, v in {**_SECURITY_HEADERS, **(extra or {})}.items():
            self.send_header(k, v)
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)

    def _json(self, status: int, data) -> None:
        self._send(status, json.dumps(data).encode(), "application/json; charset=utf-8")

    def _authorized(self) -> bool:
        header = self.headers.get("Authorization", "")
        if header.startswith("Basic "):
            try:
                _user, _, pw = base64.b64decode(header[6:]).decode("utf-8").partition(":")
            except Exception:
                pw = ""
            if hmac.compare_digest(pw.encode(), self.password.encode()):
                return True
            time.sleep(1.0)  # slow down guessing
            get_logger().warning("web: bad password from %s", self.client_address[0])
        self._send(HTTPStatus.UNAUTHORIZED, b"auth required", "text/plain",
                   {"WWW-Authenticate": 'Basic realm="FitFast", charset="UTF-8"'})
        return False

    def do_HEAD(self):
        self.do_GET()

    def do_GET(self):
        if not self._authorized():
            return
        path = self.path.split("?", 1)[0]
        if path in ("/", "/index.html"):
            body = (STATIC_DIR / "index.html").read_bytes()
            return self._send(HTTPStatus.OK, body, "text/html; charset=utf-8")
        if path == "/api/status":
            return self._json(HTTPStatus.OK, {
                "version": APP_VERSION, "browser_ready": browser_ready(),
                "warning": browser_staleness_warning(), "parallel": PARALLEL,
                "max_links": MAX_LINKS})
        m = _JOB_PATH.match(path)
        if m and not m.group(2):
            job = JOBS.get(m.group(1))
            if job is None:
                return self._json(HTTPStatus.NOT_FOUND, {"error": "job not found (expired?)"})
            return self._json(HTTPStatus.OK, job.as_dict())
        self._json(HTTPStatus.NOT_FOUND, {"error": "not found"})

    def _read_json(self) -> dict | None:
        try:
            length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            length = -1
        if length < 0 or length > MAX_BODY:
            self._json(HTTPStatus.REQUEST_ENTITY_TOO_LARGE, {"error": "request too large"})
            return None
        try:
            data = json.loads(self.rfile.read(length) or b"{}")
            if not isinstance(data, dict):
                raise ValueError
            return data
        except ValueError:
            self._json(HTTPStatus.BAD_REQUEST, {"error": "invalid JSON"})
            return None

    def do_POST(self):
        if not self._authorized():
            return
        # Basic CSRF guard: browsers always send this on fetch() JSON posts,
        # and a cross-site form cannot set it.
        if not self.headers.get("Content-Type", "").startswith("application/json"):
            return self._json(HTTPStatus.UNSUPPORTED_MEDIA_TYPE, {"error": "JSON only"})
        path = self.path.split("?", 1)[0]
        if path == "/api/jobs":
            data = self._read_json()
            if data is None:
                return
            text = str(data.get("text", ""))
            try:
                parse_input(text)  # fail fast on obviously bad input
                job = Job(text)
                JOBS.add(job)
            except InputError as e:
                return self._json(HTTPStatus.BAD_REQUEST, {"error": str(e)})
            except BusyError as e:
                return self._json(HTTPStatus.TOO_MANY_REQUESTS, {"error": str(e)})
            get_logger().info("web: job %s created by %s", job.id, self.client_address[0])
            threading.Thread(target=run_job, args=(job,), daemon=True,
                             name=f"job-{job.id}").start()
            return self._json(HTTPStatus.CREATED, {"id": job.id})
        m = _JOB_PATH.match(path)
        if m and m.group(2):
            job = JOBS.get(m.group(1))
            if job is None:
                return self._json(HTTPStatus.NOT_FOUND, {"error": "job not found"})
            return self._json(HTTPStatus.OK, {"retried": retry_failed(job)})
        self._json(HTTPStatus.NOT_FOUND, {"error": "not found"})

    def do_DELETE(self):
        if not self._authorized():
            return
        m = _JOB_PATH.match(self.path.split("?", 1)[0])
        job = JOBS.get(m.group(1)) if m and not m.group(2) else None
        if job is None:
            return self._json(HTTPStatus.NOT_FOUND, {"error": "job not found"})
        job.cancel()
        self._json(HTTPStatus.OK, {"cancelled": True})


def main(argv: list[str] | None = None) -> int:
    import argparse
    p = argparse.ArgumentParser(description="FitFast web UI (link resolver only)")
    p.add_argument("--host", default=os.environ.get("FITFAST_WEB_HOST", "0.0.0.0"))
    p.add_argument("--port", type=int, default=int(os.environ.get("FITFAST_WEB_PORT", "8787")))
    args = p.parse_args(argv)

    setup_logging()
    log = get_logger()
    Handler.password = load_password()

    if not browser_ready():
        print("!! stealth browser not installed — run: camoufox sync && camoufox fetch",
              file=sys.stderr)
    elif (stale := browser_staleness_warning()):
        print(f"!! {stale}", file=sys.stderr)

    httpd = ThreadingHTTPServer((args.host, args.port), Handler)
    httpd.daemon_threads = True
    print(f"FitFast web UI on http://{args.host}:{args.port}/  "
          f"(browsers in parallel: {PARALLEL})")
    print("  login: any username + PASS from .env")
    log.info("web: listening on %s:%d", args.host, args.port)
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        httpd.server_close()
        _pool.shutdown(wait=False, cancel_futures=True)
    return 0
