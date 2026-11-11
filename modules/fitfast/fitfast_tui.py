"""FitFast Downloader — terminal UI entry point (Linux / macOS / Nix).

Run with:  python fitfast_tui.py
"""
from app.tui import main

if __name__ == "__main__":
    raise SystemExit(main())
