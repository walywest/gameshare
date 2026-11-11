"""FitFast — terminal UI front-end (Linux / macOS / Nix friendly).

Drives the resolver / downloader / scraper / extractor backends through a
Textual TUI. Paste a FitGirl page (or FuckingFast links), press Fetch, then
Start; downloads run through aria2c and are auto-extracted with unar/unrar.

Keys:  f Fetch · s Start · p Pause/Resume · c Cancel · i Install browser ·
       d Remove selected link · x Clear all links · q Quit
"""
from __future__ import annotations
import queue
import threading
import time
from pathlib import Path

from textual.app import App, ComposeResult
from textual.containers import Horizontal, Vertical
from textual.widgets import (
    Header, Footer, Input, Button, DataTable, RichLog, Checkbox, Label,
)

if __package__ in (None, ""):
    import sys
    sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
    from app import config as config_mod
    from app.bootstrap import browser_ready, install_in_subprocess, browser_staleness_warning
    from app.downloader import Downloader, Aria2NotFound, Aria2RpcError
    from app.resolver import (
        Resolver, valid_url, filename_from_fragment,
        common_folder_name, unique_folder, sanitize_filename,
    )
    from app.resolve_proc import SubprocessResolver
    from app.scrape_proc import SubprocessScraper, ScrapeTimeout, SCRAPE_TIMEOUT_S
    from app.scraper import game_title_from_links, ScrapeError, is_fitgirl_url
    from app.extractor import extract_all, find_unrar
    from app.log import setup_logging, get_logger, log_exception
    from app.format import format_bytes, format_speed, format_eta
    from app.state import DownloadState
else:
    from . import config as config_mod
    from .bootstrap import browser_ready, install_in_subprocess, browser_staleness_warning
    from .downloader import Downloader, Aria2NotFound, Aria2RpcError
    from .resolver import (
        Resolver, valid_url, filename_from_fragment,
        common_folder_name, unique_folder, sanitize_filename,
    )
    from .resolve_proc import SubprocessResolver
    from .scrape_proc import SubprocessScraper, ScrapeTimeout, SCRAPE_TIMEOUT_S
    from .scraper import game_title_from_links, ScrapeError, is_fitgirl_url
    from .extractor import extract_all, find_unrar
    from .log import setup_logging, get_logger, log_exception
    from .format import format_bytes, format_speed, format_eta
    from .state import DownloadState


class Job:
    __slots__ = ("url", "filename", "state", "gid", "error",
                 "total", "completed", "speed", "retry_count", "next_retry_at",
                 "best_completed", "fails_without_progress")

    def __init__(self, url: str, filename: str):
        self.url = url
        self.filename = filename
        self.state = "queued"      # queued|resolving|downloading|done|error|failed
        self.gid: str | None = None
        self.error: str | None = None
        self.total = 0
        self.completed = 0
        self.speed = 0
        self.retry_count = 0
        self.next_retry_at = 0.0
        # Highest byte count this part has ever reached; lets us tell a slowly
        # progressing part (retry forever) from a truly stuck one (give up).
        self.best_completed = 0
        self.fails_without_progress = 0


class FitFastTUI(App):
    TITLE = "FitFast Downloader"
    SUB_TITLE = "FitGirl / FuckingFast · terminal edition"

    # Failed jobs are re-queued with exponential backoff. A part that keeps
    # *advancing* between failures retries indefinitely (its backoff resets).
    # A part that fails this many times WITHOUT moving a byte is given up on,
    # so a dead link can't churn the single browser resolver forever.
    RETRY_BASE_DELAY_S = 5.0
    RETRY_MAX_DELAY_S = 60.0
    MAX_FAILS_WITHOUT_PROGRESS = 8
    CHECKPOINT_EVERY_S = 10.0

    CSS = """
    Screen { layout: vertical; }
    #inputs { height: auto; padding: 1 1 0 1; }
    #inputs Label { color: $text-muted; }
    #buttons { height: auto; padding: 1; }
    #buttons Button { margin: 0 1 0 0; }
    #opts { height: auto; padding: 0 1; align-vertical: middle; }
    #bwlimit_label { color: $text-muted; width: auto; margin: 0 1; }
    .bwlimit { width: 10; margin: 0; }
    #pending_label { color: $text-muted; padding: 0 1; }
    #pending { height: 8; margin: 0 1 1 1; }
    DataTable { height: 1fr; margin: 0 1; }
    RichLog { height: 8; margin: 1 1; border: round $primary; padding: 0 1; }
    Input { margin: 0 0 1 0; }
    """

    BINDINGS = [
        ("f", "fetch", "Fetch links"),
        ("s", "start", "Start"),
        ("p", "pause", "Pause/Resume"),
        ("c", "cancel", "Cancel"),
        ("i", "install", "Install browser"),
        ("d", "remove_link", "Remove link"),
        ("x", "clear_pending", "Clear links"),
        ("q", "quit", "Quit"),
    ]

    def __init__(self) -> None:
        super().__init__()
        self.config = config_mod.load()
        self.jobs: list[Job] = []
        self.jobs_lock = threading.Lock()
        self.dest_folder: Path | None = None
        self.running = False
        self.paused = False

        self.resolver: Resolver | None = None
        self.downloader: Downloader | None = None
        self.resolve_queue: "queue.Queue[Job]" = queue.Queue()
        self.shutdown_event = threading.Event()
        self.resolver_thread: threading.Thread | None = None
        # Bumped on every Start. shutdown_event alone cannot retire an old
        # resolver thread: Start clears the flag again, and a thread still
        # parked in resolve() reads that as permission to carry on.
        self._run_epoch = 0
        self._fetch_thread: threading.Thread | None = None
        self._fetch_started_at = 0.0
        # Held so action_quit can kill an in-flight fetch: the child is
        # detached into its own process group, so a local reference would
        # leave a Camoufox tree running after the app is gone.
        self.scraper: SubprocessScraper | None = None
        self._extract_thread: threading.Thread | None = None
        self._setup_thread: threading.Thread | None = None
        self._pending_urls: list[str] = []
        self.state: DownloadState | None = None
        self._last_checkpoint = 0.0

    # ---- layout -----------------------------------------------------------
    def compose(self) -> ComposeResult:
        yield Header(show_clock=True)
        with Vertical(id="inputs"):
            yield Label("FitGirl page URL (or paste FuckingFast links, space/line separated):")
            yield Input(placeholder="https://fitgirl-repacks.site/some-game/", id="url")
            yield Label("Destination folder:")
            yield Input(value=str(self.config.get("destination") or Path.home() / "Downloads"),
                        id="dest")
        with Horizontal(id="opts"):
            yield Checkbox("Auto-extract .rar when done",
                           value=bool(self.config.get("auto_extract", True)), id="autoextract")
            yield Checkbox("Delete archives after extract",
                           value=bool(self.config.get("delete_archives_after", False)), id="delete")
            yield Label("Speed limit MB/s (0=∞):", id="bwlimit_label")
            yield Input(value=self._fmt_limit(self.config.get("max_overall_dl_mbps", 0.0)),
                        id="bwlimit", classes="bwlimit")
        with Horizontal(id="buttons"):
            yield Button("Fetch links", id="fetch", variant="primary")
            yield Button("Remove selected", id="remove")
            yield Button("Clear links", id="clear")
            yield Button("Start", id="start", variant="success")
            yield Button("Pause", id="pause", disabled=True)
            yield Button("Cancel", id="cancel", variant="error", disabled=True)
        yield Label(
            "Links queued to download (select a row, press d or Remove to drop it):",
            id="pending_label",
        )
        yield DataTable(id="pending", zebra_stripes=True, cursor_type="row")
        yield DataTable(id="jobs", zebra_stripes=True, cursor_type="row")
        yield RichLog(id="log", highlight=False, markup=False, wrap=True)
        yield Footer()

    def on_mount(self) -> None:
        try:
            setup_logging()
        except Exception:
            pass
        table = self.query_one("#jobs", DataTable)
        table.add_column("File", key="file", width=44)
        table.add_column("Status", key="status", width=16)
        table.add_column("%", key="pct", width=7)
        table.add_column("Size", key="size", width=20)
        table.add_column("Speed", key="speed", width=12)
        table.add_column("ETA", key="eta", width=9)
        pending = self.query_one("#pending", DataTable)
        pending.add_column("File", key="file", width=44)
        pending.add_column("URL", key="url")
        self.set_interval(0.5, self._poll)
        self._log("Ready. Paste a FitGirl page or FuckingFast links, then Fetch / Start.")
        if not browser_ready():
            self._log("Stealth browser not installed (or not the build this camoufox "
                      "version needs). Press 'i' to install it (one-time, ~1.2 GB).")
        else:
            stale = browser_staleness_warning()
            if stale:
                self._log(stale)
                get_logger().warning(stale)
        if find_unrar() is None:
            self._log("Note: no unar/unrar found — downloads work but auto-extract is off. "
                      "Install 'unar' to enable it.")

    # ---- small helpers ----------------------------------------------------
    def _log(self, msg: str) -> None:
        self.query_one("#log", RichLog).write(msg)

    def _row_id(self, job: Job) -> str:
        return str(id(job))

    def _set_status(self, job: Job, text: str) -> None:
        table = self.query_one("#jobs", DataTable)
        try:
            table.update_cell(self._row_id(job), "status", text)
        except Exception:
            pass

    # ---- buttons / inputs -------------------------------------------------
    def on_button_pressed(self, event: Button.Pressed) -> None:
        {"fetch": self.action_fetch, "start": self.action_start,
         "pause": self.action_pause, "cancel": self.action_cancel,
         "remove": self.action_remove_link, "clear": self.action_clear_pending}.get(
            event.button.id, lambda: None)()

    def on_input_submitted(self, event: Input.Submitted) -> None:
        # One handler for every Input: Textual dispatches by event type, not by
        # widget, so a second `def on_input_submitted` would silently shadow
        # this one and kill whichever box it did not handle. Dispatch on the id.
        if event.input.id == "bwlimit":
            self._apply_limit_input()
            return
        # Enter in the URL box fetches a FitGirl page, or loads direct
        # links into the review list so they can be checked/removed first.
        if event.input.id == "url":
            url = event.value.strip()
            if is_fitgirl_url(url):
                self.action_fetch()
            else:
                raw = event.value.replace(",", " ").split()
                urls = [u for u in raw if valid_url(u)]
                if urls:
                    self._load_pending(urls)
                else:
                    self._log("Paste a FitGirl page URL, or one or more FuckingFast links.")

    # ---- pending links (reviewed before Start) -----------------------------
    def _load_pending(self, urls: list[str]) -> None:
        self._pending_urls = list(urls)
        self._refresh_pending_table()
        self._log(f"Loaded {len(urls)} link(s). Review below, remove any you "
                  "don't want, then press Start.")

    def _refresh_pending_table(self) -> None:
        table = self.query_one("#pending", DataTable)
        table.clear()
        for u in self._pending_urls:
            name = filename_from_fragment(u) or "(unknown)"
            table.add_row(name, u, key=u)

    def action_remove_link(self) -> None:
        if self.running:
            return
        table = self.query_one("#pending", DataTable)
        if table.row_count == 0:
            return
        try:
            row_key, _ = table.coordinate_to_cell_key(table.cursor_coordinate)
        except Exception:
            return
        url = row_key.value
        if url in self._pending_urls:
            self._pending_urls.remove(url)
        table.remove_row(row_key)
        self._log(f"Removed link: {url}")

    def action_clear_pending(self) -> None:
        if self.running or not self._pending_urls:
            return
        self._pending_urls = []
        self.query_one("#pending", DataTable).clear()
        self._log("Cleared all pending links.")

    # ---- fetch ------------------------------------------------------------
    def action_install(self) -> None:
        if self._setup_thread and self._setup_thread.is_alive():
            return
        if browser_ready():
            self._log("Stealth browser already installed.")
            return
        self._log("Installing stealth browser… one-time download of ~1.2 GB.")
        self._setup_thread = threading.Thread(target=self._install_worker, daemon=True)
        self._setup_thread.start()

    def _install_worker(self) -> None:
        try:
            install_in_subprocess(lambda m: self.call_from_thread(self._log, m))
            self.call_from_thread(self._log, "Browser ready. You can Fetch now.")
        except Exception as e:
            tb = log_exception("installing the stealth browser", e)
            self.call_from_thread(self._log, f"Browser install failed: {e}")

    def action_fetch(self) -> None:
        if self.running:
            return
        if self._fetch_thread and self._fetch_thread.is_alive():
            # Recoverable: a fetch that outlives the subprocess timeout is
            # wedged, not working. Skipping forever is how one bad fetch used
            # to kill this button for the rest of the session, silently.
            age = time.time() - self._fetch_started_at
            if age < SCRAPE_TIMEOUT_S + 15:
                self._log("A fetch is already running — give it a moment.")
                return
            self._log("Previous fetch never returned; starting a fresh one.")
        url = self.query_one("#url", Input).value.strip()
        if not url:
            self._log("Paste a FitGirl page URL first, then Fetch.")
            return
        if not is_fitgirl_url(url):
            self._log("That isn't a fitgirl-repacks.site URL. For direct FuckingFast "
                      "links, skip Fetch and press Start.")
            return
        if not browser_ready():
            self._log("Stealth browser not installed — press 'i' first.")
            return
        self._log(f"Fetching links from {url} (passing Cloudflare)…")
        self._fetch_started_at = time.time()
        self._fetch_thread = threading.Thread(
            target=self._fetch_worker, args=(url,), daemon=True)
        self._fetch_thread.start()

    def _fetch_worker(self, url: str) -> None:
        # Out of process, with a hard timeout: an in-process Camoufox launch
        # can wedge forever, and because action_fetch skips while this thread
        # is alive, one wedge would kill Fetch for the rest of the session.
        scraper = SubprocessScraper()
        self.scraper = scraper
        try:
            result = scraper.scrape(url)
        except ScrapeTimeout as e:
            self.call_from_thread(self._log, f"Fetch failed: {e}")
            return
        except ScrapeError as e:
            self.call_from_thread(self._log, f"Fetch failed: {e}")
            return
        except Exception as e:
            tb = log_exception("fetching FitGirl page", e)
            self.call_from_thread(self._log, f"Fetch failed: {e}")
            return
        finally:
            if self.scraper is scraper:
                self.scraper = None
        subfolder = game_title_from_links(result.links) or result.title or ""
        self.call_from_thread(self._fetch_done, result.links, subfolder)

    def _fetch_done(self, links: list[str], subfolder: str) -> None:
        self._load_pending(links)
        if subfolder:
            self._suggested_subfolder = sanitize_filename(subfolder)
        self._log(f"Fetched {len(links)} link(s)"
                  + (f' for "{subfolder}".' if subfolder else ".")
                  + " Review below, remove any you don't want, then press Start.")

    # ---- start ------------------------------------------------------------
    def action_start(self) -> None:
        if self.running:
            return
        urls = list(self._pending_urls)
        if not urls:
            # Fall back to whatever direct links are in the URL box.
            raw = self.query_one("#url", Input).value.replace(",", " ").split()
            urls = [u for u in raw if valid_url(u)]
        if not urls:
            self._log("Nothing to download. Fetch a FitGirl page, or paste FuckingFast links.")
            return

        dest_root = Path(self.query_one("#dest", Input).value.strip()).expanduser()
        try:
            dest_root.mkdir(parents=True, exist_ok=True)
        except OSError as e:
            self._log(f"Destination not writable: {e}")
            return

        names = [filename_from_fragment(u) or "" for u in urls]
        subfolder = getattr(self, "_suggested_subfolder", "") or common_folder_name(
            [n for n in names if n])
        try:
            self.dest_folder = unique_folder(dest_root, subfolder)
        except OSError as e:
            self._log(f"Cannot create subfolder: {e}")
            return

        self.config["destination"] = str(dest_root)
        self.config["auto_extract"] = self.query_one("#autoextract", Checkbox).value
        self.config["delete_archives_after"] = self.query_one("#delete", Checkbox).value
        config_mod.save(self.config)

        # Load whatever a previous run left behind for this folder, so files
        # that already finished never touch the browser again.
        self.state = DownloadState(self.dest_folder)

        table = self.query_one("#jobs", DataTable)
        table.clear()
        skipped = 0
        adopted = 0
        vanished: list[str] = []
        with self.jobs_lock:
            self.jobs.clear()
            for i, u in enumerate(urls):
                # A file that finished but was never recorded (crash, kill) is
                # already perfect on disk — claim it instead of refetching it.
                if self.state.adopt_if_finished(u):
                    adopted += 1
                gone = self.state.vanished(u)
                if gone:
                    vanished.append(gone)
                cached_name = self.state.known_filename(u)
                guess = cached_name or filename_from_fragment(u) or f"part{i+1:02d}.bin"
                job = Job(u, guess)
                if self.state.is_complete(u):
                    # Verified against the manifest AND the file on disk — no
                    # resolve, no aria2, no network at all.
                    job.state = "done"
                    entry = self.state.get(u) or {}
                    job.total = job.completed = int(entry.get("total") or 0)
                    skipped += 1
                self.jobs.append(job)
                table.add_row(
                    guess,
                    "Done (cached)" if job.state == "done" else "Queued",
                    "100%" if job.state == "done" else "",
                    format_bytes(job.total) if job.state == "done" and job.total else "",
                    "", "",
                    key=self._row_id(job),
                )

        pending_jobs = [j for j in self.jobs if j.state != "done"]
        if skipped:
            self._log(f"{skipped} file(s) already complete on disk — skipping resolve.")
        if adopted:
            msg = f"{adopted} file(s) had finished but were never recorded — adopted."
            self._log(msg)
            get_logger().info(msg)
        if vanished:
            # Almost always antivirus quarantining a finished archive. Say so,
            # rather than silently downloading it again and again.
            msg = (f"WARNING: {len(vanished)} file(s) completed earlier but are now "
                   f"MISSING from disk (antivirus quarantine?): {', '.join(vanished[:5])}")
            self._log(msg)
            get_logger().warning(msg)
        if not pending_jobs:
            # Nothing to fetch — don't spawn aria2 or the browser at all.
            self._log("Everything was already downloaded.")
            self.running = True          # so _all_finished tears down cleanly
            self._pending_urls = []
            self.query_one("#pending", DataTable).clear()
            self._all_finished(len(self.jobs), 0)
            return

        limit = self._read_limit_input()
        self.config["max_overall_dl_mbps"] = limit
        try:
            self.downloader = Downloader(
                connections_per_file=int(self.config.get("connections_per_file", 16)),
                concurrent_files=int(self.config.get("concurrent_files", 3)),
                max_overall_dl_mbps=limit,
            )
            self.downloader.start()
        except (Aria2NotFound, Aria2RpcError) as e:
            self._log(f"Could not start aria2c: {e}")
            self.downloader = None
            return

        # Resolve out-of-process: an in-process Camoufox call can hang forever
        # and wedge the resolver thread (it has, twice). See app/resolve_proc.py.
        self.resolver = SubprocessResolver()
        self._run_epoch += 1
        epoch = self._run_epoch
        self.shutdown_event.clear()
        # A cancelled batch leaves its Job objects sitting in the queue. Those
        # jobs are gone from self.jobs (cleared above) but the new resolver
        # thread would still pop them: a second Camoufox launch per file, a
        # second single-use token burnt, and an aria2 gid on a job _poll can
        # never see — an invisible download racing the real one for the same
        # filename in the same folder. Drop them before queueing the new batch.
        while not self.resolve_queue.empty():
            try:
                self.resolve_queue.get_nowait()
            except queue.Empty:
                break
        for job in pending_jobs:
            self.resolve_queue.put(job)
        self.resolver_thread = threading.Thread(
            target=self._resolver_loop, args=(epoch,), daemon=True)
        self.resolver_thread.start()

        self.running = True
        self.paused = False
        self._pending_urls = []
        self.query_one("#pending", DataTable).clear()
        self._set_controls(running=True)
        self._log(f"Downloading {len(pending_jobs)} file(s) → {self.dest_folder}")

    def _stale(self, epoch: int) -> bool:
        """True once this run is over — cancelled, finished, or superseded
        by a newer Start."""
        return self.shutdown_event.is_set() or epoch != self._run_epoch

    def _resolver_loop(self, epoch: int) -> None:
        while not self._stale(epoch):
            try:
                job = self.resolve_queue.get(timeout=0.5)
            except queue.Empty:
                continue
            if self._stale(epoch):
                # This job may belong to the *new* batch: _start drains the
                # queue and repopulates it while a retiring thread is still
                # parked in get(). Dropping it would strand a row on "Queued"
                # forever and _poll would never reach active == 0, hanging the
                # run. Put it back — the next _start drains before repopulating,
                # so a genuinely dead job cannot leak into the following batch.
                self.resolve_queue.put(job)
                return
            self.call_from_thread(self._set_status, job, "Resolving…")
            job.state = "resolving"
            # Cancel and batch teardown run on the main thread and drop both
            # backends while this thread is mid-flight, so bind them to locals
            # and re-check instead of dereferencing None mid-loop.
            resolver = self.resolver
            if resolver is None:
                return
            try:
                signed_url, filename = resolver.resolve(job.url)
            except Exception as e:
                # close()/stop() run on background threads during teardown and
                # can land *inside* the call above, not just before it. That
                # raises, and blaming the part for a download the user just
                # cancelled is the same mis-blame the guards above prevent.
                if self._stale(epoch):
                    return
                self._fail_job(job, "Resolve error", str(e))
                continue
            job.filename = filename
            if self.state is not None:
                # Heal a partial left fragmented by the old multi-connection
                # code so it can resume against this Range-broken CDN.
                healed = self.state.heal_fragmented_partial(job.url)
                if healed is not None:
                    self.call_from_thread(
                        self._log,
                        f"Healed fragmented partial {filename} — kept "
                        f"{format_bytes(healed)}, resuming sequentially.")
                # Drop a preallocated leftover whose .aria2 control file is
                # gone, otherwise aria2 would call the truncated file complete.
                ghost = self.state.clear_ghost_file(job.url)
                if ghost:
                    self.call_from_thread(
                        self._log,
                        f"Discarding unverifiable partial {ghost} (no resume data) "
                        "— redownloading it.")
                self.state.record_resolved(job.url, filename)
            self.call_from_thread(self._rename_row, job)

            # No pre-flight HEAD check here: aria2's own --continue=true
            # already detects an existing complete file (single plain
            # request, no data transfer) and resumes a partial one. A
            # separate requests.head() against the signed URL only adds an
            # extra hit against a token that may be single-use, which is
            # exactly what broke real downloads.
            # resolve() above can take minutes; by the time it returns the
            # main thread may have torn the downloader down (Cancel, or the
            # batch finishing). That isn't an "aria2 error" — there is simply
            # nothing left to add to, so stop rather than fail the part.
            downloader = self.downloader
            if self._stale(epoch) or downloader is None:
                return
            try:
                job.gid = downloader.add(signed_url, str(self.dest_folder), filename)
            except Exception as e:
                if self._stale(epoch):
                    return
                self._fail_job(job, "aria2 error", str(e))
                continue
            job.state = "downloading"
            self.call_from_thread(self._set_status, job, "Starting…")

    # ---- bandwidth limit --------------------------------------------------
    @staticmethod
    def _fmt_limit(mbps) -> str:
        try:
            v = float(mbps)
        except (TypeError, ValueError):
            v = 0.0
        return "0" if v <= 0 else (f"{v:g}")

    def _read_limit_input(self) -> float:
        """Parse the speed-limit box; blank/garbage/negative all mean 0 (∞)."""
        try:
            raw = self.query_one("#bwlimit", Input).value.strip()
        except Exception:
            return float(self.config.get("max_overall_dl_mbps", 0.0) or 0.0)
        try:
            v = float(raw)
        except ValueError:
            return 0.0
        return v if v > 0 else 0.0

    def _apply_limit_input(self) -> None:
        """Enter in the speed box applies the new cap live to a running download."""
        limit = self._read_limit_input()
        self.config["max_overall_dl_mbps"] = limit
        config_mod.save(self.config)
        if self.downloader is not None:
            self.downloader.set_overall_limit(limit)
        self._log(f"Speed limit set to {'unlimited' if limit <= 0 else f'{limit:g} MB/s'}.")

    def _mark_failed(self, job: Job, error: str) -> float:
        """Record a failure and compute its retry backoff. Pure state, no UI —
        safe to call from the resolver thread or the (main-thread) poll loop.

        Returns the retry delay in seconds, or -1.0 if the part has been given
        up on (state set to "failed"); callers use that to choose row text.
        """
        job.error = error
        # Progress since the last failure? Then the retry loop is working:
        # reset the streak so a slow/flaky part is retried indefinitely.
        if job.completed > job.best_completed:
            job.best_completed = job.completed
            job.fails_without_progress = 0
        else:
            job.fails_without_progress += 1
        job.retry_count += 1
        get_logger().warning(
            "Part failed: %s (attempt %d, %d/%d fails without progress) — %s",
            job.filename, job.retry_count, job.fails_without_progress,
            self.MAX_FAILS_WITHOUT_PROGRESS, error,
        )
        if job.fails_without_progress >= self.MAX_FAILS_WITHOUT_PROGRESS:
            get_logger().error(
                "Giving up on %s after %d attempts with no progress: %s",
                job.filename, job.retry_count, error,
            )
            job.state = "failed"   # terminal: poll loop never re-queues it
            return -1.0
        delay = min(
            self.RETRY_BASE_DELAY_S * (2 ** (job.retry_count - 1)),
            self.RETRY_MAX_DELAY_S,
        )
        job.next_retry_at = time.time() + delay
        job.state = "error"
        return delay

    def _fail_job(self, job: Job, status_text: str, error: str) -> None:
        """Resolver-thread failure path: mark failed, then hop to the main
        thread to update the row."""
        delay = self._mark_failed(job, error)
        if delay < 0:
            text = f"Failed after {job.retry_count} attempts — {status_text}"
        else:
            text = f"{status_text} — retrying in {delay:.0f}s"
        self.call_from_thread(self._set_status, job, text)

    def _requeue_failed_job(self, job: Job) -> None:
        """Its backoff has elapsed — send it back through resolve+download.
        Called from the (main-thread) poll loop, so it updates the row directly."""
        job.gid = None
        job.error = None
        job.state = "queued"
        self._set_status(job, f"Retrying (attempt {job.retry_count + 1})…")
        self.resolve_queue.put(job)

    def _rename_row(self, job: Job) -> None:
        table = self.query_one("#jobs", DataTable)
        try:
            table.update_cell(self._row_id(job), "file", job.filename)
        except Exception:
            pass

    # ---- poll -------------------------------------------------------------
    def _poll(self) -> None:
        if not self.running or self.downloader is None:
            return
        table = self.query_one("#jobs", DataTable)
        with self.jobs_lock:
            jobs = list(self.jobs)
        agg_speed = 0
        done = err = failed = 0
        total_c = total_s = 0
        now = time.time()
        for job in jobs:
            if job.state == "done":
                done += 1
                continue
            if job.state == "error":
                if now >= job.next_retry_at:
                    self._requeue_failed_job(job)
                else:
                    err += 1
                continue
            if job.state == "failed":
                failed += 1     # terminal: not retried, doesn't block finishing
                continue
            if job.gid is None:
                continue
            try:
                st = self.downloader.status(job.gid)
            except Exception:
                continue
            job.total = int(st.get("totalLength") or 0)
            job.completed = int(st.get("completedLength") or 0)
            job.speed = int(st.get("downloadSpeed") or 0)
            status = st.get("status", "")
            agg_speed += job.speed
            total_c += job.completed
            total_s += job.total
            rid = self._row_id(job)
            frac = (job.completed / job.total) if job.total else 0
            try:
                table.update_cell(rid, "pct", f"{frac*100:.1f}%" if job.total else "")
                table.update_cell(rid, "size",
                                  f"{format_bytes(job.completed)}/{format_bytes(job.total)}"
                                  if job.total else "")
                table.update_cell(rid, "speed", format_speed(job.speed) if job.speed else "")
                if job.speed > 0 and job.total:
                    eta = (job.total - job.completed) / job.speed
                    table.update_cell(rid, "eta", format_eta(eta))
                if status == "active":
                    table.update_cell(rid, "status", "Downloading")
                elif status == "complete":
                    job.state = "done"
                    if self.state is not None:
                        self.state.record_complete(job.url, job.filename, job.total)
                    table.update_cell(rid, "status", "Done")
                    table.update_cell(rid, "pct", "100%")
                    table.update_cell(rid, "eta", "")
                elif status == "error":
                    delay = self._mark_failed(job, st.get("errorMessage") or "aria2 error")
                    if delay < 0:
                        table.update_cell(rid, "status",
                                          f"Failed after {job.retry_count} attempts")
                    else:
                        table.update_cell(rid, "status", f"Error — retrying in {delay:.0f}s")
                elif status == "paused":
                    table.update_cell(rid, "status", "Paused")
                elif status == "waiting":
                    table.update_cell(rid, "status", "Waiting")
            except Exception:
                pass

        # Checkpoint in-flight progress occasionally. The .aria2 control files
        # carry the byte-level resume state; this just keeps our own view of
        # filenames and expected sizes fresh for the next run.
        now = time.time()
        if self.state is not None and now - self._last_checkpoint >= self.CHECKPOINT_EVERY_S:
            self._last_checkpoint = now
            for job in jobs:
                if job.state == "downloading" and job.total:
                    self.state.record_progress(
                        job.url, job.filename, job.total, job.completed)

        self.sub_title = (
            f"{done}/{len(jobs)} done"
            + (f" · {err} retrying" if err else "")
            + (f" · {failed} failed" if failed else "")
            + f" · {format_bytes(total_c)}/{format_bytes(total_s)}"
            + f" · {format_speed(agg_speed)}"
        )
        # Transient errors get re-queued above; terminal failures don't. Stop
        # once nothing is active AND nothing is still waiting out its backoff —
        # parts we've given up on don't block completion.
        active = len(jobs) - done - err - failed
        if active == 0 and err == 0 and self.running:
            self._all_finished(done, failed)

    def _all_finished(self, done: int, err: int) -> None:
        self.running = False
        self._set_controls(running=False)
        self._log(f"All done — {done} ok"
                  + (f", {err} failed" if err else "")
                  + f". Saved to {self.dest_folder}")
        get_logger().info("Batch finished: %d ok, %d failed", done, err)
        if err:
            with self.jobs_lock:
                stuck = [(j.filename, j.error or "unknown")
                         for j in self.jobs if j.state in ("error", "failed")]
            for name, why in stuck:
                get_logger().warning("  failed part: %s — %s", name, why)
                self._log(f"  failed: {name} — {why}")
        self.shutdown_event.set()
        if self.resolver is not None:
            threading.Thread(target=self.resolver.close, daemon=True).start()
            self.resolver = None
        d = self.downloader
        if d is not None:
            threading.Thread(target=d.stop, daemon=True).start()
            self.downloader = None

        if (done > 0 and self.dest_folder is not None
                and self.query_one("#autoextract", Checkbox).value
                and find_unrar() is not None):
            self._log("Extracting archives…")
            self._extract_thread = threading.Thread(
                target=self._extract_worker,
                args=(self.dest_folder, self.query_one("#delete", Checkbox).value),
                daemon=True)
            self._extract_thread.start()

    def _extract_worker(self, folder: Path, delete_after: bool) -> None:
        def on_start(name, i, total):
            self.call_from_thread(self._log, f"Extracting [{i}/{total}] {name}…")
        try:
            ok, fail = extract_all(folder, delete_after=delete_after, on_set_start=on_start)
            msg = f"Extraction complete: {ok} set(s) unpacked"
            if fail:
                msg += f", {fail} failed"
            self.call_from_thread(self._log, msg + f". Ready in {folder}")
        except Exception as e:
            log_exception("extracting archives", e)
            self.call_from_thread(self._log, f"Extraction error: {e}")

    # ---- pause / cancel ---------------------------------------------------
    def action_pause(self) -> None:
        if self.downloader is None:
            return
        try:
            if self.paused:
                self.downloader.unpause_all()
                self.paused = False
                self.query_one("#pause", Button).label = "Pause"
                self._log("Resumed.")
            else:
                self.downloader.pause_all()
                self.paused = True
                self.query_one("#pause", Button).label = "Resume"
                self._log("Paused.")
        except Exception as e:
            self._log(f"Pause/resume failed: {e}")

    def action_cancel(self) -> None:
        if not self.running:
            return
        self.shutdown_event.set()
        self.running = False
        self._set_controls(running=False)
        # Must actually stop the old aria2c process and close the old
        # Camoufox browser (not just null out our reference) — otherwise
        # each Cancel-then-Start cycle leaks another aria2c + browser pair
        # that keeps running in the background forever.
        if self.resolver is not None:
            threading.Thread(target=self.resolver.close, daemon=True).start()
            self.resolver = None
        d = self.downloader
        if d is not None:
            threading.Thread(target=d.stop, daemon=True).start()
            self.downloader = None
        self._log("Cancelled. Partial files kept — paste the same links to resume.")

    def _set_controls(self, running: bool) -> None:
        self.query_one("#fetch", Button).disabled = running
        self.query_one("#start", Button).disabled = running
        self.query_one("#remove", Button).disabled = running
        self.query_one("#clear", Button).disabled = running
        self.query_one("#pause", Button).disabled = not running
        self.query_one("#cancel", Button).disabled = not running

    def action_quit(self) -> None:
        # close()/stop() stay synchronous here, unlike cancel: killing the
        # browser tree has to finish before the interpreter exits, or we orphan
        # a Camoufox. That can cost a moment on the way out; a background
        # thread would be a daemon and die mid-kill, leaving the orphan.
        self.shutdown_event.set()
        try:
            if self.downloader is not None:
                self.downloader.stop()
        except Exception:
            pass
        try:
            if self.resolver is not None:
                self.resolver.close()
        except Exception:
            pass
        try:
            # Synchronous, like the resolver close: the kill has to finish
            # before the interpreter exits or the orphan is what we get.
            if self.scraper is not None:
                self.scraper.close()
        except Exception:
            pass
        self.exit()


def main() -> int:
    try:
        setup_logging()
    except Exception:
        pass
    FitFastTUI().run()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
