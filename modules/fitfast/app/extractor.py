"""Post-download RAR extraction via a system unrar / rar / unar.

FitGirl repacks arrive as multi-volume RAR sets (``*.part01.rar`` …). Pointing
the extractor at the first volume automatically joins the rest, so we just
locate each first volume in the folder and extract it.
"""
from __future__ import annotations
import os
import re
import shutil
import subprocess
import threading
from pathlib import Path

if __package__ in (None, ""):
    from log import get_logger
else:
    from .log import get_logger

# First volume of a multi-part set: part01 / part001 / part1 (with any digits
# that reduce to 1). Also a plain single-volume .rar with no .partNN.
_FIRST_VOL_RE = re.compile(r"\.part0*1\.rar$", re.IGNORECASE)
_ANY_PART_RE = re.compile(r"\.part\d+\.rar$", re.IGNORECASE)
_PERCENT_RE = re.compile(rb"(\d{1,3})%")


class ExtractError(Exception):
    pass


def find_unrar() -> Path | None:
    # A RAR extractor on PATH. `unrar`/`rar` take UnRAR-style args; `unar`
    # (The Unarchiver, free, handles RAR5 + multi-volume) is the Nix-friendly
    # fallback and is invoked with its own flags below.
    for exe in ("unrar", "rar", "unar"):
        found = shutil.which(exe)
        if found:
            return Path(found)
    return None


def find_first_volumes(folder: str | Path) -> list[Path]:
    """Return the first-volume archive of every RAR set in ``folder``."""
    folder = Path(folder)
    if not folder.is_dir():
        return []
    rars = sorted(folder.glob("*.rar"))
    firsts: list[Path] = []
    for r in rars:
        name = r.name
        if _FIRST_VOL_RE.search(name):
            firsts.append(r)
        elif not _ANY_PART_RE.search(name):
            # A .rar with no .partNN at all == a single-volume archive.
            firsts.append(r)
    return firsts


def extract_archive(
    archive: str | Path,
    dest_dir: str | Path,
    on_progress=None,
    unrar: Path | None = None,
) -> None:
    """Extract one archive (and its sibling volumes) into ``dest_dir``.

    ``on_progress`` is called with an int percentage 0-100 as UnRAR reports it.
    Raises ExtractError on non-zero exit.
    """
    unrar = unrar or find_unrar()
    if unrar is None:
        raise ExtractError(
            "No unrar/unar found. Install `unar` (or `unrar`), or extract the "
            ".rar files manually."
        )
    archive = Path(archive)
    dest_dir = Path(dest_dir)
    dest_dir.mkdir(parents=True, exist_ok=True)

    if "unar" in unrar.name.lower():
        # The Unarchiver: -f overwrite, -o output dir, -q quiet. It auto-joins
        # sibling volumes when pointed at the first .partNN. No percentage stream.
        args = [str(unrar), "-force-overwrite", "-quiet",
                "-output-directory", str(dest_dir), str(archive)]
    else:
        # UnRAR/RAR: x = extract with full paths, -o+ = overwrite, -y = assume
        # yes, trailing separator on dest tells it the target is a directory.
        args = [str(unrar), "x", "-o+", "-y", str(archive), str(dest_dir) + os.sep]
    proc = subprocess.Popen(
        args,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
    )

    # UnRAR prints "\b\b\b\b45%" style updates; read raw bytes and pull the
    # last percentage we see.
    last_pct = -1
    buf = b""
    assert proc.stdout is not None
    while True:
        chunk = proc.stdout.read(256)
        if not chunk:
            break
        buf += chunk
        for m in _PERCENT_RE.finditer(buf):
            pct = int(m.group(1))
            if 0 <= pct <= 100 and pct != last_pct:
                last_pct = pct
                if on_progress:
                    try:
                        on_progress(pct)
                    except Exception:
                        pass
        buf = buf[-64:]  # keep tail in case a % straddles a read boundary

    ret = proc.wait()
    if ret != 0:
        raise ExtractError(f"UnRAR exited with code {ret} on {archive.name}")


def extract_all(
    folder: str | Path,
    delete_after: bool = False,
    on_set_start=None,
    on_progress=None,
    on_set_done=None,
) -> tuple[int, int]:
    """Extract every RAR set found in ``folder``.

    Returns (succeeded, failed). Callbacks:
      on_set_start(archive_name, index, total)
      on_progress(percent)
      on_set_done(archive_name, ok, error_or_None)
    """
    firsts = find_first_volumes(folder)
    total = len(firsts)
    ok_count = 0
    fail_count = 0
    for i, archive in enumerate(firsts, 1):
        if on_set_start:
            on_set_start(archive.name, i, total)
        try:
            extract_archive(archive, folder, on_progress=on_progress)
            ok_count += 1
            if on_set_done:
                on_set_done(archive.name, True, None)
            if delete_after:
                _delete_set(archive)
        except ExtractError as e:
            fail_count += 1
            if on_set_done:
                on_set_done(archive.name, False, str(e))
    return ok_count, fail_count


def _unlink(path: Path) -> None:
    """Delete one archive, saying so if it fails — this path exists to reclaim
    tens of GB, and a silent failure looks like the space simply never came
    back."""
    try:
        path.unlink()
    except OSError as e:
        get_logger().warning("Could not delete %s — %s", path.name, e)


def _delete_set(first_volume: Path) -> None:
    """Delete every volume belonging to a set once it's safely extracted."""
    folder = first_volume.parent
    # part01.rar -> match part\d+.rar with same prefix; else just the file.
    base = re.sub(r"\.part0*1\.rar$", "", first_volume.name, flags=re.IGNORECASE)
    if base == first_volume.name:
        _unlink(first_volume)
        return
    # Match by exact prefix, never by glob. Repack names routinely contain
    # [brackets], which a glob reads as a character class: the set's own
    # volumes then fail to match their own pattern, while unrelated archives
    # differing only at that position do match and get deleted. This is the
    # one path that removes the user's files, so it is exact by construction.
    prefix = base + ".part"
    for f in folder.iterdir():
        name = f.name
        if not name.lower().endswith(".rar"):
            continue
        if not name[:len(prefix)].lower() == prefix.lower():
            continue
        if name[len(prefix):-4].isdigit() and f.is_file():
            _unlink(f)
