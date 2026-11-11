"""On-disk download state, cached next to the files themselves.

Why this exists
---------------
Resolving a fuckingfast.co link is *expensive*: a headless Firefox page load,
a Cloudflare challenge, two button clicks — 15-35s per link. Without a cache,
resuming a 40-part repack where 39 parts already finished costs ~20 minutes of
browser work just to rediscover that there is nothing to do.

Worse, on-disk file size is not evidence of completeness. aria2 runs with
``--file-allocation=falloc``, so every file is preallocated to its *full* size
the moment the download starts. A half-finished 4 GB part looks byte-identical
in size to a finished one. The only real progress record is aria2's own
``.aria2`` control file — and if that is gone (cancel, crash, kill), aria2 sees
a full-size file with ``--continue=true`` and declares it complete. That is how
truncated parts get marked "Done" and blow up at extraction time.

So we keep our own manifest, in the spirit of aria2's control files: one JSON
file per destination folder recording, per *source* URL (the stable key — the
signed URL expires), the resolved filename, the expected total size, and
whether we ever actually saw it finish.

A file counts as complete only when all of these hold:
  * the manifest says we saw it complete, and
  * the file exists, and
  * its size matches the recorded total exactly, and
  * no ``.aria2`` control file remains beside it (that would mean aria2 still
    considers the download unfinished).

Anything short of that is re-resolved and handed back to aria2, which resumes
from the control file if it is there and restarts the part if it is not.
"""
from __future__ import annotations

import json
import os
import struct
import tempfile
import threading
from pathlib import Path

MANIFEST_NAME = ".fitfast.json"
_FORMAT_VERSION = 1


def _parse_aria2_control(raw: bytes) -> tuple[int, int, bytes]:
    """Parse an aria2 (v1) ``.aria2`` control file.

    Layout: version(u16), extension(u32), infoHashLength(u32) + infoHash,
    pieceLength(u32), totalLength(u64), uploadLength(u64),
    bitfieldLength(u32) + bitfield, ... (rest unused here).

    Returns (piece_length, total_length, bitfield_bytes).
    """
    o = 0
    o += 2                                             # version
    o += 4                                             # extension flags
    (ih_len,) = struct.unpack_from(">I", raw, o); o += 4
    o += ih_len                                        # info hash (0 for HTTP)
    (piece_len,) = struct.unpack_from(">I", raw, o); o += 4
    (total,) = struct.unpack_from(">Q", raw, o); o += 8
    o += 8                                             # upload length
    (bf_len,) = struct.unpack_from(">I", raw, o); o += 4
    bitfield = raw[o:o + bf_len]
    return piece_len, total, bitfield


class DownloadState:
    """Manifest of per-file download state for one destination folder.

    Thread-safe: the resolver thread and the UI poll loop both touch this.
    """

    def __init__(self, folder: Path) -> None:
        self.folder = Path(folder)
        self.path = self.folder / MANIFEST_NAME
        self._lock = threading.RLock()
        # Serialises whole writes. The entry lock only guards the snapshot;
        # without this two savers can replace out of order and the manifest
        # regresses to an older view — which only shows up if the process
        # dies before the next save, i.e. exactly the crash the manifest
        # exists to survive. Separate from _lock so readers don't block on
        # the fsync.
        self._write_lock = threading.Lock()
        self._entries: dict[str, dict] = {}
        self._load()

    # ---- persistence ------------------------------------------------------
    def _load(self) -> None:
        try:
            raw = json.loads(self.path.read_text(encoding="utf-8"))
        except (OSError, ValueError):
            return  # missing or corrupt manifest is not fatal — start empty
        if not isinstance(raw, dict) or raw.get("version") != _FORMAT_VERSION:
            return
        files = raw.get("files")
        if isinstance(files, dict):
            self._entries = {k: v for k, v in files.items() if isinstance(v, dict)}

    def save(self) -> None:
        """Write the manifest atomically so a crash mid-write can't corrupt it."""
        with self._write_lock:
            with self._lock:
                payload = {
                    "version": _FORMAT_VERSION,
                    "files": dict(self._entries),
                }
            try:
                self.folder.mkdir(parents=True, exist_ok=True)
                fd, tmp = tempfile.mkstemp(
                    dir=str(self.folder), prefix=".fitfast-", suffix=".tmp")
                try:
                    with os.fdopen(fd, "w", encoding="utf-8") as fh:
                        json.dump(payload, fh, indent=2)
                        fh.flush()
                        os.fsync(fh.fileno())
                    os.replace(tmp, self.path)
                except BaseException:
                    try:
                        os.unlink(tmp)
                    except OSError:
                        pass
                    raise
            except OSError:
                pass  # a read-only destination shouldn't kill the download

    # ---- queries ----------------------------------------------------------
    def get(self, url: str) -> dict | None:
        with self._lock:
            entry = self._entries.get(url)
            return dict(entry) if entry else None

    def known_filename(self, url: str) -> str | None:
        entry = self.get(url)
        name = entry.get("filename") if entry else None
        return name or None

    def is_complete(self, url: str) -> bool:
        """True only if the manifest *and* the disk agree this file is finished.

        Deliberately strict — see the module docstring. A false positive here
        means handing a truncated archive to the extractor.
        """
        entry = self.get(url)
        if not entry or not entry.get("complete"):
            return False
        name = entry.get("filename")
        total = entry.get("total")
        if not name or not isinstance(total, int) or total <= 0:
            return False
        target = self.folder / name
        # A leftover .aria2 control file means aria2 still has unfinished
        # business with this file, whatever our manifest thinks.
        if target.with_name(target.name + ".aria2").exists():
            return False
        try:
            return target.stat().st_size == total
        except OSError:
            return False

    # ---- updates ----------------------------------------------------------
    def record_resolved(self, url: str, filename: str) -> None:
        """Remember the filename a URL resolved to, before bytes are moved."""
        with self._lock:
            entry = self._entries.setdefault(url, {})
            entry["filename"] = filename
            entry.setdefault("complete", False)
            entry.setdefault("total", 0)
        self.save()

    def record_progress(self, url: str, filename: str, total: int, completed: int) -> None:
        """Checkpoint in-flight progress. Never sets ``complete``."""
        with self._lock:
            entry = self._entries.setdefault(url, {})
            entry["filename"] = filename
            if total > 0:
                entry["total"] = int(total)
            entry["completed"] = int(completed)
            entry.setdefault("complete", False)
        self.save()

    def record_complete(self, url: str, filename: str, total: int) -> None:
        """Mark a file finished. Only call when aria2 reported it complete."""
        with self._lock:
            entry = self._entries.setdefault(url, {})
            entry["filename"] = filename
            if total > 0:
                entry["total"] = int(total)
            else:
                # aria2 didn't give us a length; fall back to the real file so
                # is_complete() has something exact to compare against.
                try:
                    entry["total"] = (self.folder / filename).stat().st_size
                except OSError:
                    entry["total"] = 0
            entry["completed"] = entry["total"]
            entry["complete"] = True
        self.save()

    def adopt_if_finished(self, url: str) -> int | None:
        """Record a file that finished on disk but was never marked complete.

        A download can finish and the run end before we get to write the
        manifest — a crash, a kill, or the app wedging elsewhere. The bytes are
        on disk and correct; only our bookkeeping is behind. Without this we
        re-resolve and re-download a file that is already perfect (and, worse,
        ``clear_ghost_file`` used to *delete* it).

        Evidence required: the file is exactly the expected size and aria2 left
        no control file beside it. Since downloads no longer preallocate
        (``--file-allocation=none``), reaching full length means the bytes were
        actually transferred.

        Returns the adopted size, or None if there was nothing to adopt.
        """
        entry = self.get(url)
        if not entry or entry.get("complete"):
            return None
        name = entry.get("filename")
        total = entry.get("total")
        if not name or not isinstance(total, int) or total <= 0:
            return None
        target = self.folder / name
        if target.with_name(target.name + ".aria2").exists():
            return None  # aria2 still considers it unfinished
        try:
            if target.stat().st_size != total:
                return None
        except OSError:
            return None
        self.record_complete(url, name, total)
        return total

    def vanished(self, url: str) -> str | None:
        """Name of a file the manifest says is complete but that is now gone.

        Seen in the wild: antivirus quarantines a finished archive seconds
        after it lands, so the download "succeeds" and the file silently is not
        there. Callers surface this instead of just re-queueing it forever.
        """
        entry = self.get(url)
        if not entry or not entry.get("complete"):
            return None
        name = entry.get("filename")
        if not name:
            return None
        return None if (self.folder / name).exists() else name

    def clear_ghost_file(self, url: str) -> str | None:
        """Delete a preallocated-but-unfinished leftover we know we created.

        Legacy hazard: downloads used to run with ``--file-allocation=falloc``,
        which sized a file to its full length the moment it started, so an
        interrupted part was indistinguishable from a finished one *by size*.
        With the ``.aria2`` control file gone (kill -9, disk full, manual
        cleanup), aria2 saw a full-size file with ``--continue=true`` and
        declared it complete without transferring a byte — a truncated archive
        that only failed at extraction time.

        Preallocation is now off, so new downloads only reach full length by
        actually transferring the bytes. That makes "full size, no control
        file" the signature of a *finished* download far more often than a
        ghost — deleting on that alone destroys good data (see
        :meth:`adopt_if_finished`, which runs first). So we now additionally
        require positive evidence of interruption: a checkpoint that recorded
        genuinely fewer bytes than the file's full length.

        A file with no manifest entry is still left strictly alone.

        Returns the name of the file removed, or None if nothing was touched.
        """
        entry = self.get(url)
        if not entry or entry.get("complete"):
            return None  # unknown file, or genuinely finished — don't touch it
        name = entry.get("filename")
        if not name:
            return None
        target = self.folder / name
        if target.with_name(target.name + ".aria2").exists():
            return None  # control file present: aria2 can resume properly
        try:
            if not target.exists():
                return None
            size = target.stat().st_size
        except OSError:
            return None
        # A *smaller-than-full* file with no control file is a legitimate
        # sequential partial (e.g. one healed by heal_fragmented_partial):
        # aria2 resumes it with an open-ended request from its current size.
        total = entry.get("total")
        if not isinstance(total, int) or total <= 0 or size < total:
            return None
        # Full size. Only a recorded checkpoint proving we never finished makes
        # this a ghost; otherwise assume the bytes are real and keep them.
        completed = entry.get("completed")
        if not isinstance(completed, int) or completed >= total:
            return None
        try:
            target.unlink()
        except OSError:
            return None
        with self._lock:
            e = self._entries.get(url)
            if e is not None:
                e["completed"] = 0
        self.save()
        return name

    def heal_fragmented_partial(self, url: str) -> int | None:
        """Repair a partial left with interior gaps by an older multi-connection
        download, so it can resume against a Range-broken CDN.

        fuckingfast's CDN ignores the *end* of a bounded HTTP Range request
        (it always answers to EOF), so aria2 can only ever download a file as
        one sequential stream from its contiguous front. A partial written by
        the old 16-connection code has completed pieces scattered with holes in
        between; aria2 must issue bounded requests to fill those holes, every
        one of which the CDN botches -> "Invalid range header" -> the part can
        never finish. (A partial with no interior holes — everything done from
        byte 0 up to some offset — resumes fine and is left untouched.)

        The fix: truncate the file back to its contiguous-from-start prefix,
        which is known-good, and drop the ``.aria2`` control file. aria2 then
        resumes with a single open-ended request from that offset and downloads
        the rest sequentially. Returns the number of bytes kept, or None if
        there was nothing to heal (no partial, or already gap-free).
        """
        entry = self.get(url)
        if not entry or entry.get("complete"):
            return None
        name = entry.get("filename")
        if not name:
            return None
        target = self.folder / name
        ctrl = target.with_name(target.name + ".aria2")
        try:
            raw = ctrl.read_bytes()
        except OSError:
            return None  # no control file: nothing to inspect or heal
        try:
            piece_len, total, bitfield = _parse_aria2_control(raw)
        except Exception:
            return None
        bits = "".join(f"{b:08b}" for b in bitfield)
        num_pieces = (total + piece_len - 1) // piece_len
        bits = bits[:num_pieces]
        first_gap = bits.find("0")
        if first_gap < 0:
            return None  # all pieces present — not fragmented
        if "1" not in bits[first_gap:]:
            return None  # contiguous prefix only, no interior holes: resumes fine
        # Interior hole present -> poisoned partial. Keep only the good prefix.
        keep = min(first_gap * piece_len, total)
        try:
            with open(target, "r+b") as fh:
                fh.truncate(keep)
            ctrl.unlink()
        except OSError:
            return None
        with self._lock:
            e = self._entries.get(url)
            if e is not None:
                e["completed"] = keep
                e["complete"] = False
        self.save()
        return keep

    def forget(self, url: str) -> None:
        with self._lock:
            self._entries.pop(url, None)
        self.save()
