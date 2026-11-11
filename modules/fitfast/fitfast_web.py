"""FitFast Downloader — web UI entry point (link resolver only, no downloads).

Run with:  python fitfast_web.py [--host 0.0.0.0] [--port 8787]
"""
from app.web import main

if __name__ == "__main__":
    raise SystemExit(main())
