"""The file hosts FitFast downloads from, one plugin each.

A host turns a pasted link into ``(signed_url, filename)``: a direct URL that
aria2 can download and resume with no cookies, referer or browser fingerprint.
Every host here sits behind Cloudflare, which is what stops JDownloader. Each
one gets two ways past it:

- ``fast_resolve()``: plain HTTP through curl_cffi with Chrome's TLS
  fingerprint. Cloudflare's bot check keys on that fingerprint, so this
  answers in about a second when it works.
- ``browser()``: the steps to take in Camoufox (a stealth Firefox) when the
  fast path is refused. ``resolver.Resolver`` runs them in a killable
  subprocess and catches the signed URL in the ``hx-redirect`` response
  header, which is how both hosts' download buttons hand it out (they're HTMX).

To add a host: subclass Host, add an instance to HOSTS, and cover its parsing
in tests/test_hosts.py with canned pages. Tests never touch the network.
"""
from __future__ import annotations

import html
import re
from urllib.parse import unquote, urljoin, urlsplit

from .resolver import (DeadLinkError, ResolveError, filename_from_fragment,
                       http_session, sanitize_filename)

_HX = {"Accept": "*/*", "HX-Request": "true", "Sec-Fetch-Dest": "empty",
       "Sec-Fetch-Mode": "cors", "Sec-Fetch-Site": "same-origin"}


class Host:
    name = ""        # id used by the API and the page
    label = ""       # what people see
    pattern: re.Pattern
    # Whether a 1-byte Range request on the signed URL is safe, to learn the
    # exact size and the server's filename before downloading.
    probe = False
    # aria2 connections per file. More than one needs a host that answers
    # bounded Range requests correctly, and it changes how a partial file
    # looks on disk: see DownloadState's `split` handling.
    connections = 1
    referer = ""     # sent with the download; "" keeps aria2's default

    def match(self, url: str) -> bool:
        return bool(self.pattern.match(url.strip()))

    def fast_resolve(self, url: str) -> tuple[str, str]:
        raise NotImplementedError

    def browser(self, page, url: str, await_challenge) -> tuple[str, str]:
        """Drive a Camoufox page until its download request has gone out.

        Returns (page title, challenge status). The caller catches the signed
        URL from the response headers.
        """
        raise NotImplementedError

    def filename(self, url: str, title: str) -> str:
        if title and title.strip():
            return sanitize_filename(html.unescape(title.strip()))
        tail = urlsplit(url).path.rstrip("/").rsplit("/", 1)[-1]
        return f"{tail or 'download'}.bin"


# ── FuckingFast (FitGirl's host) ─────────────────────────────────────────────
class FuckingFast(Host):
    """The DOWNLOAD button is an HTMX POST to /f/<id>/go, and the signed URL
    comes back in its ``hx-redirect`` header. Cloudflare gates that endpoint on
    the TLS fingerprint, not on Turnstile, so a Chrome-impersonating client gets
    the answer directly in ~0.3s instead of the 10-30s a browser launch and
    Turnstile wait cost. Measured live 2026-11-04. When the site does demand a
    captcha, or anything else is off, the browser path takes over."""

    name, label = "fuckingfast", "FuckingFast"
    pattern = re.compile(r"^https?://fuckingfast\.co/([a-z0-9]+)", re.IGNORECASE)
    site = "https://fuckingfast.co"

    def fast_resolve(self, url: str) -> tuple[str, str]:
        m = self.pattern.match(url.strip())
        if not m:
            raise ResolveError("not a fuckingfast.co link")
        file_id = m.group(1)
        page = f"{self.site}/{file_id}"
        try:
            r = http_session().post(
                f"{self.site}/f/{file_id}/go", data=b"", allow_redirects=False, timeout=20,
                headers={**_HX, "HX-Current-URL": page, "Origin": self.site, "Referer": page})
        except Exception as e:
            raise ResolveError(f"fast resolve: {type(e).__name__}: {e}") from e
        hx = r.headers.get("hx-redirect")
        if hx and "/dl/" in hx:
            return hx.strip(), self.filename(url, "")
        body = (r.text or "")[:400].lower()
        if r.status_code == 404 or "not found" in body:
            raise DeadLinkError("file not found on fuckingfast.co (removed?)")
        if "captcha" in body:
            raise ResolveError("fast resolve: captcha required")
        raise ResolveError(f"fast resolve: HTTP {r.status_code}, no hx-redirect")

    def browser(self, page, url: str, await_challenge) -> tuple[str, str]:
        page.goto(url, wait_until="domcontentloaded", timeout=60_000)
        challenge = await_challenge(page)
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
        # The first click opens an ad; the second sends the request.
        page.click("a.link-button", timeout=30_000)
        page.wait_for_timeout(1200)
        page.click("a.link-button", timeout=30_000)
        return title, challenge

    def filename(self, url: str, title: str) -> str:
        # The URL fragment is the *per-file* name (e.g. #Game.part03.rar) that
        # FitGirl links carry, so it wins whenever present. The page <title> is
        # only a fallback for bare links pasted without a fragment: FuckingFast
        # serves the SAME title for every part of a repack, so preferring it
        # named every part identically: files collided under one name and
        # matched none of the links that were pasted.
        frag = filename_from_fragment(url)
        if frag:
            return frag
        if title and title.strip() and title.strip().lower() not in ("fuckingfast", "fuckingfast.co"):
            return sanitize_filename(title.strip())
        m = re.search(r"/([a-z0-9]+)$", url.split("#", 1)[0])
        return f"{m.group(1) if m else 'download'}.bin"


# ── BuzzHeavier (SteamRIP's host, among others) ──────────────────────────────
class BuzzHeavier(Host):
    """Probed live 2026-11-20 on https://bzzhr.to/ijvxifbx5sgh:

    1. A plain client gets Cloudflare's JS challenge (403, cf-mitigated). With
       Chrome's TLS fingerprint the page loads.
    2. An uploader can allow only visitors from its own site: the page then
       answers 303 to that site (SteamRIP's files send you to steamrip.com).
       Asking again with that site as the Referer gets the file page.
    3. The page's Download button is an HTMX GET to /<id>/download?t=<token>.
       It answers 204 with ``hx-redirect`` to a signed URL on ts.bzzhr.to.
    4. That URL serves Range requests (206) to any client, so aria2 resumes.
    A removed file is a plain 404 "Not Found".

    Downloads are capped at ~350 KB/s per connection. Measured with aria2 over
    12 s each: 1 connection 354 KB/s, 4 -> 1.4 MB/s, 8 -> 2.0 MB/s, 16 -> 2.0
    MB/s. Bounded Range requests are answered correctly, so 8 it is."""

    name, label = "buzzheavier", "BuzzHeavier"
    connections = 8
    referer = "https://buzzheavier.com/"
    # File ids are 12 characters; the length also keeps out /help, /proxy...
    pattern = re.compile(
        r"^https?://(?:www\.)?(?:buzzheavier\.com|bzzhr\.(?:to|co))/([a-z0-9]{10,16})/?(?:[?#].*)?$",
        re.IGNORECASE)
    probe = True
    _REDIRECTS = (301, 302, 303, 307, 308)

    def page(self, url: str) -> tuple[str, str]:
        """Fetch the file page. Returns (html, url it was served at)."""
        s, referer = http_session(), None
        url = url.strip().split("#", 1)[0]
        for _ in range(5):
            try:
                r = s.get(url, allow_redirects=False, timeout=20,
                          headers={"Referer": referer} if referer else {})
            except Exception as e:
                raise ResolveError(f"fast resolve: {type(e).__name__}: {e}") from e
            if r.status_code in self._REDIRECTS:
                where = urljoin(url, r.headers.get("location") or "")
                if self.match(where):          # buzzheavier.com <-> bzzhr.to
                    url = where
                    continue
                if referer:
                    raise ResolveError(f"the uploader's site ({referer}) doesn't let this file through")
                # Only visitors from the uploader's site get the file, and the
                # redirect names the site: come from there.
                p = urlsplit(where)
                referer = f"{p.scheme}://{p.netloc}/"
                continue
            if r.status_code == 404:
                raise DeadLinkError("file not found on BuzzHeavier (removed?)")
            if r.headers.get("cf-mitigated") or r.status_code in (403, 503):
                raise ResolveError(f"fast resolve: Cloudflare challenged it (HTTP {r.status_code})")
            if r.status_code != 200:
                raise ResolveError(f"fast resolve: HTTP {r.status_code}")
            return r.text or "", url
        raise ResolveError("fast resolve: too many redirects")

    @staticmethod
    def parse(page_html: str) -> tuple[str | None, str | None]:
        """(download button's hx-get path, file name) from a file page."""
        button = None
        for path in re.findall(r'hx-get="([^"]*/download\?[^"]*)"', page_html):
            path = html.unescape(path)
            if "alt=true" not in path:     # the alternate-server button
                button = path
                break
        m = (re.search(r'<p[^>]*class="[^"]*file-name[^"]*"[^>]*>([^<]+)</p>', page_html)
             or re.search(r"<title>([^<]+)</title>", page_html, re.IGNORECASE))
        name = html.unescape(m.group(1)).strip() if m else None
        return button, name

    def fast_resolve(self, url: str) -> tuple[str, str]:
        page_html, page_url = self.page(url)
        button, name = self.parse(page_html)
        if not button:
            raise ResolveError("fast resolve: no download button on the page")
        try:
            r = http_session().get(urljoin(page_url, button), allow_redirects=False, timeout=20,
                                   headers={**_HX, "HX-Current-URL": page_url, "Referer": page_url})
        except Exception as e:
            raise ResolveError(f"fast resolve: {type(e).__name__}: {e}") from e
        hx = r.headers.get("hx-redirect")
        if hx:
            return urljoin(page_url, hx.strip()), self.filename(url, name or "")
        if r.headers.get("cf-mitigated"):
            raise ResolveError("fast resolve: Cloudflare challenged the download button")
        if r.status_code == 404:
            raise DeadLinkError("file not found on BuzzHeavier (removed?)")
        raise ResolveError(f"fast resolve: download button answered HTTP {r.status_code} without a link")

    def browser(self, page, url: str, await_challenge) -> tuple[str, str]:
        page.goto(url, wait_until="domcontentloaded", timeout=60_000)
        if not self.match(page.url or url):
            p = urlsplit(page.url)     # sent to the uploader's site: come from there
            page.goto(url, referer=f"{p.scheme}://{p.netloc}/", wait_until="domcontentloaded", timeout=60_000)
        title = ""
        try:
            title = page.title() or ""
        except Exception:
            pass
        if title.strip().lower().startswith("not found"):
            raise DeadLinkError("file not found on BuzzHeavier (removed?)")
        # Cloudflare's interstitial, if any, clears by itself and loads the page.
        page.wait_for_selector("a.download-btn", timeout=45_000)
        challenge = "none"
        if page.query_selector('input[name="cf-turnstile-response"]'):
            challenge = await_challenge(page)
        try:
            title = page.title() or title
        except Exception:
            pass
        page.click("a.download-btn", timeout=30_000)
        return title, challenge


HOSTS: tuple[Host, ...] = (FuckingFast(), BuzzHeavier())


def host_for(url: str) -> Host | None:
    return next((h for h in HOSTS if h.match(url)), None)


def probe(signed: str) -> tuple[int | None, str | None]:
    """Ask for one byte of a signed URL: (total size, server's filename).

    Only for hosts whose ``probe`` is set. Never raises: it's a nicety for the
    link check, and the download doesn't depend on it.
    """
    try:
        r = http_session().get(signed, headers={"Range": "bytes=0-0"}, stream=True, timeout=20)
    except Exception:
        return None, None
    try:
        size = None
        m = re.search(r"/(\d+)\s*$", r.headers.get("content-range") or "")
        if m:
            size = int(m.group(1))
        elif r.status_code == 200 and (r.headers.get("content-length") or "").isdigit():
            size = int(r.headers["content-length"])
        cd = r.headers.get("content-disposition") or ""
        name = None
        m = re.search(r"filename\*\s*=\s*UTF-8''([^;]+)", cd, re.IGNORECASE) or \
            re.search(r'filename\s*=\s*"([^"]+)"', cd, re.IGNORECASE)
        if m:
            name = sanitize_filename(unquote(m.group(1)).strip())
        return size, name
    finally:
        try:
            r.close()
        except Exception:
            pass
