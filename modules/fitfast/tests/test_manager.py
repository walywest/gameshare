"""Manager: queue persistence, resume and retry, with aria2 and the resolver faked."""
import json
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

from app import manager as M

LINKS = [f"https://fuckingfast.co/abc{i}#Game_Name--fitgirl-repacks.site--.part0{i}.rar" for i in (1, 2)]


class FakeDownloader:
    instances = []

    def __init__(self, **kw):
        self.added = {}
        self.fail = set()
        FakeDownloader.instances.append(self)

    def start(self): pass
    def stop(self): self.stopped = True
    def set_overall_limit(self, mbps): self.limit = mbps
    def pause(self, gid): pass
    def unpause(self, gid): pass
    def remove(self, gid): self.added.pop(gid, None)

    def add(self, url, out_dir, filename):
        gid = f"g{len(self.added)}"
        Path(out_dir, filename).write_bytes(b"x" * 10)
        self.added[gid] = filename
        return gid

    def status(self, gid):
        if self.added.get(gid) in self.fail:
            return {"status": "error", "errorMessage": "boom", "totalLength": "10", "completedLength": "0"}
        return {"status": "complete", "totalLength": "10", "completedLength": "10", "downloadSpeed": "0"}


class FakeResolver:
    calls = 0

    def resolve(self, url):
        FakeResolver.calls += 1
        return "https://dl.example/" + url[-8:], url.split("#", 1)[1]

    def close(self): pass


def wait(cond, timeout=10):
    end = time.time() + timeout
    while time.time() < end:
        if cond():
            return True
        time.sleep(0.05)
    return False


@mock.patch.object(M, "Downloader", FakeDownloader)
@mock.patch.object(M, "SubprocessResolver", FakeResolver)
@mock.patch.object(M, "find_unrar", lambda: None)
@mock.patch.object(M, "TICK_S", 0.05)
class ManagerTest(unittest.TestCase):
    def setUp(self):
        self.root = Path(tempfile.mkdtemp())
        FakeResolver.calls = 0

    def test_download_persist_and_resume(self):
        m = M.Manager(self.root)
        m.start()
        g = m.add(None, LINKS, extract=False)
        self.assertTrue(wait(lambda: m.get(g.id).state == "done"), m.snapshot())
        self.assertEqual(FakeResolver.calls, 2)
        self.assertIn("FitFast/", g.path())
        self.assertTrue((self.root / g.path() / ".fitfast.json").exists())
        m.shutdown()
        saved = json.loads((self.root / ".fitfast" / "queue.json").read_text())
        self.assertEqual(saved["games"][0]["state"], "done")

        # A finished game reloads as done and resolves nothing again.
        m2 = M.Manager(self.root)
        m2.start()
        time.sleep(0.3)
        self.assertEqual(m2.get(g.id).state, "done")
        self.assertEqual(FakeResolver.calls, 2)
        m2.shutdown()

    def test_interrupted_game_resumes_without_redownloading(self):
        m = M.Manager(self.root)
        g = m.add(None, LINKS, extract=False)
        m.save()                      # as if the server died before anything ran
        m2 = M.Manager(self.root)
        m2.start()
        self.assertTrue(wait(lambda: m2.get(g.id).state == "done"))
        m2.shutdown()
        # Manifest says both parts are whole: a third start resolves nothing.
        data = json.loads((self.root / ".fitfast" / "queue.json").read_text())
        data["games"][0]["state"] = "queued"
        for p in data["games"][0]["parts"]:
            p["state"] = "queued"
        (self.root / ".fitfast" / "queue.json").write_text(json.dumps(data))
        calls = FakeResolver.calls
        m3 = M.Manager(self.root)
        m3.start()
        self.assertTrue(wait(lambda: m3.get(g.id).state == "done"))
        self.assertEqual(FakeResolver.calls, calls)
        m3.shutdown()

    def test_failing_part_gives_up_then_retry(self):
        with mock.patch.object(M, "RETRY_BASE_DELAY_S", 0.01), mock.patch.object(M, "RETRY_MAX_DELAY_S", 0.01):
            m = M.Manager(self.root)
            m.start()
            g = m.add(None, LINKS, extract=False)
            self.assertTrue(wait(lambda: FakeDownloader.instances and m.downloader is not None))
            m.downloader.fail.add(LINKS[1].split("#", 1)[1])
            self.assertTrue(wait(lambda: m.get(g.id).state == "failed"), m.snapshot())
            self.assertIn("Retry", m.get(g.id).error)
            self.assertEqual(m.busy_paths(), [])
            m.downloader.fail.clear() if m.downloader else None
            self.assertGreater(m.retry(g.id), 0)
            self.assertTrue(wait(lambda: m.get(g.id).state == "done"), m.snapshot())
            m.shutdown()

    def test_pause_and_remove(self):
        m = M.Manager(self.root)
        g = m.add(None, LINKS, extract=False)
        self.assertTrue(m.pause(g.id))
        m.start()
        time.sleep(0.3)
        self.assertEqual(m.get(g.id).state, "paused")
        self.assertEqual(FakeResolver.calls, 0)
        self.assertTrue(m.resume(g.id))
        self.assertTrue(wait(lambda: m.get(g.id).state == "done"))
        self.assertTrue(m.remove(g.id))
        self.assertIsNone(m.get(g.id))
        m.shutdown()


if __name__ == "__main__":
    unittest.main()
