"""Host plugins and the link grabber, against canned pages: no network."""
import unittest
from unittest import mock

from app import hosts as H
from app import web as W
from app.resolver import DeadLinkError, ResolveError

PAGE = """<!DOCTYPE html><html><head><title>Some.Game-SteamRIP.com.rar</title></head><body>
<div class="max-w-2xl mx-auto mt-7"> <p class="file-name">Some.Game &amp; DLC-SteamRIP.com.rar</p>
<div class="download-row"> <a class="download-btn gay-button" hx-get="/abcdefghij12/download?t=TOKEN.sig&amp;x=1" rel="noopener">Download File <span class="size">5.0GB</span></a>
<a class="link-button gay-button" hx-get="/abcdefghij12/download?t=TOKEN.sig&amp;alt=true">Alternate</a>
</div></div></body></html>"""


class Resp:
    def __init__(self, status=200, headers=None, text=""):
        self.status_code, self.headers, self.text = status, headers or {}, text

    def close(self):
        pass


class FakeSession:
    """Answers like BuzzHeavier for an uploader who only lets in visitors from its site."""

    def __init__(self, page_status=200, button=None):
        self.calls = []
        self.page_status, self.button = page_status, button

    def get(self, url, headers=None, **kw):
        headers = headers or {}
        self.calls.append((url, dict(headers)))
        if "/download?" in url:
            return self.button or Resp(204, {"hx-redirect": "https://ts.bzzhr.to/d/abcdefghij12?v=SIGNED"})
        if self.page_status != 200:
            return Resp(self.page_status, text="Not Found")
        if headers.get("Referer") != "https://steamrip.com/":
            return Resp(303, {"location": "https://steamrip.com"})
        return Resp(200, text=PAGE)


class HostMatching(unittest.TestCase):
    def test_hosts(self):
        cases = {
            "https://bzzhr.to/ijvxifbx5sgh": "buzzheavier",
            "https://buzzheavier.com/ijvxifbx5sgh/": "buzzheavier",
            "http://www.bzzhr.co/ijvxifbx5sgh?ref=x": "buzzheavier",
            "https://fuckingfast.co/abc123#Game.part001.rar": "fuckingfast",
            "https://bzzhr.to/help": None,                    # a site page, not a file
            "https://bzzhr.to/ijvxifbx5sgh/download?t=1": None,
            "https://gofile.io/d/abc": None,
        }
        for url, want in cases.items():
            h = H.host_for(url)
            self.assertEqual(h.name if h else None, want, url)


class BuzzHeavier(unittest.TestCase):
    def setUp(self):
        self.bh = H.BuzzHeavier()

    def test_parse(self):
        button, name = self.bh.parse(PAGE)
        self.assertEqual(button, "/abcdefghij12/download?t=TOKEN.sig&x=1")   # not the alt=true one
        self.assertEqual(name, "Some.Game & DLC-SteamRIP.com.rar")

    def test_resolve_comes_from_the_uploaders_site(self):
        s = FakeSession()
        with mock.patch.object(H, "http_session", return_value=s):
            signed, name = self.bh.fast_resolve("https://bzzhr.to/abcdefghij12")
        self.assertEqual(signed, "https://ts.bzzhr.to/d/abcdefghij12?v=SIGNED")
        self.assertEqual(name, "Some.Game & DLC-SteamRIP.com.rar")
        self.assertEqual(s.calls[0][1].get("Referer"), None)                # first try: no referer
        self.assertEqual(s.calls[1][1].get("Referer"), "https://steamrip.com/")  # named by the redirect
        url, hdrs = s.calls[2]
        self.assertEqual(url, "https://bzzhr.to/abcdefghij12/download?t=TOKEN.sig&x=1")
        self.assertEqual(hdrs.get("HX-Request"), "true")

    def test_dead_link(self):
        with mock.patch.object(H, "http_session", return_value=FakeSession(page_status=404)):
            with self.assertRaises(DeadLinkError):
                self.bh.fast_resolve("https://bzzhr.to/abcdefghij12")

    def test_cloudflare_refusal_falls_back(self):
        s = FakeSession(button=Resp(403, {"cf-mitigated": "challenge"}))
        with mock.patch.object(H, "http_session", return_value=s):
            with self.assertRaises(ResolveError) as cm:
                self.bh.fast_resolve("https://bzzhr.to/abcdefghij12")
        self.assertNotIsInstance(cm.exception, DeadLinkError)  # the browser gets a go

    def test_probe(self):
        r = Resp(206, {"content-range": "bytes 0-0/5337461543",
                       "content-disposition": "attachment; filename=\"a.rar\"; filename*=UTF-8''My%20Game.rar"})
        with mock.patch.object(H, "http_session", return_value=mock.Mock(get=mock.Mock(return_value=r))):
            self.assertEqual(H.probe("https://x/y"), (5337461543, "My Game.rar"))
        with mock.patch.object(H, "http_session", return_value=mock.Mock(get=mock.Mock(side_effect=OSError("down")))):
            self.assertEqual(H.probe("https://x/y"), (None, None))


class ParseInput(unittest.TestCase):
    def test_free_text(self):
        text = ("Mirrors (pick one): https://bzzhr.to/ijvxifbx5sgh, https://gofile.io/d/abc and\n"
                "https://bzzhr.to/ijvxifbx5sgh/ (again) https://fuckingfast.co/x1y2z3#G.part01.rar.")
        page, links, ignored = W.parse_input(text)
        self.assertIsNone(page)
        self.assertEqual(links, ["https://bzzhr.to/ijvxifbx5sgh", "https://fuckingfast.co/x1y2z3#G.part01.rar"])
        self.assertEqual(ignored, ["gofile.io"])

    def test_fitgirl_page(self):
        page, links, _ = W.parse_input("https://fitgirl-repacks.site/some-game/  see https://gofile.io/x")
        self.assertEqual((page, links), ("https://fitgirl-repacks.site/some-game/", []))
        with self.assertRaises(W.InputError):
            W.parse_input("https://fitgirl-repacks.site/some-game/ https://bzzhr.to/ijvxifbx5sgh")

    def test_nothing_supported(self):
        with self.assertRaises(W.InputError) as cm:
            W.parse_input("https://gofile.io/d/abc https://mega.nz/file/x")
        self.assertIn("gofile.io", str(cm.exception))
        self.assertIn("BuzzHeavier", str(cm.exception))


class CheckThenQueue(unittest.TestCase):
    """A "download now" of links without names: checked, then queued as a
    package named after them, with each part's name and size known."""

    def test_autostart(self):
        added = {}

        class FakeManager:
            def add(self, page, links, extract, delete, title, names=None):
                added.update(page=page, links=links, title=title, names=names, extract=extract)
                return mock.Mock(id="dl1")

        links = ["https://bzzhr.to/aaaaaaaaaa01", "https://bzzhr.to/bbbbbbbbbb02"]
        names = {links[0]: "Some.Game-SteamRIP.com.part1.rar", links[1]: "Some.Game-SteamRIP.com.part2.rar"}
        job = W.Job(" ".join(links))
        job.autostart = {"extract": True, "delete_archives": False}
        with mock.patch.object(W, "fast_resolve", side_effect=lambda u: ("https://ts/" + u[-4:], names[u])), \
                mock.patch.object(W, "probe", return_value=(123, None)), \
                mock.patch.object(W, "MANAGER", FakeManager()), \
                mock.patch.object(W, "_pool", mock.Mock(submit=lambda fn, *a: fn(*a))):
            W.run_job(job)
        d = job.as_dict()
        self.assertEqual(d["phase"], "done")
        self.assertEqual([it["size"] for it in d["items"]], [123, 123])
        self.assertEqual([it["host"] for it in d["items"]], ["BuzzHeavier", "BuzzHeavier"])
        self.assertEqual(d["title"], "Some.Game-SteamRIP.com")
        self.assertEqual(d["queued"], "dl1")
        self.assertEqual(added["links"], links)
        self.assertEqual(added["names"], names)
        self.assertEqual(added["title"], "Some.Game-SteamRIP.com")


class SplitState(unittest.TestCase):
    """A split download's partial reaches full length with holes in it. Without
    its .aria2 control file it must never pass for finished."""

    URL = "https://bzzhr.to/aaaaaaaaaa01"

    def setUp(self):
        import tempfile
        from pathlib import Path
        from app.state import DownloadState
        self.dir = Path(tempfile.mkdtemp())
        self.file = self.dir / "big.rar"
        with open(self.file, "wb") as f:
            f.truncate(64 << 20)      # full length, nothing written: all hole
        if self.file.stat().st_blocks * 512 >= self.file.stat().st_size:
            self.skipTest("this filesystem has no sparse files")
        self.st = DownloadState(self.dir)
        self.st.record_resolved(self.URL, "big.rar")
        self.st.record_progress(self.URL, "big.rar", 64 << 20, 1 << 20)

    def test_holes_are_not_finished(self):
        self.assertIsNone(self.st.adopt_if_finished(self.URL, split=True))
        self.assertFalse(self.st.is_complete(self.URL))
        self.assertEqual(self.st.clear_ghost_file(self.URL, split=True), "big.rar")
        self.assertFalse(self.file.exists())

    def test_control_file_keeps_it(self):
        (self.dir / "big.rar.aria2").write_bytes(b"x")
        self.assertIsNone(self.st.clear_ghost_file(self.URL, split=True))
        self.assertTrue(self.file.exists())   # aria2 resumes it from the segment map

    def test_written_file_is_adopted(self):
        self.file.write_bytes(b"\1" * (64 << 20))
        self.assertEqual(self.st.adopt_if_finished(self.URL, split=True), 64 << 20)
        self.assertTrue(self.st.is_complete(self.URL))


class ManagerNames(unittest.TestCase):
    def test_add_with_names(self):
        import tempfile
        from app import manager as M
        links = ["https://bzzhr.to/aaaaaaaaaa01", "https://bzzhr.to/bbbbbbbbbb02"]
        with tempfile.TemporaryDirectory() as d:
            m = M.Manager(d)
            g = m.add(None, links, names={links[0]: "Some.Game.part1.rar", links[1]: "Some/Game.part2.rar"})
            self.assertEqual([p.filename for p in g.parts], ["Some.Game.part1.rar", "Some_Game.part2.rar"])
            self.assertEqual(g.title, "Some")   # the names' common prefix, as for FitGirl parts
            g2 = m.add(None, links[:1], title="Picked name", names={links[0]: "x.rar"})
            self.assertEqual(g2.title, "Picked name")


if __name__ == "__main__":
    unittest.main()
