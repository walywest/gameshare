# Game Share

Moves huge game files over the LAN with transfers that always resume. A small Go server (`server/`) runs on the Linux laptop, and a terminal client (`client/`) runs in WSL. Both are set up with [devenv](https://devenv.sh).

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

## CD mode

Start the server with `serve --cd [folder]` and `gs` can update it: press `U`, then `y`. The server runs `git pull --ff-only` in its repo. If anything under `server/` changed and still builds, it restarts on the new code. Transfers reconnect by themselves. `gs` shows what happened: already up to date, pulled with no server change, restarting, or pulled but doesn't build. A broken build leaves the old server running.

The pull authenticates with `GIT_PASS` from `.env` at the repo root on the server, read on every update. For GitHub, use a personal access token with read access to the repo. `GIT_USER` is optional. `.env` is gitignored.

```sh
GIT_PASS=github_pat_...
```

In CD mode, `serve` builds the server from `server/` with Go instead of using the devenv package, then rebuilds it after each update. It doesn't reload itself, so after a pull that changes `devenv.nix`, restart `serve` by hand. A pull fails rather than merging if the server's checkout has local commits or edits that clash.

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

Plain HTTP and no auth, on purpose. Anyone who can reach the port can read, upload and delete files, so open it only to networks you trust. Requests can't leave the shared folder and dotfiles aren't served. Deleting refuses the shared folder itself and anything an upload is still writing into. In CD mode, anyone who can reach the port can also make the server pull and restart. They can only pull what's already on GitHub.

## Changing the code

devenv can miss edits in `server/` and keep running the old build. After changing it, enter the shell with:

```sh
devenv shell --refresh-eval-cache
```
