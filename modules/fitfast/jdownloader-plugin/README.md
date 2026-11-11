# FitFast for JDownloader (prototype)

A spike proving that FitFast's Cloudflare bypass can feed **JDownloader**
instead of (or alongside) the TUI — so you get JD's mature download
manager (pause/resume, queueing, its own UI) while still getting past
fuckingfast.co's Cloudflare wall, which JDownloader cannot do on its own.

**Status: prototype.** The Python side (`resolver_daemon.py`) is written,
tested, and confirmed working end-to-end against real fitgirl-repacks.site /
fuckingfast.co links on Linux. The Java side
(`src/jd/plugins/decrypter/FuckingFastCoStealth.java`) is modeled closely on
JDownloader's own plugin source but **has not been compiled or run against a
real JDownloader install** — there's no JDownloader jar/plugin API available
in the environment this was built in. Treat it as a strong starting point,
not a drop-in-and-done artifact.

## Why this shape

JDownloader gets `Blocked by Cloudflare` on fuckingfast.co because its
download engine is a plain HTTP client — it can't run Cloudflare's JS
challenge, and its TLS/HTTP fingerprint doesn't read as a real browser. No
amount of retrying or header-spoofing fixes that; you need an actual browser
to pass the check once per session.

So the work splits in two, matching how the rest of this project is built:

- **Pass Cloudflare** (do this once, keep the trust) — Camoufox, the same
  stealth browser the TUI use, resolves a fuckingfast.co page into a
  pre-signed, cookie-free `dl.fuckingfast.co` URL.
- **Move the bytes fast** (do this per file, at volume) — hand that signed
  URL to a download engine built for throughput. For the TUI that's
  aria2c; here, it's JDownloader itself.

A FitGirl repack is ~150–250 part links, so resolving is not a one-shot
operation — `resolver_daemon.py` keeps a single Camoufox browser alive for
its whole run and resolves every link against that one session, rather than
cold-starting a browser per link (which an earlier, simpler version of this
— `resolve_cli.py` — does; kept around because it's handy for testing one
link by hand, not because it's what the plugin should use for a whole game).

```
JDownloader  --HTTP GET /resolve?url=...-->  resolver_daemon.py (Camoufox, one browser)
     |                                              |
     `<-- signed dl.fuckingfast.co URL -------------'
     |
     `--> its own download engine (pause/resume, queue, GUI)
```

## Running the daemon

```sh
# from the repo root, inside the devenv shell (or any env with the TUI's
# requirements.txt installed + camoufox fetch already done)
python jdownloader-plugin/resolver_daemon.py            # default port 8765
python jdownloader-plugin/resolver_daemon.py --port 9000 # custom port
```

Leave it running in a terminal (or as a background service) while you use
JDownloader. It binds `127.0.0.1` only.

Verified manually against the daemon with `curl`:

```sh
curl 'http://127.0.0.1:8765/resolve?url=https%3A%2F%2Ffuckingfast.co%2F<id>%23<name>.part01.rar'
# => {"signed_url": "https://dl.fuckingfast.co/dl/<token>", "filename": "<name>.part01.rar"}
```

Two different real part-links were resolved through the same daemon process
in one test run — both succeeded, and the second reused the already-open
browser rather than relaunching one.

### `resolve_cli.py` (single-link helper, not what the plugin uses)

```sh
python jdownloader-plugin/resolve_cli.py "https://fuckingfast.co/<id>#<name>.part01.rar"
```
Prints one line of JSON to stdout on success, an error to stderr and exit 1
on failure. Useful for checking a single link works before wiring up the
daemon or the plugin; spins up (and tears down) its own browser per call, so
it's the wrong tool for resolving a whole game's worth of links.

## Installing the plugin into JDownloader

JDownloader can load custom `.java` decrypter/hoster plugins it compiles
itself at runtime. The exact mechanism has shifted across JDownloader
releases, so treat these as a starting point:

1. Locate your JDownloader install's plugin-loading/dev folder (varies by
   version/platform — check JDownloader's own docs/forum for your release;
   search for how JD loads third-party/local `.java` plugins).
2. Edit `REPO_ROOT` near the top of `FuckingFastCoStealth.java` to your actual
   FitFast-Downloader checkout path, then copy the file there, preserving the
   `jd/plugins/decrypter/` package path.
3. Either start `resolver_daemon.py` (above) yourself, or just skip this —
   the plugin will auto-launch it on first use if `/health` isn't already
   answering (see "Known gaps" below for what that depends on).
4. Paste a fuckingfast.co link into JDownloader's LinkGrabber. It should hit
   the plugin, which calls the daemon and hands JD the resolved signed URL.

If it doesn't compile: JDownloader's internal plugin API
(`PluginForDecrypt`, `@DecrypterPlugin`, `CryptedLink`, `DownloadLink`, …)
isn't perfectly stable release to release. The plugin here was written
against the shape found in JDownloader's own published source
(`jd.plugins.decrypter.*`, e.g. `BergFilesCom.java`) — if a method/annotation
field doesn't exist in your JD version, find the closest first-party
decrypter plugin shipped with your install and match its signatures.

## Known gaps / next steps

- **Not compiled or run against a real JDownloader.** Still the single
  highest-value next step — this environment has no `javac`/JD jar to
  compile against, so the Java has only been checked by hand (brace/paren
  balance) and by matching a real shipped decrypter's shape. Open it in an
  actual JDownloader dev setup and fix whatever the real compiler complains
  about.
- **The daemon now auto-launches.** `decryptIt()` calls `ensureDaemonRunning()`
  first: a quick `/health` check, and if that fails, a guarded (single-launch)
  `ProcessBuilder` spawn of `resolver_daemon.py`, then polling `/health` for
  up to 30s (Camoufox's first request is a real browser cold-start, so this
  needs more headroom than a normal HTTP call). **You must edit `REPO_ROOT`**
  in `FuckingFastCoStealth.java` to point at your actual checkout path before
  this works — it can't be discovered at runtime from inside a single `.java`
  file with no build metadata. Daemon stdout/stderr goes to
  `jdownloader-plugin/resolver_daemon.log`. You can still start the daemon
  by hand first (as before) — auto-launch only kicks in when `/health`
  already fails.
- **JSON parsing is still regex, not a real parser** — but the regexes now
  match escaped-string content (`(?:\\.|[^"\\])*`) and a `jsonUnescape()`
  helper undoes `\"`, `\\`, `\/`, `\n`, `\r`, `\t`, `\b`, `\f`, and `\uXXXX`,
  so filenames with quotes/backslashes/unicode no longer break parsing. If
  you extend the response shape beyond these fixed fields, switch to
  JDownloader's bundled `org.appwork.storage.JSonStorage` (available once
  you're compiling against the real jar) instead of adding more regexes.
- **No captcha/edge-case handling in Java.** Anything Camoufox's resolve step
  hits (captcha, dead link, layout change) surfaces as a plain error string
  from the daemon; the plugin reports it via `DecrypterException` rather than
  trying to solve it.
