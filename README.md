# MyHomeLab

Moves huge game files over the LAN with transfers that always resume, and runs the homelab's other services. A small Go server (`server/`) runs on the Linux laptop, and a terminal client (`client/`) runs in WSL. Both are set up with [devenv](https://devenv.sh), from one `devenv.nix` at the repo root.

## Usage

On the laptop:

```sh
devenv shell
serve /srv/games     # or plain `serve` to share ./games
```

`serve` prints its URLs. The port needs to be open on the laptop's firewall.

In WSL:

```sh
devenv shell
gs http://192.168.1.11:8080
```

`gs` compiles the first time you run it, so the laptop never builds it unless you run it there. It rebuilds by itself when `client/` changes.

`gs` remembers the server, so after the first run plain `gs` is enough. Downloads go to `~/Downloads/GameShare`; use `-d <folder>` to change that.

## Speed check

Speed is the point of this tool, so both ends test themselves before they start.

**`serve`** takes about 2 seconds and checks what the laptop can see alone: the speed of the shared folder's disk, whether Ethernet is up and what speed it negotiated, and free space. It prints alerts but always starts, because a server may have nobody watching it. It can't measure real throughput, which takes both ends.

**`gs`** then times every stage, in about 10 seconds:

| Direction | Stages, in order                              |
| --------- | --------------------------------------------- |
| Download  | Server disk read, network, local disk write   |
| Upload    | Local disk read, network, server disk write   |

A transfer is only as fast as its slowest stage, so `gs` prints that stage for each direction. The network tests use memory, not disk, so they measure the link alone. Disk tests write a temporary file, flush it, and read it back with the page cache dropped.

An **ALERT** means something is holding transfers back, and `gs` waits for Enter before continuing: a network under 100 MB/s, or a disk slower than the network. A **note** is worth knowing but doesn't stop anything: high latency, a disk with little headroom, or a download folder on `/mnt`.

If the server can't be reached, the check is skipped and `gs` starts anyway, so queued transfers can still resume. Use `gs -no-check` to skip it, or `gs -check` to run only the check. `-get` and `-put` don't run it.

## Keys

Press `?` in `gs` for this list. The footer always shows the keys for the pane you're in.

| Pane  | Key                     | Action                                     |
| ----- | ----------------------- | ------------------------------------------ |
| Both  | `tab`                   | Switch between files and queue             |
| Both  | `↑` `↓` / `k` `j`       | Move                                       |
| Both  | `pgup` `pgdn`           | Move a page (also `ctrl+u`, `ctrl+d`)      |
| Both  | `home` `end` / `g` `G`  | Jump to the top or bottom                  |
| Both  | `u`                     | Type or drop paths to upload               |
| Both  | `c`                     | Clear finished items                       |
| Both  | `U`                     | Update the server from GitHub, after a `y` to confirm. Needs CD mode |
| Both  | `?`                     | Show all keys                              |
| Both  | `q`                     | Quit. The queue resumes on the next run    |
| Files | `enter` `→` `l`         | Open a folder                              |
| Files | `←` `h` `backspace`     | Go up, with the folder you left selected. `~` goes to the top |
| Files | `d`                     | Download the selected file or folder       |
| Files | `a`                     | Download everything in this folder         |
| Files | `/`                     | Filter this folder by name. `esc` clears it |
| Files | `x` / `delete`          | Delete from the server, after a `y` to confirm |
| Files | `r`                     | Refresh                                    |
| Queue | `space`                 | Pause or resume                            |
| Queue | `n`                     | Run this item next                         |
| Queue | `x` / `delete`          | Remove from the queue. Its partial file stays, so adding it again resumes |
| Queue | `o` / `enter`           | Show the item's folder in the file list    |

In the file list, `↓` and `↑` mark files and folders with transfers still to finish. Below 80 columns only one pane shows at a time; `tab` switches.

To upload, drag files or folders from Explorer onto the terminal. They go to the folder open in the Files pane. If a drop doesn't register, press `u` first, then drop and press Enter. `C:\...` paths are converted to `/mnt/c/...`.

Without the UI: `gs -get game/folder` and `gs -put /path -into game`.

## Homelab page and services

The server serves a page at `http://<server>:8080/` that brings everything together:

- **Storage**: free and total space on the shared folder's disk, what the share uses, its largest folders, unfinished uploads kept for resuming, and the folders still holding `.rar` archives (`GET /api/storage`).
- **Transfers**: every upload and download moving through the server right now, with progress, speed, ETA and the client's address (`GET /api/transfers`). gs's queue lives on the WSL machine, so items waiting there show up once they start.
- **Files**: browse the share, download files or whole folders, delete, and upload files or folders (with the buttons, or by dropping them on the box). The address bar keeps the folder (`/#/Game/sub`).
  - Browser uploads use the same resumable protocol as gs, so uploading a file again after a drop carries on where it stopped. Names starting with a dot are skipped.
  - A folder downloads as a `.zip` built on the fly (`GET /zip?path=`), with files stored rather than compressed, so it runs at network speed. It can't resume after a drop, so gs is safer for a big game over a shaky link.
- **Deleting `.rar` archives**: once a game is extracted its RAR volumes are dead weight the size of the game. **Delete** next to a folder in Storage, **Delete .rar archives here** in Files, or the button on a finished FitFast game removes only the `.rar` / `.rNN` volumes under that folder (`POST /api/archives/delete?path=`). Games FitFast extracted are marked as such. The server refuses while FitFast is still downloading or extracting there.
- **Services**: what's running, its health, PID, uptime and restarts, its log, and a **Restart** button.
- **Pull & reconcile**: the same update as `U` in gs (see CD mode), with what changed.
- **FitFast downloads**: paste a `fitgirl-repacks.site` page or `fuckingfast.co` links and press **Download on server**. The game downloads on the server into `FitFast/<Game>/` in the shared folder, then extracts. Pull it to WSL with gs once it's done. **Get links only** gives you the signed links for another downloader instead. Downloads resume across server restarts.

### Declaring services

`homelab.json` at the repo root is the desired state, like a Kubernetes manifest. The server keeps reality in line with it, checking every 5 seconds:

- a declared service that isn't running is started;
- one whose entry in `homelab.json`, or any file under its `watch` paths in git, changed is restarted;
- one that crashes is restarted, with a backoff from 2 seconds up to 5 minutes. After three quick crashes in a row it shows as `crashloop`, but it keeps being retried;
- one whose `health` check fails for 30 seconds is restarted;
- one that's removed, or set to `"enabled": false`, is stopped.

```json
{
  "services": {
    "fitfast": {
      "title": "FitFast downloader",
      "dir": "modules/fitfast",
      "run": ["python", "fitfast_web.py", "--host", "127.0.0.1", "--port", "8787"],
      "env": { "FITFAST_ROOT": "${ROOT}", "PYTHONUNBUFFERED": "1" },
      "devenv": true,
      "port": 8787,
      "health": "/api/status",
      "busy": "/api/downloads/busy",
      "watch": ["modules/fitfast", "devenv.nix", "devenv.yaml", "devenv.lock"]
    }
  }
}
```

| Field | Meaning |
| ----- | ------- |
| `dir`, `run` | Folder in the repo, and the command to run there |
| `env` | Extra environment. `${ROOT}` is the shared folder, `${REPO}` the repo |
| `devenv` | Run inside the repo's `devenv shell`, so it gets the packages `devenv.nix` declares, current as of its last start |
| `port`, `health` | Where it listens on 127.0.0.1, and a path that answers 200 when healthy. The page and API reach it at `/svc/<name>/` |
| `busy` | A path listing folders in the share it's writing into. Deleting those from gs is refused |
| `watch` | Repo paths whose change restarts it. Defaults to `dir` |
| `startupSeconds` | How long a new process may fail its health check. Defaults to 20 minutes with `devenv`, since the first start builds the environment, and 1 minute without |
| `enabled` | `false` stops it but keeps it listed |

A service never takes the server down. When `homelab.json` doesn't parse, the last good version stays in force and the page shows the error. A service that won't start shows its error and log on the page. Services stop cleanly when the server stops or restarts.

The server runs services only when it knows its repo, which `serve` tells it. The first start of a `devenv` service builds its environment, so it shows as `starting` for a few minutes. FitFast also fetches its ~1 GB stealth browser in the background the first time, and resolves links without the browser meanwhile.

## CD mode

Start the server with `serve --cd [folder]` and you can update everything from GitHub: press `U` in gs and then `y`, or press **Pull & reconcile** on the page. The server runs `git pull --ff-only` in its repo, and then:

- if anything under `server/` changed and still builds, it stops the services and restarts on the new code, which starts them again. Transfers reconnect by themselves. A broken build leaves the old server running;
- otherwise it reconciles straight away: the services the pull changed restart, new ones start and removed ones stop. The server itself keeps running.

gs and the page show what happened: already up to date, the services the pull restarted, started or stopped, restarting, or pulled but doesn't build.

Services run in a fresh `devenv shell`, so a pull that changes `devenv.nix` reaches them when they restart. The one exception is the `serve` script itself, which keeps running the version it started with: when a pull changes it, gs and the page say to restart `serve` by hand.

The pull authenticates with `GIT_PASS` from `.env` at the repo root, or in `server/`, on the server. It's read on every update. `GIT_PASS` must be a GitHub personal access token with read access to the repo, because GitHub refuses account passwords for git. `GIT_USER` is optional. `.env` is gitignored.

git never prompts during an update. Without `GIT_PASS` the update stops with an error that names the files it checked. GitHub is always reached over HTTPS, even if the server's checkout was cloned over SSH.

```sh
GIT_PASS=github_pat_...
```

In CD mode, `serve` builds the server from `server/` with Go instead of using the devenv package, then rebuilds it after each update. A pull fails rather than merging if the server's checkout has local commits or edits that clash.

## Resuming

- **Network trouble never fails a transfer.** A dropped connection, a stall of 20 seconds, or a server restart all lead to reconnecting from the last byte, indefinitely.
- **The queue is on disk.** It survives quitting, `kill -9` and rebooting, and `gs` carries on when started again.
- **Downloads** write to `<name>.part` and are renamed when complete, so a file with its final name is always whole. Each resume re-reads the last 1 MB and compares it with the server. If it differs, or the server's file changed, that file starts over.
- **Folders** transfer file by file, so each file resumes on its own.
- **Uploads** keep partial data in `.uploads` on the server until they finish. Abandoned ones stay there until you delete them.
- **Failed** means something a retry can't fix alone, such as a full disk. Fix it and press `space`.

Files are matched by size and modified time, not by checksum, so a file that's already complete is skipped straight away.

## The queue

- Smaller items go first, so one huge file doesn't hold up the rest.
- Per direction, one item of 256 MB or more runs at a time, alongside up to four smaller ones.

## Speed

Gigabit Ethernet tops out around 117 MB/s, and the network has to stay the slowest part. Measured through `gs` on one machine with no network involved:

| Path                  | Speed     |
| --------------------- | --------- |
| Download to WSL disk  | 285 MB/s  |
| Download to `/mnt/c`  | 165 MB/s  |
| Upload from WSL disk  | 523 MB/s  |
| Upload from `/mnt/c`  | 199 MB/s  |

`/mnt/c` goes through WSL's slower 9p bridge. It clears gigabit but would be the bottleneck on a faster link, so the WSL disk is the default. The speed check's `/mnt/c` read can read a little high, because Windows may have cached its test file.

A rate stuck near 12 MB/s means the link is running at 100 Mbps. Check the cable and the adapter's Speed & Duplex setting, which should be Auto Negotiation.

## Security

Plain HTTP and no auth, on purpose: this is for the LAN only. Anyone who can reach the port can read, upload and delete files, start and stop downloads, and restart services, so don't forward it to the internet. Services listen on 127.0.0.1 only and are reached through the server. Requests can't leave the shared folder and dotfiles aren't served. Deleting refuses the shared folder itself and anything an upload is still writing into. In CD mode, anyone who can reach the port can also make the server pull and restart. They can only pull what's already on GitHub, and only what `homelab.json` there declares runs.

## Changing the code

Server tests run the test binary as a fake service, and FitFast's manager has its own tests: `cd modules/fitfast && python -m unittest discover tests`, inside `devenv shell`.

FitFast came in with `git subtree`, so its history is in this repo. Its Textual TUI (`fitfast_tui.py`) is kept but is legacy; the page replaces it.

devenv can miss edits in `server/` and keep running the old build. After changing it, enter the shell with:

```sh
devenv shell --refresh-eval-cache
```
