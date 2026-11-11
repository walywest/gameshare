"""First-run setup: make sure the Camoufox stealth browser is installed.

The browser (~a few hundred MB) is NOT bundled in the release — it's fetched
once on first launch into the per-user Camoufox cache. Subsequent launches see
it and skip straight to the app.
"""
from __future__ import annotations

import re
import shutil
from pathlib import Path


def browser_ready() -> bool:
    """True if the Camoufox browser is already installed for this user."""
    try:
        from camoufox.pkgman import installed_verstr
        installed_verstr()
        return True
    except Exception:
        return False


# Cloudflare's interactive Turnstile will not vouch for a Firefox build no real
# user runs. A months-old bundle resolves nothing while looking perfectly
# healthy — every request 200, the challenge simply never satisfied — so say so
# loudly rather than let it present as "the site broke".
STALE_BROWSER_DAYS = 180


def _browser_build() -> tuple[str | None, int | None]:
    """(version, age_in_days) of the browser that will actually launch.

    Both come from the same application.ini next to the binary launch_path()
    resolves to, so the version we name can never disagree with the age we
    measured. BuildID (YYYYMMDDHHMMSS) is the build date, not the download date.
    """
    from datetime import datetime
    version = age = None
    try:
        from camoufox.pkgman import launch_path
        from pathlib import Path
        ini = Path(launch_path()).parent / "application.ini"
        for line in ini.read_text(errors="replace").splitlines():
            if line.startswith("Version="):
                version = line.split("=", 1)[1].strip()
            elif line.startswith("BuildID="):
                stamp = line.split("=", 1)[1].strip()[:8]
                age = (datetime.now() - datetime.strptime(stamp, "%Y%m%d")).days
    except Exception:
        pass
    return version, age


def browser_age_days() -> int | None:
    """Age of the installed browser build in days, or None if undeterminable."""
    return _browser_build()[1]


def browser_staleness_warning() -> str | None:
    """A line to show the user when the installed browser is too old to pass
    Cloudflare, or None when it looks current."""
    ver, age = _browser_build()
    if age is None or age < STALE_BROWSER_DAYS:
        return None
    return (f"Stealth browser {ver or 'unknown'} is {age} days old — Cloudflare will likely "
            "refuse to issue a challenge token to a build this stale, and every "
            "resolve will fail. Run: camoufox sync && camoufox fetch")


def _clear_partial_installs() -> None:
    """Remove version folders an interrupted install left without version.json.

    camoufox creates the folder before downloading; kill it mid-download and
    the empty folder stays behind, looking like an install to anyone browsing
    the cache while camoufox itself (rightly) ignores it.
    """
    import shutil
    try:
        from camoufox.multiversion import BROWSERS_DIR
        for d in BROWSERS_DIR.glob("*/*"):
            if d.is_dir() and not (d / "version.json").exists():
                shutil.rmtree(d, ignore_errors=True)
    except Exception:
        pass


# aria2c's periodic summary line: "[#1a2b3c 120MiB/700MiB(17%) CN:16 DL:31MiB ETA:18s]"
_ARIA2_PROGRESS_RE = re.compile(
    r"\[#\w+ (?P<done>[\d.]+\w+)/(?P<total>[\d.]+\w+)\((?P<pct>\d+)%\)"
    r".*?DL:(?P<speed>[\d.]+\w+)(?:.*?ETA:(?P<eta>\w+))?")


def _aria2_download(url: str, say) -> Path:
    """Fetch ``url`` into the camoufox cache with aria2c and return the file.

    camoufox's own downloader is one connection with no read timeout, so a
    stalled socket hangs it forever and every retry restarts from byte zero.
    aria2c splits the ~1.2 GB zip across 16 connections, gives up on dead
    ones, and ``-c`` resumes whatever an earlier attempt already pulled.
    """
    import subprocess
    from camoufox.pkgman import INSTALL_DIR

    aria2c = shutil.which("aria2c")
    if not aria2c:
        raise FileNotFoundError("aria2c not on PATH")
    dl_dir = Path(INSTALL_DIR) / "downloads"
    dl_dir.mkdir(parents=True, exist_ok=True)
    name = url.rsplit("/", 1)[-1]
    proc = subprocess.Popen(
        [aria2c, url, "--dir", str(dl_dir), "--out", name,
         "-c", "-x16", "-s16", "-k1M", "--file-allocation=none",
         "--max-tries=0", "--retry-wait=3", "--timeout=30", "--connect-timeout=15",
         "--lowest-speed-limit=20K", "--auto-file-renaming=false",
         "--allow-overwrite=true", "--show-console-readout=false",
         "--summary-interval=3", "--console-log-level=warn",
         "--download-result=hide"],
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL,
        text=True, errors="replace")
    tail: list[str] = []
    assert proc.stdout is not None
    for line in proc.stdout:
        m = _ARIA2_PROGRESS_RE.search(line)
        if m:
            say(f"  {m['pct']}%  {m['done']} / {m['total']}  at {m['speed']}/s"
                + (f"  ETA {m['eta']}" if m['eta'] else ""))
        elif line.strip():
            tail = (tail + [line.strip()])[-5:]
    if proc.wait() != 0:
        raise RuntimeError("aria2c failed downloading the browser: "
                           + (" | ".join(tail) or f"exit {proc.returncode}"))
    return dl_dir / name


def ensure_browser(status_cb=None) -> None:
    """Download + install the Camoufox browser if missing or outdated.

    ``status_cb(str)`` receives human-readable progress lines. Raises on hard
    failure (caller should surface it). Run this out of process (see
    ``install_in_subprocess``): camoufox prints rich progress bars and can
    stop at an interactive prompt, neither of which a TUI can host.
    """
    def say(msg: str) -> None:
        if status_cb:
            try:
                status_cb(msg)
            except Exception:
                pass

    from camoufox.__main__ import CamoufoxUpdate

    _clear_partial_installs()
    upd = CamoufoxUpdate()
    if upd.is_updated_needed():
        cached: list[Path] = []
        if shutil.which("aria2c"):
            def download_file(file, url):
                say(f"Downloading stealth browser v{upd.verstr} with aria2c "
                    "(resumes if interrupted)…")
                path = _aria2_download(url, say)
                cached.append(path)
                with open(path, "rb") as src:
                    shutil.copyfileobj(src, file, 1024 * 1024)
                file.flush()
                file.seek(0)
                return file
            upd.download_file = download_file
        else:
            say("aria2c not found — falling back to camoufox's slower single-stream download.")
        say(f"Installing stealth browser v{upd.verstr} (one-time, ~1.2 GB)…")
        # The pinned build is the one this camoufox release was tested with;
        # its prerelease flag must not turn into a y/N prompt nobody can answer.
        upd.update(i_know_what_im_doing=True)
        if not browser_ready():
            raise RuntimeError("camoufox reported success but the browser still isn't usable.")
        for path in cached:
            path.unlink(missing_ok=True)
        say("Browser installed.")
    else:
        say("Browser already installed.")

    # GeoIP DB (optional; flaky download — never fatal)
    try:
        from camoufox.locale import ALLOW_GEOIP, download_mmdb
        if ALLOW_GEOIP:
            say("Fetching GeoIP database…")
            download_mmdb()
    except Exception:
        pass

    # Default addons (best effort)
    try:
        from camoufox.addons import DefaultAddons, maybe_download_addons
        say("Fetching browser add-ons…")
        maybe_download_addons(list(DefaultAddons))
    except Exception:
        pass

    say("Setup complete.")


_STATUS_PREFIX = "@@status "


def install_in_subprocess(status_cb) -> None:
    """Run ensure_browser() in a child process, relaying its status lines.

    stdin is /dev/null so any prompt camoufox raises fails at once instead of
    waiting forever on a keyboard the TUI owns; its rich output goes to the
    log rather than over the TUI.
    """
    import subprocess
    import sys
    repo_root = Path(__file__).resolve().parent.parent
    proc = subprocess.Popen(
        [sys.executable, "-m", "app.bootstrap"], cwd=str(repo_root),
        stdout=subprocess.PIPE, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL,
        text=True, errors="replace")
    tail: list[str] = []
    assert proc.stdout is not None
    for line in proc.stdout:
        line = line.rstrip()
        if line.startswith(_STATUS_PREFIX):
            status_cb(line[len(_STATUS_PREFIX):])
        elif line.strip():
            tail = (tail + [line.strip()])[-8:]
    if proc.wait() != 0:
        raise RuntimeError(" | ".join(tail) or f"installer exited {proc.returncode}")


if __name__ == "__main__":
    import sys
    try:
        ensure_browser(lambda m: print(_STATUS_PREFIX + m, flush=True))
    except Exception as e:
        print(f"{type(e).__name__}: {e}", flush=True)
        sys.exit(1)
