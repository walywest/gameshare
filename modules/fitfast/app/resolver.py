"""Resolve fuckingfast.co URLs to signed direct download URLs via Camoufox.

The flow (probed 2026-07-12; challenge step re-diagnosed 2026-08-30):
  1. Load the page and wait for Cloudflare to be satisfied. This used to be a
     passive JS challenge Camoufox cleared on its own, so a fixed 2.5s wait was
     enough. It is now an interactive Turnstile widget: a live trace showed
     every resolve failing with the challenge issued and exercised
     (challenges.cloudflare.com .../turnstile/... and .../cdn-cgi/challenge-
     platform/... , all 200) but never satisfied, so the download endpoint was
     never reached at all — zero /go requests. We now wait for the widget's
     token instead of guessing at a duration.
  2. Block ad domains + neuter window.open so popups don't fire.
  3. Click the DOWNLOAD button twice. The second click triggers an HTMX
     POST to /f/<id>/go whose response header `hx-redirect` contains the
     pre-signed https://dl.fuckingfast.co/dl/<token> URL.
  4. That signed URL is Range-capable and needs no cookies to download.
"""
from __future__ import annotations
import re
import threading
import time
from datetime import datetime
from pathlib import Path

from camoufox.sync_api import Camoufox

LINK_RE = re.compile(r"^https?://fuckingfast\.co/[a-z0-9]+", re.IGNORECASE)
_INVALID_CHARS = re.compile(r'[<>:"/\\|?*\x00-\x1f]')
FIREFOX_UA = (
    "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:135.0) "
    "Gecko/20100101 Firefox/135.0"
)


class ResolveError(Exception):
    pass


class DeadLinkError(ResolveError):
    """The file is gone (404) — no point trying the browser either."""


# ── Fast path: no browser ────────────────────────────────────────────────────
# The DOWNLOAD button is only an HTMX POST to /f/<id>/go, and the signed URL
# comes back in its `hx-redirect` header. Cloudflare gates that endpoint on the
# TLS fingerprint, not on Turnstile, so a Chrome-impersonating client
# (curl_cffi) gets the answer directly in ~0.3s instead of the 10-30s a browser
# launch + Turnstile wait costs. Measured live 2026-11-04. When the site does
# demand a captcha ("captcha verification failed"), or anything else is off,
# callers fall back to the Camoufox Resolver below.
FF_HOST = "https://fuckingfast.co"
_FF_ID_RE = re.compile(r"^https?://fuckingfast\.co/([A-Za-z0-9]+)", re.IGNORECASE)
_tls = threading.local()


def http_session():
    """Per-thread curl_cffi session (sessions are not thread-safe)."""
    s = getattr(_tls, "session", None)
    if s is None:
        from curl_cffi import requests as cf_requests
        s = cf_requests.Session(impersonate="chrome")
        _tls.session = s
    return s


def fast_resolve(url: str, timeout: float = 20.0) -> tuple[str, str]:
    """Return (signed_url, filename) without a browser.

    Raises DeadLinkError for a removed file, ResolveError for anything the
    browser path might still get past.
    """
    m = _FF_ID_RE.match(url.strip())
    if not m:
        raise ResolveError("not a fuckingfast.co link")
    file_id = m.group(1)
    page = f"{FF_HOST}/{file_id}"
    try:
        r = http_session().post(
            f"{FF_HOST}/f/{file_id}/go", data=b"", allow_redirects=False,
            timeout=timeout, headers={
                "Accept": "*/*", "HX-Request": "true", "HX-Current-URL": page,
                "Origin": FF_HOST, "Referer": page, "Sec-Fetch-Dest": "empty",
                "Sec-Fetch-Mode": "cors", "Sec-Fetch-Site": "same-origin"})
    except Exception as e:
        raise ResolveError(f"fast resolve: {type(e).__name__}: {e}") from e
    hx = r.headers.get("hx-redirect")
    if hx and "/dl/" in hx:
        return hx.strip(), Resolver._pick_filename(url, "")
    body = (r.text or "")[:400].lower()
    if r.status_code == 404 or "not found" in body:
        raise DeadLinkError("file not found on fuckingfast.co (removed?)")
    if "captcha" in body:
        raise ResolveError("fast resolve: captcha required")
    raise ResolveError(f"fast resolve: HTTP {r.status_code}, no hx-redirect")


def valid_url(line: str) -> bool:
    return bool(LINK_RE.match(line.strip()))


def sanitize_filename(name: str) -> str:
    cleaned = _INVALID_CHARS.sub("_", name).strip()
    return cleaned or "download.bin"


def filename_from_fragment(url: str) -> str | None:
    if "#" in url:
        frag = url.split("#", 1)[1]
        if frag:
            return sanitize_filename(frag)
    return None


def common_folder_name(filenames: list[str], fallback: str | None = None) -> str:
    """Compute a subfolder name from a list of filenames.

    Strategy: longest common prefix, strip trailing .part\\d* + punctuation,
    sanitize for Windows, fall back to a timestamped name if the result
    is unusable.
    """
    if not filenames:
        return fallback or f"fitgirl_download_{datetime.now():%Y%m%d_%H%M%S}"
    if len(filenames) == 1:
        base = re.sub(r"\.part\d+\.(rar|r\d+|zip|7z)$", "", filenames[0], flags=re.IGNORECASE)
        base = re.sub(r"\.(rar|zip|7z)$", "", base, flags=re.IGNORECASE)
        base = base.strip("._- ")
        if len(base) < 3:
            base = fallback or f"fitgirl_download_{datetime.now():%Y%m%d_%H%M%S}"
        return sanitize_filename(base)

    prefix = filenames[0]
    for name in filenames[1:]:
        while prefix and not name.startswith(prefix):
            prefix = prefix[:-1]
        if not prefix:
            break

    prefix = re.sub(r"\.part\d*$", "", prefix, flags=re.IGNORECASE)
    prefix = prefix.rstrip("._- ")
    if len(prefix) < 3:
        prefix = fallback or f"fitgirl_download_{datetime.now():%Y%m%d_%H%M%S}"
    return sanitize_filename(prefix)


def unique_folder(base_dir: str | Path, name: str) -> Path:
    """Return the destination subfolder, reusing it if it already exists.

    A prior run may have left partial (or complete) files here — that's the
    point of resuming. We never rename to "name (2)" just because the folder
    is non-empty; per-file completeness is checked later, at download time.
    """
    base = Path(base_dir)
    candidate = base / name
    candidate.mkdir(parents=True, exist_ok=True)
    return candidate


_AD_KEYWORDS = ("c8yq5.sbs", "doubleclick.net", "googlesyndication",
                "googletagmanager", ".sbs/?tag=", "adservice", "adnxs")


class Resolver:
    """Single persistent Camoufox browser reused across all resolves."""

    def __init__(self) -> None:
        self._cm = None
        self._browser = None
        self._lock = threading.Lock()
        self._closed = False

    def _ensure_browser(self) -> None:
        if self._browser is not None:
            return
        # humanize=True: that comment's premise (passive JS challenge, no
        # behavioural analysis) held when it was written and no longer does —
        # the site now serves an interactive Turnstile. Cursor simulation costs
        # us time on every resolve, which is why it was off; being unable to
        # resolve at all costs more.
        self._cm = Camoufox(headless=True, humanize=True, geoip=False)
        self._browser = self._cm.__enter__()

    def close(self) -> None:
        with self._lock:
            self._closed = True
            if self._cm is not None:
                try:
                    self._cm.__exit__(None, None, None)
                except Exception:
                    pass
            self._cm = None
            self._browser = None

    def _hard_reset(self) -> None:
        try:
            if self._cm is not None:
                self._cm.__exit__(None, None, None)
        except Exception:
            pass
        self._cm = None
        self._browser = None

    def resolve(self, url: str, retries: int = 1) -> tuple[str, str]:
        """Return (signed_url, filename). Raises ResolveError on failure."""
        with self._lock:
            if self._closed:
                raise ResolveError("resolver has been closed")
            last_err: Exception | None = None
            for attempt in range(retries + 1):
                try:
                    return self._resolve_once(url)
                except Exception as e:
                    last_err = e
                    self._hard_reset()
                    time.sleep(2.0)
            raise ResolveError(f"resolve failed after retries: {last_err}") from last_err

    def _resolve_once(self, url: str) -> tuple[str, str]:
        self._ensure_browser()
        assert self._browser is not None
        page = self._browser.new_page()

        def _route(route):
            u = route.request.url
            if any(k in u for k in _AD_KEYWORDS):
                route.abort()
            else:
                route.continue_()

        page.route("**/*", _route)

        captured: dict = {"signed_url": None}
        # Id-redacted trace of what the page actually did, so a failure can say
        # why instead of only "nothing captured". Never holds a signed URL.
        trace: list[str] = []

        def _shape(u: str) -> str:
            """host + path with the file id blanked — safe to log."""
            try:
                without_scheme = u.split("://", 1)[-1]
                hostpath = without_scheme.split("?", 1)[0].split("#", 1)[0]
                # Blank only id-shaped segments (long, and containing a digit
                # or mixed case). Plain words like /go or /verify are the part
                # of the path worth seeing.
                host, slash, path = hostpath.partition("/")
                path = slash + path
                path = re.sub(
                    r"/(?=[A-Za-z0-9_-]{6,}(?:/|$))(?![a-z-]+(?:/|$))[A-Za-z0-9_-]+",
                    "/<id>", path)
                # Cloudflare's challenge tokens are colon- and dot-separated so
                # they survive per-segment redaction. Blank any long opaque run
                # too — this string can end up in the on-disk log. Host is left
                # alone: which host answered is the most useful part.
                path = re.sub(r"[A-Za-z0-9_.:+-]{24,}", "<tok>", path)
                return host + path
            except Exception:
                return "<unparsable>"

        def on_response(resp):
            try:
                # Match on the header, not the URL. hx-redirect *is* the
                # signal; keying on "/f/<id>/go" coupled us to a path we do not
                # control, so an endpoint change would silently capture nothing
                # while the header sat right there on the response.
                hx = (resp.headers.get("hx-redirect")
                      or resp.headers.get("HX-Redirect"))
                if hx:
                    captured["signed_url"] = hx
                    return
                if len(trace) < 12 and (
                        "/f/" in resp.url or resp.request.method != "GET"):
                    trace.append(
                        f"{resp.request.method} {_shape(resp.url)} -> {resp.status}")
            except Exception:
                pass

        page.on("response", on_response)

        try:
            page.goto(url, wait_until="domcontentloaded", timeout=60_000)
            challenge = self._await_challenge(page)
            page.wait_for_timeout(500)   # let the button's handler bind

            title = ""
            try:
                title = page.title() or ""
            except Exception:
                pass

            try:
                page.evaluate("window.open = function(){ return null; }")
            except Exception:
                pass

            page.click("a.link-button", timeout=30_000)
            page.wait_for_timeout(1200)

            page.click("a.link-button", timeout=30_000)

            deadline = time.time() + 20
            while captured["signed_url"] is None and time.time() < deadline:
                page.wait_for_timeout(200)

            if not captured["signed_url"]:
                detail = "; ".join(trace) if trace else "no candidate responses at all"
                raise ResolveError(
                    "no hx-redirect header on any response after 20s "
                    f"(challenge: {challenge}; saw: {detail})")

            filename = self._pick_filename(url, title)
            return captured["signed_url"], filename
        finally:
            try:
                page.close()
            except Exception:
                pass

    @staticmethod
    def _await_challenge(page, timeout_s: float = 30.0) -> str:
        """Block until Cloudflare's Turnstile hands out a token.

        Returns a short status for the error message: 'none' (no widget on the
        page — nothing to wait for), 'solved', or 'unsolved' (widget present,
        no token inside the window). Clicking before the token exists is the
        failure we are avoiding: the button is there and the click lands, but
        its handler will not fire the HTMX POST, so the resolve dies 20s later
        having never touched the download endpoint.
        """
        deadline = time.time() + timeout_s
        seen_widget = False
        while time.time() < deadline:
            try:
                token = page.evaluate(
                    "() => { const e = document.querySelector("
                    "'input[name=\"cf-turnstile-response\"]');"
                    " return e ? e.value : null; }")
            except Exception:
                token = None
            if token is not None:
                seen_widget = True
                if token:
                    return "solved"
            page.wait_for_timeout(250)
        return "unsolved" if seen_widget else "none"

    @staticmethod
    def _pick_filename(url: str, title: str) -> str:
        # The URL fragment is the *per-file* name (e.g. #Game.part03.rar) that
        # FitGirl links carry, so it wins whenever present. The page <title> is
        # only a fallback for bare links pasted without a fragment: FuckingFast
        # serves the SAME title for every part of a repack, so preferring it
        # named every part identically — files collided under one name and
        # matched none of the links that were pasted.
        frag = filename_from_fragment(url)
        if frag:
            return frag
        if title and title.strip() and title.strip().lower() not in ("fuckingfast", "fuckingfast.co"):
            return sanitize_filename(title.strip())
        m = re.search(r"/([a-z0-9]+)$", url.split("#", 1)[0])
        return f"{m.group(1) if m else 'download'}.bin"
