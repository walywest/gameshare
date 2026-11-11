"""Scrape a fitgirl-repacks.site game page for its FuckingFast part links.

FitGirl posts list the FuckingFast mirror as inline anchors in the exact form
    https://fuckingfast.co/<id>#<Game>_--_fitgirl-repacks.site_--_.partNN.rar
so scraping is just: load the page past Cloudflare, grab every fuckingfast.co
anchor, natural-sort by part number, dedupe.
"""
from __future__ import annotations
import html
import re

from camoufox.sync_api import Camoufox

FITGIRL_HOST_RE = re.compile(r"^https?://(www\.)?fitgirl-repacks\.site/", re.IGNORECASE)
FF_HREF_RE = re.compile(r"https?://fuckingfast\.co/[A-Za-z0-9]+", re.IGNORECASE)
_PART_RE = re.compile(r"\.part(\d+)\.", re.IGNORECASE)
_INVALID_CHARS = re.compile(r'[<>:"/\\|?*\x00-\x1f]')


class ScrapeError(Exception):
    pass


class ScrapeResult:
    """Links plus the game title parsed from the page (used for the subfolder)."""
    __slots__ = ("links", "title")

    def __init__(self, links: list[str], title: str | None):
        self.links = links
        self.title = title


def is_fitgirl_url(url: str) -> bool:
    return bool(FITGIRL_HOST_RE.match(url.strip()))


def _base_and_part(url: str) -> tuple[str, int]:
    """Split a link's filename fragment into (base_set_name, part_number).

    A repack can contain several archive sets (main game + bonus content);
    grouping by base name keeps each set contiguous instead of interleaving
    them by part number.
    """
    frag = url.split("#", 1)[1] if "#" in url else url
    m = _PART_RE.search(frag)
    part = int(m.group(1)) if m else 0
    base = frag[: m.start()] if m else frag
    return (base.lower(), part)


def _clean_title(raw: str) -> str | None:
    if not raw:
        return None
    # Strip common WordPress suffixes: " - FitGirl Repacks", " – FitGirl Repacks"
    t = re.split(r"\s+[-–—]\s+FitGirl", raw, maxsplit=1)[0].strip()
    t = _INVALID_CHARS.sub("_", t).strip("._- ")
    return t or None


def _collect_links(page, links: list[str], seen: set[str]) -> None:
    """Append any new FuckingFast anchors on ``page`` into ``links`` (deduped)."""
    hrefs = page.eval_on_selector_all(
        "a[href*='fuckingfast']",
        "els => els.map(e => e.href)",
    )
    for h in hrefs:
        m = FF_HREF_RE.match(h)
        if not m:
            continue
        clean = h if "#" in h else m.group(0)
        key = clean.split("#", 1)[0]
        if key in seen:
            continue
        seen.add(key)
        links.append(clean)


def _scrape_once(page_url: str, timeout_ms: int) -> tuple[list[str], str | None]:
    """One browser session. Returns whatever links/title it captured.

    Grabs the links the instant the page's DOM is ready — before any extra
    waits or clicks — so an intermittent post-load browser crash (common with
    these ad-heavy pages in headless mode) can't discard data we already have.
    A crash after we've collected the links is swallowed; the caller decides
    whether an empty result is worth retrying.
    """
    links: list[str] = []
    seen: set[str] = set()
    title: str | None = None
    try:
        with Camoufox(headless=True, humanize=False, geoip=False) as browser:
            page = browser.new_page()
            page.goto(page_url, wait_until="domcontentloaded", timeout=timeout_ms)

            # Primary grab: the part links are static anchors present on load.
            _collect_links(page, links, seen)

            # Game title from the H1 (falls back to <title>).
            try:
                title = _clean_title(
                    page.eval_on_selector("h1.entry-title", "e => e.textContent")
                )
            except Exception:
                title = None
            if not title:
                try:
                    title = _clean_title(page.title())
                except Exception:
                    title = None

            # Best-effort: expand collapsible spoiler sections in case a repack
            # hides extra archive sets, then re-collect. Never fatal — we already
            # have the primary links, and clicking can destabilise the browser.
            try:
                page.eval_on_selector_all(
                    ".su-spoiler-title, .su-spoiler",
                    "els => els.forEach(e => { try { e.click(); } catch (_) {} })",
                )
                page.wait_for_timeout(500)
                _collect_links(page, links, seen)
            except Exception:
                pass
    except Exception:
        # A late browser/driver crash is only a problem if we got nothing.
        if not links:
            raise
    return links, title


_ANCHOR_RE = re.compile(r"""href\s*=\s*["'](https?://fuckingfast\.co/[^"'\s>]+)["']""",
                        re.IGNORECASE)
_H1_RE = re.compile(r'<h1[^>]*class="[^"]*entry-title[^"]*"[^>]*>(.*?)</h1>',
                    re.IGNORECASE | re.DOTALL)
_TITLE_RE = re.compile(r"<title[^>]*>(.*?)</title>", re.IGNORECASE | re.DOTALL)
_TAG_RE = re.compile(r"<[^>]+>")


def _html_text(fragment: str) -> str:
    return html.unescape(_TAG_RE.sub("", fragment)).strip()


def fast_scrape(page_url: str, timeout: float = 30.0) -> ScrapeResult:
    """Scrape without a browser. The part links are static anchors (spoiler
    sections are only hidden with CSS), so one Chrome-TLS GET is enough when
    Cloudflare isn't challenging. Raises ScrapeError; callers fall back to the
    browser scrape."""
    from .resolver import http_session

    page_url = page_url.strip()
    if not is_fitgirl_url(page_url):
        raise ScrapeError("Not a fitgirl-repacks.site URL")
    try:
        r = http_session().get(page_url, timeout=timeout)
    except Exception as e:
        raise ScrapeError(f"fast scrape: {type(e).__name__}: {e}") from e
    if r.status_code != 200 or r.headers.get("cf-mitigated"):
        raise ScrapeError(f"fast scrape: HTTP {r.status_code}")
    text = r.text or ""
    links: list[str] = []
    seen: set[str] = set()
    for raw in _ANCHOR_RE.findall(text):
        h = html.unescape(raw)
        m = FF_HREF_RE.match(h)
        if not m:
            continue
        key = m.group(0)
        if key in seen:
            continue
        seen.add(key)
        links.append(h if "#" in h else key)
    if not links:
        raise ScrapeError(
            "No FuckingFast links found on that page. Make sure it's a game "
            "repack page (not the homepage or an 'upcoming' post).")
    links.sort(key=_base_and_part)
    title = None
    for rx in (_H1_RE, _TITLE_RE):
        mt = rx.search(text)
        if mt:
            title = _clean_title(_html_text(mt.group(1)))
            if title:
                break
    return ScrapeResult(links, title)


def scrape_fitgirl_page(page_url: str, timeout_ms: int = 60_000,
                        attempts: int = 2) -> ScrapeResult:
    """Return the sorted, de-duplicated FuckingFast links on a FitGirl repack
    post plus the parsed game title. Raises ScrapeError on failure."""
    page_url = page_url.strip()
    if not is_fitgirl_url(page_url):
        raise ScrapeError("Not a fitgirl-repacks.site URL")

    links: list[str] = []
    title: str | None = None
    last_err: Exception | None = None
    # Retry once: the browser can crash before we capture anything. Callers
    # that already enforce a hard timeout (app/scrape_proc.py) pass attempts=1
    # and retry the whole process instead, which halves the worst-case wait and
    # cannot retry inside a wedged process.
    for _attempt in range(attempts):
        try:
            links, title = _scrape_once(page_url, timeout_ms)
        except Exception as e:
            last_err = e
            continue
        if links:
            break

    if not links:
        if last_err is not None:
            raise ScrapeError(f"Failed to load page: {last_err}") from last_err
        raise ScrapeError(
            "No FuckingFast links found on that page. Make sure it's a game "
            "repack page (not the homepage or an 'upcoming' post)."
        )

    links.sort(key=_base_and_part)
    return ScrapeResult(links, title)


def game_title_from_links(links: list[str]) -> str | None:
    """Best-effort game name from the shared filename prefix in the fragments."""
    for l in links:
        if "#" in l:
            frag = l.split("#", 1)[1]
            frag = re.split(r"_--_", frag)[0]  # "Game_--_fitgirl..." -> "Game"
            if frag and "bonus" not in frag.lower() and "optional" not in frag.lower():
                name = frag.replace("_", " ").strip()
                if name:
                    return name
    return None
