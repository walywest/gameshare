# FitFast on Linux / NixOS (terminal edition)

FitFast is a **terminal UI** — `fitfast_tui.py` — built on
[Textual](https://textual.textualize.io/). It shells out to the system
`aria2c` (downloads) and `unar`/`unrar` (extraction).

## Quick start with devenv

```sh
devenv shell        # enters the env: python + aria2 + unar + the libs
camoufox sync       # refresh the available-version list FIRST (see below)
camoufox fetch      # ONE TIME: downloads the stealth browser (~1GB)
python fitfast_tui.py
```

## Using it

1. Paste a `fitgirl-repacks.site` game URL into the **FitGirl page** box and
   press **Enter** (or click **Fetch links**). It loads the page past
   Cloudflare and collects every FuckingFast part link.
2. Adjust the **destination** folder if you like.
3. Click **Start** (or Tab to it and press Enter). Downloads run through aria2c
   with 16 connections per file; the table shows per-file progress, speed and
   ETA.
4. When everything finishes it auto-extracts the `.rar` set (toggleable).

Keys: **f** fetch · **s** start · **p** pause/resume · **c** cancel ·
**i** install browser · **q** quit. (Single-letter keys fire when focus isn't
in a text box — otherwise use the buttons or Tab+Enter.)

## The one NixOS gotcha: nix-ld

Camoufox downloads a prebuilt Firefox, and Playwright ships a prebuilt Node
driver. These are ordinary dynamically-linked binaries and **won't launch on
NixOS out of the box** — there's no `/lib64/ld-linux` for them to start from.

Enable [`nix-ld`](https://github.com/nix-community/nix-ld) **once** in your
system config and rebuild:

```nix
# configuration.nix
programs.nix-ld.enable = true;
```

`devenv.nix` then supplies the actual libraries via `NIX_LD_LIBRARY_PATH`, so
the browser and driver find everything they need. On a normal (non-NixOS)
distro this isn't necessary — the system loader already handles it, and
`LD_LIBRARY_PATH` (also set by `devenv.nix`) is enough.

If Fetch fails with a browser/driver launch error, `nix-ld` is almost always
the missing piece.

## Without devenv (any Linux)

```sh
python -m venv .venv && . .venv/bin/activate
pip install -r requirements.txt
# ensure aria2c and unar (or unrar) are on PATH, e.g. apt install aria2 unar
camoufox fetch
python fitfast_tui.py
```
