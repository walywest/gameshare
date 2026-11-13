# Notes for agents working on MyHomeLab

Read this before changing anything. `README.md` is the user-facing manual; this file covers how the system works, the rules that keep the live server safe, and how to test.

## What this is

A home server on a Linux laptop, driven from a Windows/WSL machine on the same LAN. It started as **game-share**, a file mover with resumable transfers, and became a homelab that runs other services. Plain HTTP with no auth, **LAN only**, on purpose. Don't add internet exposure or Tailscale without the owner asking.

```
server/            Go server (port 8080): file API, CD updates, service supervisor, homelab page
  main.go          file API (list/tree/upload/delete/dl/speed), startup, signals
  update.go        CD: POST /api/update -> git pull, then rebuild+exit 75 or reconcile services
  services.go      the reconciler: homelab.json -> running processes, /svc/<name>/ proxy
  storage.go       GET /api/storage (disk, folder sizes, .rar folders) and /api/transfers (live, read from file offsets)
  folders.go       GET /zip (folder as a stored zip, streamed; skip=archives for extracted files only),
                   POST /api/archives/delete (RAR volumes only), POST /api/extracted/delete (all but the download)
  web.go, web/     the homelab page (single embedded index.html, vanilla JS, no CDN)
  preflight.go     disk/NIC checks printed at startup
client/            gs: Bubble Tea TUI for transfers (runs in WSL). Its only homelab link is `U` (update)
modules/fitfast/   FitFast downloader (Python), imported with git subtree (history kept)
  app/manager.py   headless server-side download engine (queue, aria2, retries, extract)
  app/web.py       HTTP API: /api/status, /api/downloads*, /api/jobs* (links only)
  app/tui.py       LEGACY Textual UI. Kept for reference; nothing runs it. Don't extend it
  tests/           unittest for manager.py (fake aria2 + resolver)
homelab.json       desired state: which services run and how
devenv.nix         ONE devenv for everything (Go server package, python venv, aria2, unar)
```

## How the live server runs

The server box runs `devenv shell` then `serve --cd <folder>`. The `serve` script (in `devenv.nix`) is a bash loop:

1. `go build` `server/` to `$DEVENV_STATE/game-share-cd` with the **pinned** Go (`${pkgs.go}`, Go 1.26 from `devenv.lock`) and `CGO_ENABLED=0 GOTOOLCHAIN=local`.
2. Run it with `GAMESHARE_CD=<repo>` and `GAMESHARE_GO=<go>`.
3. If it exits **75**, loop and rebuild. Any other exit ends serve.

`POST /api/update` (gs `U`, or the page's button) runs `git pull --ff-only`. Then:

- **`server/` changed:** test-build it. If the build works, stop all services and exit 75. If it fails, keep running and report `buildErr`.
- **Otherwise:** reload `homelab.json` and reconcile right away. The response lists `services` changes and `manifestErr`.
- **`scripts.serve` text in `devenv.nix` changed:** set `shellChanged`. The running bash loop never re-reads itself, so this is the one change that needs a manual restart.

Git auth comes from `GIT_PASS` in `.env` (repo root or `server/`), fed through a self-askpass. git is never allowed to prompt. See `update.go`.

## Services (`server/services.go`)

- The manifest is decoded with `DisallowUnknownFields`, and names must match `[a-z0-9_-]`. Fields are documented in README "Declaring services".
- **Spec hash:** a hash of the JSON entry plus `git ls-tree HEAD -- <watch paths>`. A different hash means a restart. Only **committed** files count, so uncommitted local edits don't restart anything.
- **`devenv: true`** runs `devenv shell <flags> -- bash -c 'cd "$0" && exec "$@"' <dir> <run...>` from the repo root.
  - The flags (`--no-tui --no-reload --refresh-eval-cache`) are probed from `devenv shell --help`, because an unknown flag would break every start on an older devenv.
  - Inherited `DEVENV_*`, `DIRENV_*`, `GAMESHARE_*`, `VIRTUAL_ENV`, `IN_NIX_SHELL` and `PYTHON*` vars are stripped, so the nested shell is clean.
  - `devenv` is looked up on PATH, then in the usual Nix profile paths.
- **Process group:** `Setpgid` + `Pdeathsig`. A stop sends SIGTERM to the group, waits 15s, then SIGKILL. The PGID is written to `<root>/.homelab/<name>.pgid`, so leftovers from a `kill -9`'d server get cleaned up before the next start.
- **States:** `starting`, `running`, `unhealthy`, `exited`, `crashloop`, `failed`, `stopping`, `stopped` and `disabled`.
  - Crash backoff runs from 2s up to 5min. A run over 1min resets it.
  - Health has to pass within `startupSeconds` (20 min default for devenv services, because the first start builds the venv), and later failures for 30s cause a restart.
- **Proxy:** `/svc/<name>/*` goes to `127.0.0.1:<port>/*`. When the service is down it answers 503 JSON.
- **Busy paths:** `busy` endpoints return `{"paths": [...]}` relative to the share. `handleDelete` refuses those paths, their parents and their children. A service that doesn't answer within 1s is skipped.
- **API:** `GET /api/services`, `GET /api/services/{name}/logs?n=`, `POST /api/services/{name}/restart`, `POST /api/reconcile`. `/api/version` also includes `services`.

## FitFast service

- It runs bound to `127.0.0.1:8787`. Loopback means **open access** (no PASS): the Go server is the gate. Bound anywhere else, it still requires `PASS` from `.env`, as before.
- `FITFAST_ROOT` is the shared folder:
  - games download to `<root>/FitFast/<Game>/`;
  - the queue is in `<root>/.fitfast/queue.json`, written atomically;
  - per-folder `.fitfast.json` manifests (from `app/state.py`) are what make resume correct. Read the long docstring in `state.py` before touching completeness logic.
- **Engine:** one aria2 daemon, one resolver thread, games one at a time, and parts in parallel.
  - **Resolving:** links go through `SubprocessResolver`, which tries curl_cffi first and falls back to Camoufox in a killable subprocess.
  - **Retries:** the TUI's rules apply (`MAX_FAILS_WITHOUT_PROGRESS = 8`, exponential backoff).
  - **Downloads:** **one connection per file**, because the CDN breaks bounded Range requests (see the `downloader.py` docstring). Don't raise `--split`.
  - **SIGTERM:** stops aria2 cleanly, which keeps its control files, then saves the queue.
- If the Camoufox browser is missing, it installs in the background on start (`BROWSER` status in `/api/status`). Fast resolving works without it.
- `PYTHONUNBUFFERED=1` is set in `homelab.json`, so its logs reach the page live.

## Rules (each was learned the hard way)

1. **The server must never die from a service or manifest problem.** If the new binary crashes at startup, the bash loop exits, and nobody is at the box to restart it. Fail soft and report on the page.
2. **The Go server is standard library only** (`go 1.22` in go.mod, built with the pinned 1.26). It must build offline with the old toolchain the running loop holds.
3. **Never run a real FitGirl download as a test.** Use the fakes in `modules/fitfast/tests/`.
4. **Don't put Nix libs on `LD_LIBRARY_PATH`.** On Ubuntu/WSL, Nix's glib needs a newer glibc and Camoufox dies at launch. Only `NIX_LD_LIBRARY_PATH` is set, for nix-ld on NixOS.
5. **Treat changes to `scripts.serve` in `devenv.nix` as rare.** They need a manual restart on the box.
6. **Commit and push only when the owner asks.** Pushing `main` is what deploys, once someone presses `U`.
7. Don't `pkill -f <pattern>` from a shell whose own command line contains the pattern, because it kills itself. Use `pgrep -x` or a PID.
8. `FitFast-Downloader/` in the repo root is an untracked copy the owner made. Never commit or delete it.

## Testing

Go isn't on the devenv shell's PATH (only the scripts use it). Use the pinned one:

```sh
S=$(grep -o '/nix/store/[^ ]*-serve-script' "$(readlink -f .devenv/profile/bin/serve)")
export PATH=$(dirname "$(grep -o '/nix/store/[^ "]*/bin/go' "$S" | head -1)"):$PATH
(cd server && CGO_ENABLED=0 GOTOOLCHAIN=local go test ./...)   # ~10s; runs the test binary as fake services
(cd client && CGO_ENABLED=0 GOTOOLCHAIN=local go test ./...)
devenv shell --no-tui --no-reload -- bash -c 'cd modules/fitfast && python -m unittest discover tests'
```

To try it live without touching port 8080:

```sh
go build -C server -o /tmp/hl-server . && GAMESHARE_REPO=$PWD /tmp/hl-server 18080 /tmp/hl-root
# page: http://localhost:18080/   FitFast via /svc/fitfast/...
```

**Rehearse anything that changes startup, `update.go`, `services.go` or `devenv.nix` before it ships.**

1. Make a bare clone of `main` as a fake origin, and a "box" checkout of it with `GIT_PASS=dummy` in its `.env`.
2. In the box, run `devenv shell --no-tui -- serve --cd <tmp-root>`.
3. Push the new commits to the fake origin, then `curl -XPOST localhost:8080/api/update`.
4. Check that the server comes back and services reach `running`. Also exercise:
   - a module-only change: only that service restarts;
   - a broken `homelab.json`: the service keeps running;
   - `kill -9` of the service's group: it recovers;
   - `"enabled": false`: the service stops.

That's how the first homelab rollout was verified (2026-11-10).

## Status (2026-11-13)

- The box runs the homelab from `main`. Deploys so far were `git push origin main`, then `POST /api/update` (same as `U` in gs); each came back on its own with FitFast healthy.
- A real FitGirl download (HITMAN WoA, 120 parts) has gone through the page end to end, including extraction. Tests still use only the fakes (rule 3).
- Transfer tracking must never sit on the copy path: `/dl` relies on sendfile, so progress is read from the file offset, not by wrapping the writer.
- Unknown about the box: whether it's NixOS. If it is, the Camoufox fallback needs `programs.nix-ld.enable = true`.
- Ideas that weren't asked for: a gs FitFast pane (the owner chose the page instead), and auth for any internet exposure (the owner said LAN only "for now").
