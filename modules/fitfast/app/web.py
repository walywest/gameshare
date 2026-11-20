"""FitFast web API: a link grabber and server-side downloads past Cloudflare.

Paste a FitGirl page, or text holding links on any host in hosts.py. A job
("check links") resolves each link to its signed direct URL, with its name and,
where the host allows, its exact size, which is the part that needs to get past
Cloudflare. From there the links can be copied for another downloader, or
queued as a package for the server's own downloader (manager.py).

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

from .bootstrap import browser_ready, browser_staleness_warning, install_in_subprocess
from .config import APP_VERSION
from .log import get_logger, setup_logging
from .resolve_proc import SubprocessResolver
from .hosts import HOSTS, host_for, probe
from .resolver import (DeadLinkError, ResolveError, common_folder_name, fast_resolve,
                       filename_from_fragment, valid_url)
from urllib.parse import urlsplit
from .scrape_proc import SubprocessScraper
from .scraper import ScrapeError, fast_scrape, game_title_from_links, is_fitgirl_url

STATIC_DIR = Path(__file__).resolve().parent / "web_static"
MODULE_DIR = Path(__file__).resolve().parent.parent
# The module's own .env first, then the homelab repo's, which holds GIT_PASS too.
ENV_FILES = (MODULE_DIR / ".env", MODULE_DIR.parent.parent / ".env")

MAX_BODY = 256 * 1024
MAX_LINKS = 400
MAX_ACTIVE_JOBS = 3          # queued/running jobs at once, across all users
MAX_STORED_JOBS = 30
JOB_TTL_S = 6 * 3600
LINK_SPLIT_RE = re.compile(r"\s+")
URL_RE = re.compile(r"https?://\S+", re.IGNORECASE)
HOST_LABELS = ", ".join(h.label for h in HOSTS)


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
    __slots__ = ("source", "filename", "status", "url", "error", "resolved_at", "host", "size")

    def __init__(self, source: str) -> None:
        self.source = source
        self.filename = filename_from_fragment(source) or ""
        self.status = "queued"         # queued | resolving | done | error | cancelled
        self.url: str | None = None
        self.error: str | None = None
        self.resolved_at: float | None = None
        h = host_for(source)
        self.host = h.label if h else ""
        self.size: int | None = None   # exact, from a 1-byte probe, where the host allows

    def as_dict(self) -> dict:
        return {"source": self.source, "filename": self.filename,
                "status": self.status, "url": self.url, "error": self.error,
                "resolved_at": self.resolved_at, "host": self.host, "size": self.size}


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
        self.ignored: list[str] = []   # hosts of pasted links no plugin handles
        # Set when the links should go straight to the download queue once
        # checked: {"extract": bool, "delete_archives": bool}.
        self.autostart: dict | None = None
        self.queued: str | None = None  # the download it was queued as

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
                    "finished": self.finished, "counts": counts, "items": items,
                    "ignored": self.ignored, "autostart": self.autostart is not None,
                    "queued": self.queued}


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


def parse_input(text: str) -> tuple[str | None, list[str], list[str]]:
    """Find what to download in pasted text.

    Returns (fitgirl_page, [], []) or (None, links, ignored_hosts). Text around
    the links is fine, so a whole post or a page's mirror list can be pasted:
    links on hosts without a plugin are left out and their hosts reported.
    """
    urls = [u.rstrip(").,;:'\"<>") for u in URL_RE.findall(text or "")]
    if not urls:
        raise InputError(f"paste a FitGirl page, or links from {HOST_LABELS}")
    fitgirl = list(dict.fromkeys(u for u in urls if is_fitgirl_url(u)))
    if fitgirl:
        if len(fitgirl) > 1 or any(valid_url(u) for u in urls):
            raise InputError("paste one FitGirl page at a time, or only file links")
        return fitgirl[0], [], []
    links, seen, ignored = [], set(), []
    for u in urls:
        if not valid_url(u):
            host = urlsplit(u).netloc.lower()
            if host and host not in ignored:
                ignored.append(host)
            continue
        key = u.split("#", 1)[0].rstrip("/")
        if key not in seen:
            seen.add(key)
            links.append(u)
    if not links:
        found = f" (found {', '.join(ignored[:6])})" if ignored else ""
        raise InputError(f"no links from a supported host{found}. Supported: FitGirl pages, {HOST_LABELS}")
    if len(links) > MAX_LINKS:
        raise InputError(f"too many links ({len(links)}); max is {MAX_LINKS}")
    return None, links, ignored


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
        host = host_for(item.source)
        size = served_name = None
        if host is not None and host.probe:
            size, served_name = probe(signed)
        with job.lock:
            if item.status == "cancelled":
                return
            item.url = signed
            item.filename = filename or served_name or item.filename
            item.size = size
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
        if not job.title:  # links that carry no name until resolved
            names = [it.filename for it in job.items if it.filename]
            job.title = common_folder_name(names) if names else None
        auto = job.autostart is not None and job.queued is None
    get_logger().info("web: job %s finished: %d/%d resolved", job.id, ok, total)
    if auto:
        _autostart(job)


def _autostart(job: Job) -> None:
    """Queue what a "download now" job resolved. Links that failed to resolve
    stay on the job's card, where they can be retried."""
    with job.lock:
        items = [it for it in job.items if it.status == "done"]
        opts, title = job.autostart or {}, job.title
    if not items or MANAGER is None:
        with job.lock:
            job.error = "no link resolved, so nothing was queued"
        return
    try:
        game = MANAGER.add(None, [it.source for it in items], bool(opts.get("extract", True)),
                           bool(opts.get("delete_archives", False)), title,
                           names={it.source: it.filename for it in items})
    except ValueError as e:
        with job.lock:
            job.error = str(e)
        return
    with job.lock:
        job.queued = game.id
    get_logger().info("web: job %s queued as download %s", job.id, game.id)


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
        page_url, links, ignored = parse_input(job.text)
        with job.lock:
            job.ignored = ignored
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
    """The PASS value from .env (an exported PASS overrides it)."""
    pw = os.environ.get("PASS", "").strip()
    for f in ENV_FILES:
        pw = pw or _read_dotenv(f).get("PASS", "").strip()
    if not pw:
        raise SystemExit(f"!! no password: put PASS=<password> in {ENV_FILES[0]} or {ENV_FILES[1]}")
    return pw


# ── server-side downloads ─────────────────────────────────────────────────────

MANAGER = None          # app.manager.Manager, when started with --root
BROWSER = {"installing": False, "status": "", "error": None}
_DL_PATH = re.compile(r"^/api/downloads/([A-Za-z0-9_-]{6,40})(?:/(pause|resume|retry))?$")


def _install_browser() -> None:
    """Fetch the Camoufox browser in the background on first start, so a fresh
    server needs no manual step. Browserless resolving works meanwhile."""
    BROWSER["installing"] = True
    try:
        install_in_subprocess(lambda m: BROWSER.__setitem__("status", m))
        BROWSER["status"] = "installed"
    except Exception as e:
        BROWSER["error"] = str(e)[:300]
        get_logger().warning("web: browser install failed: %s", e)
    finally:
        BROWSER["installing"] = False


def _flag(data: dict, key: str, default: bool) -> bool:
    v = data.get(key, default)
    return v if isinstance(v, bool) else default


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
    open_access = False     # bound to loopback behind the homelab server, which guards access

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
        if self.open_access:
            return True
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
                "browser": BROWSER, "downloads": MANAGER is not None,
                "warning": browser_staleness_warning(), "parallel": PARALLEL,
                "max_links": MAX_LINKS, "hosts": ["FitGirl pages"] + [h.label for h in HOSTS]})
        if path == "/api/downloads":
            if MANAGER is None:
                return self._json(HTTPStatus.NOT_FOUND, {"error": "server-side downloads are off (no --root)"})
            return self._json(HTTPStatus.OK, MANAGER.snapshot())
        if path == "/api/downloads/busy":
            return self._json(HTTPStatus.OK, {"paths": MANAGER.busy_paths() if MANAGER else []})
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
                job = self._start_job(Job(text))
            except InputError as e:
                return self._json(HTTPStatus.BAD_REQUEST, {"error": str(e)})
            except BusyError as e:
                return self._json(HTTPStatus.TOO_MANY_REQUESTS, {"error": str(e)})
            return self._json(HTTPStatus.CREATED, {"id": job.id})
        if path.startswith("/api/downloads"):
            return self._post_download(path)
        m = _JOB_PATH.match(path)
        if m and m.group(2):
            job = JOBS.get(m.group(1))
            if job is None:
                return self._json(HTTPStatus.NOT_FOUND, {"error": "job not found"})
            return self._json(HTTPStatus.OK, {"retried": retry_failed(job)})
        self._json(HTTPStatus.NOT_FOUND, {"error": "not found"})

    def _start_job(self, job: Job) -> Job:
        JOBS.add(job)  # raises BusyError
        get_logger().info("web: job %s created by %s", job.id, self.client_address[0])
        threading.Thread(target=run_job, args=(job,), daemon=True, name=f"job-{job.id}").start()
        return job

    def _post_download(self, path: str):
        if MANAGER is None:
            return self._json(HTTPStatus.NOT_FOUND, {"error": "server-side downloads are off (no --root)"})
        data = self._read_json()
        if data is None:
            return
        if path == "/api/downloads":
            extract = _flag(data, "extract", True)
            delete = _flag(data, "delete_archives", False)
            title, names = None, None
            if data.get("job"):
                job = JOBS.get(str(data["job"]))
                if job is None:
                    return self._json(HTTPStatus.NOT_FOUND, {"error": "link job not found (expired?)"})
                # The link grabber sends the links ticked and the package name typed.
                wanted = data.get("sources")
                wanted = set(map(str, wanted)) if isinstance(wanted, list) else None
                with job.lock:
                    items = [it for it in job.items if wanted is None or it.source in wanted]
                    page, links = None, [it.source for it in items]
                    names = {it.source: it.filename for it in items if it.filename}
                    title = str(data.get("title") or "").strip()[:200] or job.title
                if not links:
                    return self._json(HTTPStatus.BAD_REQUEST, {"error": "no links picked"})
            else:
                text = str(data.get("text", ""))
                try:
                    page, links, _ = parse_input(text)
                except InputError as e:
                    return self._json(HTTPStatus.BAD_REQUEST, {"error": str(e)})
                if not page and any(not filename_from_fragment(l) for l in links):
                    # Names that only come from resolving (anything but FitGirl's
                    # links): check them first, then queue. The package is named
                    # from them, which is the folder it lands in.
                    job = Job(text)
                    job.autostart = {"extract": extract, "delete_archives": delete}
                    try:
                        self._start_job(job)
                    except BusyError as e:
                        return self._json(HTTPStatus.TOO_MANY_REQUESTS, {"error": str(e)})
                    return self._json(HTTPStatus.ACCEPTED, {"job": job.id})
            try:
                game = MANAGER.add(page, links, extract, delete, title, names=names)
            except ValueError as e:
                return self._json(HTTPStatus.TOO_MANY_REQUESTS, {"error": str(e)})
            get_logger().info("web: download %s queued by %s", game.id, self.client_address[0])
            return self._json(HTTPStatus.CREATED, {"id": game.id})
        if path == "/api/downloads/limit":
            try:
                mbps = float(data.get("mbps", 0))
            except (TypeError, ValueError):
                return self._json(HTTPStatus.BAD_REQUEST, {"error": "mbps must be a number"})
            MANAGER.set_limit(mbps)
            return self._json(HTTPStatus.OK, {"limit_mbps": MANAGER.limit_mbps})
        m = _DL_PATH.match(path)
        if not m or not m.group(2):
            return self._json(HTTPStatus.NOT_FOUND, {"error": "not found"})
        gid, action = m.group(1), m.group(2)
        if MANAGER.get(gid) is None:
            return self._json(HTTPStatus.NOT_FOUND, {"error": "no such download"})
        ok = {"pause": MANAGER.pause, "resume": MANAGER.resume, "retry": MANAGER.retry}[action](gid)
        if not ok:
            return self._json(HTTPStatus.CONFLICT, {"error": f"can't {action} it now"})
        return self._json(HTTPStatus.OK, {"ok": True})

    def do_DELETE(self):
        if not self._authorized():
            return
        path = self.path.split("?", 1)[0]
        m = _DL_PATH.match(path)
        if m and not m.group(2):
            if MANAGER is None or not MANAGER.remove(m.group(1)):
                return self._json(HTTPStatus.NOT_FOUND, {"error": "no such download"})
            return self._json(HTTPStatus.OK, {"removed": True})
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
    p.add_argument("--root", default=os.environ.get("FITFAST_ROOT", ""),
                   help="download games on the server, into <root>/FitFast")
    args = p.parse_args(argv)

    setup_logging()
    log = get_logger()
    # Loopback only means the homelab server proxies to it and guards access, so
    # no password is needed. Anywhere else, PASS is required as before.
    if args.host in ("127.0.0.1", "localhost", "::1"):
        Handler.open_access = True
    else:
        Handler.password = load_password()

    if not browser_ready():
        print("!! stealth browser not installed; fetching it in the background (~1 GB)",
              file=sys.stderr)
        threading.Thread(target=_install_browser, daemon=True, name="browser-install").start()
    elif (stale := browser_staleness_warning()):
        print(f"!! {stale}", file=sys.stderr)

    global MANAGER
    if args.root:
        from .manager import Manager
        MANAGER = Manager(args.root)
        MANAGER.start()
        print(f"server-side downloads go to {Path(args.root) / 'FitFast'}")

    # The homelab server stops services with SIGTERM: stop aria2 cleanly so its
    # control files are saved, and save the queue.
    import signal

    def _term(*_):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, _term)

    httpd = ThreadingHTTPServer((args.host, args.port), Handler)
    httpd.daemon_threads = True
    print(f"FitFast web UI on http://{args.host}:{args.port}/  "
          f"(browsers in parallel: {PARALLEL})")
    print("  open access on loopback" if Handler.open_access else "  login: any username + PASS from .env")
    log.info("web: listening on %s:%d", args.host, args.port)
    try:
        httpd.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        httpd.server_close()
        _pool.shutdown(wait=False, cancel_futures=True)
        if MANAGER is not None:
            MANAGER.shutdown()
    return 0
