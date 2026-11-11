"""Server-side downloads: the TUI's download engine without the TUI.

The homelab server runs this inside the FitFast web service, so a game is
fetched, downloaded and extracted on the server, into the folder game-share
shares. Anyone on the LAN starts and watches downloads from the homelab page.

It is the same engine as app/tui.py (the legacy UI): FuckingFast links are
resolved one at a time (browserless first, Camoufox as the fallback, always
out of process), handed to one aria2 daemon, polled, retried with the same
backoff rules, and recorded in the per-folder .fitfast.json manifest, so a
restart never re-downloads what's already whole.

What it adds is a queue of games that outlives the process:
<root>/.fitfast/queue.json. Games run one at a time, in order, with their parts
in parallel. A restart picks the queue up where it was; signed URLs don't
survive it, so unfinished parts are simply resolved again.
"""
from __future__ import annotations

import json
import os
import queue
import secrets
import tempfile
import threading
import time
from pathlib import Path

from .downloader import Aria2NotFound, Aria2RpcError, Downloader
from .extractor import extract_all, find_unrar
from .log import get_logger, log_exception
from .resolve_proc import SubprocessResolver
from .resolver import common_folder_name, filename_from_fragment, sanitize_filename
from .scrape_proc import SubprocessScraper
from .scraper import ScrapeError, fast_scrape, game_title_from_links
from .state import DownloadState

GAMES_DIR = "FitFast"            # under the served root, so gs can pull finished games
STATE_DIR = ".fitfast"           # dot folder: hidden from gs and refused by the server

RETRY_BASE_DELAY_S = 5.0
RETRY_MAX_DELAY_S = 60.0
MAX_FAILS_WITHOUT_PROGRESS = 8
CHECKPOINT_EVERY_S = 10.0
TICK_S = 1.0
MAX_GAMES = 200

# queued: waiting its turn. scraping: reading the FitGirl page. downloading.
# paused: by hand; its aria2 downloads are paused and it gives up its turn.
# extracting. done. failed: parts gave up or the page couldn't be read.
ACTIVE_STATES = ("queued", "scraping", "downloading", "extracting")
BUSY_STATES = ("scraping", "downloading", "paused", "extracting")


class Part:
    def __init__(self, source: str, filename: str = "") -> None:
        self.source = source
        self.filename = filename or filename_from_fragment(source) or ""
        self.state = "queued"           # queued|resolving|downloading|done|error|failed
        self.gid: str | None = None
        self.error: str | None = None
        self.total = 0
        self.completed = 0
        self.speed = 0
        self.retry_count = 0
        self.next_retry_at = 0.0
        self.best_completed = 0
        self.fails_without_progress = 0

    def saved(self) -> dict:
        return {"source": self.source, "filename": self.filename,
                "state": self.state if self.state in ("done", "failed") else "queued",
                "total": self.total, "completed": self.completed, "error": self.error}

    @classmethod
    def load(cls, d: dict) -> "Part":
        p = cls(str(d["source"]), str(d.get("filename") or ""))
        p.state = d.get("state") if d.get("state") in ("done", "failed") else "queued"
        p.total = int(d.get("total") or 0)
        p.completed = int(d.get("completed") or 0)
        p.best_completed = p.completed
        p.error = d.get("error")
        return p

    def view(self) -> dict:
        return {"filename": self.filename, "state": self.state, "total": self.total,
                "completed": self.completed, "speed": self.speed, "error": self.error,
                "retries": self.retry_count}


class Game:
    def __init__(self, page: str | None, links: list[str], extract: bool, delete_archives: bool) -> None:
        self.id = secrets.token_urlsafe(9)
        self.page = page                 # FitGirl page still to scrape, if any
        self.title: str | None = None
        self.folder = ""                 # name under GAMES_DIR, set once links are known
        self.parts = [Part(u) for u in links]
        self.state = "queued"
        self.paused_from: str | None = None
        self.error: str | None = None
        self.message = ""
        self.extract = extract
        self.delete_archives = delete_archives
        self.created = time.time()
        self.finished: float | None = None
        self.started = False             # parts handed to the resolver this run
        self.scraping = False            # a scrape thread is running for it

    def saved(self) -> dict:
        state = self.state
        if state in ("scraping", "downloading", "extracting"):
            state = "queued"             # picked up again from the start of its turn
        return {"id": self.id, "page": self.page, "title": self.title, "folder": self.folder,
                "parts": [p.saved() for p in self.parts], "state": state, "error": self.error,
                "message": self.message, "extract": self.extract,
                "delete_archives": self.delete_archives, "created": self.created,
                "finished": self.finished}

    @classmethod
    def load(cls, d: dict) -> "Game":
        g = cls(d.get("page"), [], bool(d.get("extract", True)), bool(d.get("delete_archives", False)))
        g.id = str(d["id"])
        g.title = d.get("title")
        g.folder = str(d.get("folder") or "")
        g.parts = [Part.load(p) for p in d.get("parts", [])]
        g.state = d.get("state") or "queued"
        g.error = d.get("error")
        g.message = d.get("message") or ""
        g.created = float(d.get("created") or time.time())
        g.finished = d.get("finished")
        return g

    def view(self) -> dict:
        counts: dict[str, int] = {}
        total = completed = speed = 0
        for p in self.parts:
            counts[p.state] = counts.get(p.state, 0) + 1
            total += p.total
            completed += p.completed if p.state != "done" else p.total
            speed += p.speed if p.state == "downloading" else 0
        return {"id": self.id, "title": self.title or self.page or "", "folder": self.path(),
                "state": self.state, "error": self.error, "message": self.message,
                "extract": self.extract, "delete_archives": self.delete_archives,
                "created": self.created, "finished": self.finished, "counts": counts,
                "total": total, "completed": completed, "speed": speed,
                "parts": [p.view() for p in self.parts]}

    def path(self) -> str:
        return f"{GAMES_DIR}/{self.folder}" if self.folder else ""


class Manager:
    def __init__(self, root: str | Path) -> None:
        self.root = Path(root)
        self.games_dir = self.root / GAMES_DIR
        self.state_dir = self.root / STATE_DIR
        self.queue_file = self.state_dir / "queue.json"
        self.lock = threading.RLock()
        self.games: list[Game] = []
        self.limit_mbps = 0.0
        self.downloader: Downloader | None = None
        self.states: dict[str, DownloadState] = {}
        self.resolve_q: queue.Queue = queue.Queue()
        self.resolver: SubprocessResolver | None = None
        self.scraper: SubprocessScraper | None = None
        self.stopping = threading.Event()
        self._dirty = False
        self._last_save = 0.0
        self._last_checkpoint = 0.0
        self._workers: list[threading.Thread] = []
        self.log = get_logger()

    # ── persistence ───────────────────────────────────────────────────────────
    def load(self) -> None:
        try:
            data = json.loads(self.queue_file.read_text(encoding="utf-8"))
        except FileNotFoundError:
            return
        except (OSError, ValueError) as e:
            self.log.warning("fitfast: unreadable %s (%s); starting with an empty queue", self.queue_file, e)
            return
        with self.lock:
            self.limit_mbps = float(data.get("limit_mbps") or 0.0)
            for d in data.get("games", []):
                try:
                    self.games.append(Game.load(d))
                except (KeyError, TypeError, ValueError) as e:
                    self.log.warning("fitfast: skipping a damaged queue entry: %s", e)
        self.log.info("fitfast: loaded %d game(s) from the queue", len(self.games))

    def save(self) -> None:
        with self.lock:
            data = {"limit_mbps": self.limit_mbps, "games": [g.saved() for g in self.games]}
            self._dirty = False
            self._last_save = time.time()
        try:
            self.state_dir.mkdir(parents=True, exist_ok=True)
            fd, tmp = tempfile.mkstemp(dir=self.state_dir, prefix=".queue-")
            with os.fdopen(fd, "w", encoding="utf-8") as f:
                json.dump(data, f, indent=1)
                f.flush()
                os.fsync(f.fileno())
            os.replace(tmp, self.queue_file)
        except OSError as e:
            self.log.warning("fitfast: couldn't save the queue: %s", e)

    def _changed(self) -> None:
        self._dirty = True

    # ── public API (called from HTTP threads) ─────────────────────────────────
    def add(self, page: str | None, links: list[str], extract: bool = True,
            delete_archives: bool = False, title: str | None = None) -> Game:
        g = Game(page, links, extract, delete_archives)
        if links:
            g.title = title or game_title_from_links(links)
        with self.lock:
            done = [x for x in self.games if x.state in ("done", "failed")]
            while len(self.games) >= MAX_GAMES and done:
                self.games.remove(done.pop(0))
            if len(self.games) >= MAX_GAMES:
                raise ValueError(f"the queue is full ({MAX_GAMES} games)")
            self.games.append(g)
            self._changed()
        self.log.info("fitfast: queued game %s (%s)", g.id, page or f"{len(links)} links")
        return g

    def get(self, gid: str) -> Game | None:
        with self.lock:
            return next((g for g in self.games if g.id == gid), None)

    def snapshot(self) -> dict:
        with self.lock:
            games = [g.view() for g in self.games]
            return {"games": games, "limit_mbps": self.limit_mbps,
                    "speed": sum(g["speed"] for g in games),
                    "aria2": self.downloader is not None}

    def busy_paths(self) -> list[str]:
        with self.lock:
            return [g.path() for g in self.games if g.state in BUSY_STATES and g.folder]

    def pause(self, gid: str) -> bool:
        with self.lock:
            g = self.get(gid)
            if g is None or g.state not in ACTIVE_STATES or g.state == "extracting":
                return False
            g.paused_from, g.state = g.state, "paused"
            gids = [p.gid for p in g.parts if p.gid and p.state == "downloading"]
            self._changed()
        d = self.downloader
        for x in gids:
            try:
                d and d.pause(x)
            except Exception:
                pass
        return True

    def resume(self, gid: str) -> bool:
        with self.lock:
            g = self.get(gid)
            if g is None or g.state != "paused":
                return False
            g.state = "downloading" if g.started else "queued"
            gids = [p.gid for p in g.parts if p.gid and p.state == "downloading"]
            self._changed()
        d = self.downloader
        for x in gids:
            try:
                d and d.unpause(x)
            except Exception:
                pass
        return True

    def retry(self, gid: str) -> int:
        with self.lock:
            g = self.get(gid)
            if g is None:
                return 0
            n = 0
            for p in g.parts:
                if p.state in ("failed", "error"):
                    p.state, p.error, p.gid = "queued", None, None
                    p.retry_count = p.fails_without_progress = 0
                    p.next_retry_at = 0.0
                    n += 1
                    if g.started:
                        self.resolve_q.put((g.id, p))
            if g.state == "failed":
                g.state, g.error, g.finished, g.started = "queued", None, None, False
                n = max(n, 1)
            self._changed()
            return n

    def remove(self, gid: str) -> bool:
        """Drop a game from the queue. Its files stay on disk."""
        with self.lock:
            g = self.get(gid)
            if g is None:
                return False
            self.games.remove(g)
            gids = [p.gid for p in g.parts if p.gid]
            for p in g.parts:
                p.state = "cancelled"
            self._changed()
        d = self.downloader
        for x in gids:
            try:
                d and d.remove(x)
            except Exception:
                pass
        self.log.info("fitfast: removed game %s", gid)
        return True

    def set_limit(self, mbps: float) -> None:
        with self.lock:
            self.limit_mbps = max(0.0, mbps)
            self._changed()
        if self.downloader is not None:
            self.downloader.set_overall_limit(self.limit_mbps)

    # ── the engine ────────────────────────────────────────────────────────────
    def start(self) -> None:
        self.load()
        for target in (self._control_loop, self._resolver_loop):
            t = threading.Thread(target=target, daemon=True, name=target.__name__)
            t.start()
            self._workers.append(t)

    def shutdown(self) -> None:
        """Stop cleanly: aria2 writes its control files, the queue is saved."""
        self.stopping.set()
        for h in (self.resolver, self.scraper):
            try:
                h and h.close()
            except Exception:
                pass
        d, self.downloader = self.downloader, None
        if d is not None:
            d.stop()
        self.save()
        self.log.info("fitfast: stopped")

    def _control_loop(self) -> None:
        while not self.stopping.is_set():
            try:
                self._tick()
            except Exception as e:
                log_exception("fitfast control loop", e)
            if self._dirty and time.time() - self._last_save > 2:
                self.save()
            self.stopping.wait(TICK_S)

    def _current(self) -> Game | None:
        """The game whose turn it is: the first one still to finish, skipping paused ones."""
        return next((g for g in self.games if g.state in ACTIVE_STATES), None)

    def _tick(self) -> None:
        with self.lock:
            g = self._current()
            if g is None:
                needs_aria2 = any(p.gid for x in self.games for p in x.parts if p.state == "downloading")
            else:
                needs_aria2 = True
            state = g.state if g else None
        if g is None:
            if not needs_aria2 and self.downloader is not None:
                d, self.downloader = self.downloader, None
                d.stop()
            self._poll()
            return
        if state == "queued":
            if g.page and not g.parts:
                if not g.scraping:
                    self._spawn(self._scrape, g)
                return
            self._begin(g)
        elif state == "downloading":
            self._poll()

    def _spawn(self, fn, g: Game) -> None:
        with self.lock:
            if fn == self._scrape:
                g.state, g.scraping = "scraping", True
                g.message = "Reading the FitGirl page…"
            else:
                g.state = "extracting"
            self._changed()
        threading.Thread(target=fn, args=(g,), daemon=True, name=f"{fn.__name__}-{g.id}").start()

    def _scrape(self, g: Game) -> None:
        try:
            self._scrape_page(g)
        finally:
            g.scraping = False

    def _scrape_page(self, g: Game) -> None:
        try:
            try:
                result = fast_scrape(g.page)
            except ScrapeError as e:
                self.log.info("fitfast: fast scrape failed (%s); using the browser", e)
                self.scraper = SubprocessScraper()
                try:
                    result = self.scraper.scrape(g.page, fast=False)
                finally:
                    self.scraper = None
        except Exception as e:
            if self.stopping.is_set():
                return
            with self.lock:
                g.state, g.error, g.finished = "failed", f"couldn't read the page: {e}"[:400], time.time()
                g.message = ""
                self._changed()
            self.log.warning("fitfast: scrape failed for %s: %s", g.id, e)
            return
        with self.lock:
            if g not in self.games:
                return
            g.parts = [Part(u) for u in result.links]
            g.title = game_title_from_links(result.links) or result.title or g.title
            if not g.parts:
                g.state, g.error, g.finished = "failed", "no FuckingFast links on that page", time.time()
            elif g.state == "scraping":
                g.state = "queued"
            elif g.state == "paused":
                g.paused_from = "queued"
            g.message = f"Found {len(g.parts)} part(s)."
            self._changed()

    def _ensure_downloader(self) -> Downloader:
        if self.downloader is None:
            d = Downloader(concurrent_files=3, max_overall_dl_mbps=self.limit_mbps)
            d.start()
            self.downloader = d
        return self.downloader

    def _begin(self, g: Game) -> None:
        """Start a game's turn: skip what's already whole, queue the rest for resolving."""
        with self.lock:
            if not g.folder:
                names = [p.filename for p in g.parts if p.filename]
                g.folder = sanitize_filename(g.title) if g.title else common_folder_name(names)
        folder = self.games_dir / g.folder
        try:
            folder.mkdir(parents=True, exist_ok=True)
        except OSError as e:
            with self.lock:
                g.state, g.error, g.finished = "failed", f"can't create {folder}: {e}", time.time()
                self._changed()
            return
        st = self.states.get(g.id) or DownloadState(folder)
        self.states[g.id] = st
        pending: list[Part] = []
        vanished: list[str] = []
        with self.lock:
            for p in g.parts:
                if p.state in ("done", "failed"):
                    if p.state == "done" and not st.is_complete(p.source) and st.vanished(p.source):
                        vanished.append(p.filename)
                        p.state = "queued"
                    else:
                        continue
                st.adopt_if_finished(p.source)
                p.filename = st.known_filename(p.source) or p.filename
                if st.is_complete(p.source):
                    entry = st.get(p.source) or {}
                    p.state = "done"
                    p.total = p.completed = int(entry.get("total") or 0)
                    continue
                p.state, p.gid, p.error = "queued", None, None
                pending.append(p)
        if vanished:
            self.log.warning("fitfast: %d finished file(s) are missing now: %s", len(vanished), ", ".join(vanished[:5]))
        if pending:
            try:
                self._ensure_downloader()
            except (Aria2NotFound, Aria2RpcError) as e:
                with self.lock:
                    g.state, g.error = "failed", f"aria2 didn't start: {e}"
                    g.finished = time.time()
                    self._changed()
                return
        with self.lock:
            if g.state != "queued":       # paused or removed meanwhile
                return
            g.state, g.started = "downloading", True
            g.message = f"Downloading {len(pending)} part(s) into {g.path()}" if pending else ""
            self._changed()
            for p in pending:
                self.resolve_q.put((g.id, p))
        self.log.info("fitfast: game %s: %d to download, %d already whole",
                      g.id, len(pending), len(g.parts) - len(pending))
        if not pending:
            self._finish(g)

    def _resolver_loop(self) -> None:
        while not self.stopping.is_set():
            try:
                gid, p = self.resolve_q.get(timeout=0.5)
            except queue.Empty:
                continue
            with self.lock:
                g = self.get(gid)
                if g is None or p.state != "queued" or p not in g.parts:
                    continue
                if g.state == "paused":
                    later = True
                else:
                    later = False
                    p.state = "resolving"
            if later:
                threading.Timer(2.0, self.resolve_q.put, args=((gid, p),)).start()
                continue
            self.resolver = SubprocessResolver()
            try:
                signed, filename = self.resolver.resolve(p.source)
            except Exception as e:
                if self.stopping.is_set():
                    return
                with self.lock:
                    if p.state == "resolving":
                        self._mark_failed(p, f"resolve error: {e}")
                continue
            finally:
                self.resolver = None
            st = self.states.get(gid)
            if st is not None:
                if st.heal_fragmented_partial(p.source) is not None:
                    self.log.info("fitfast: healed fragmented partial %s", filename)
                if st.clear_ghost_file(p.source):
                    self.log.info("fitfast: discarded unverifiable partial %s", filename)
                st.record_resolved(p.source, filename)
            d = self.downloader
            with self.lock:
                if p.state != "resolving" or d is None or self.stopping.is_set():
                    continue
                p.filename = filename
                folder = str(self.games_dir / g.folder)
            try:
                gid_ = d.add(signed, folder, filename)
            except Exception as e:
                with self.lock:
                    self._mark_failed(p, f"aria2 error: {e}")
                continue
            with self.lock:
                if p.state == "cancelled":
                    try:
                        d.remove(gid_)
                    except Exception:
                        pass
                    continue
                p.gid, p.state = gid_, "downloading"
                if g.state == "paused":
                    try:
                        d.pause(gid_)
                    except Exception:
                        pass

    def _mark_failed(self, p: Part, error: str) -> None:
        """Same rules as the TUI: retry forever while a part makes progress,
        give up after MAX_FAILS_WITHOUT_PROGRESS attempts that don't."""
        p.error = error[:300]
        p.gid = None
        if p.completed > p.best_completed:
            p.best_completed = p.completed
            p.fails_without_progress = 0
        else:
            p.fails_without_progress += 1
        p.retry_count += 1
        self.log.warning("fitfast: part %s failed (attempt %d): %s", p.filename, p.retry_count, error)
        if p.fails_without_progress >= MAX_FAILS_WITHOUT_PROGRESS:
            p.state = "failed"
        else:
            p.state = "error"
            p.next_retry_at = time.time() + min(RETRY_BASE_DELAY_S * 2 ** (p.retry_count - 1), RETRY_MAX_DELAY_S)
        self._changed()

    def _poll(self) -> None:
        d = self.downloader
        with self.lock:
            live = [(g, p) for g in self.games for p in g.parts]
        now = time.time()
        for g, p in live:
            with self.lock:
                if p.state == "error" and now >= p.next_retry_at and g.state == "downloading":
                    p.state, p.gid = "queued", None
                    self.resolve_q.put((g.id, p))
                    continue
                gid = p.gid if p.state == "downloading" else None
            if gid is None or d is None:
                continue
            try:
                st = d.status(gid)
            except Exception:
                continue
            with self.lock:
                if p.gid != gid or p.state != "downloading":
                    continue
                p.total = int(st.get("totalLength") or 0)
                p.completed = int(st.get("completedLength") or 0)
                p.speed = int(st.get("downloadSpeed") or 0)
                status = st.get("status", "")
                if status == "complete":
                    p.state, p.speed, p.gid = "done", 0, None
                    s = self.states.get(g.id)
                    if s is not None:
                        s.record_complete(p.source, p.filename, p.total)
                    self._changed()
                elif status in ("error", "removed"):
                    p.speed = 0
                    self._mark_failed(p, st.get("errorMessage") or "aria2 error")
        if now - self._last_checkpoint >= CHECKPOINT_EVERY_S:
            self._last_checkpoint = now
            with self.lock:
                for g, p in live:
                    s = self.states.get(g.id)
                    if s is not None and p.state == "downloading" and p.total:
                        s.record_progress(p.source, p.filename, p.total, p.completed)
                self._changed()
        with self.lock:
            g = self._current()
            ready = g is not None and g.state == "downloading" and all(
                p.state in ("done", "failed") for p in g.parts)
        if ready:
            self._finish(g)

    def _finish(self, g: Game) -> None:
        with self.lock:
            done = sum(1 for p in g.parts if p.state == "done")
            failed = sum(1 for p in g.parts if p.state == "failed")
            g.message = f"{done} part(s) downloaded" + (f", {failed} failed" if failed else "")
            self._changed()
        self.log.info("fitfast: game %s: %s", g.id, g.message)
        if failed:
            with self.lock:
                g.state, g.finished = "failed", time.time()
                g.error = "some parts kept failing; press Retry to try them again"
            return
        if g.extract and done and find_unrar() is not None:
            self._spawn(self._extract, g)
            return
        with self.lock:
            g.state, g.finished = "done", time.time()
            self._changed()

    def _extract(self, g: Game) -> None:
        folder = self.games_dir / g.folder

        def on_start(name, i, total):
            with self.lock:
                g.message = f"Extracting {i}/{total}: {name}"

        try:
            ok, fail = extract_all(folder, delete_after=g.delete_archives, on_set_start=on_start)
            with self.lock:
                g.message = f"Extracted {ok} set(s)" + (f", {fail} failed" if fail else "")
                g.state = "failed" if fail else "done"
                if fail:
                    g.error = "extraction failed; the archives are still there"
        except Exception as e:
            log_exception("extracting", e)
            with self.lock:
                g.state, g.error = "failed", f"extraction error: {e}"
        with self.lock:
            g.finished = time.time()
            self._changed()
