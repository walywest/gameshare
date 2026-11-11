#!/usr/bin/env python3
"""Stealth-resolve a single fuckingfast.co URL, for JDownloader's plugin to shell out to.

Reuses the exact same Camoufox-driven Resolver as the TUI/GUI (see app/resolver.py)
so there is only one place that knows how to pass Cloudflare and click through to
the signed download URL. This script's only job is to be a thin, scriptable
process boundary: one URL in on argv, one line of JSON out on stdout.

Usage:
    python resolve_cli.py "https://fuckingfast.co/<id>#<name>.part01.rar"

Stdout on success (and only on success — nothing else is printed to stdout):
    {"signed_url": "https://dl.fuckingfast.co/dl/<token>", "filename": "<name>.part01.rar"}

On failure: prints an error message to stderr and exits 1. Nothing is written
to stdout in that case, so callers can rely on "exit 0 => stdout is valid JSON".
"""
from __future__ import annotations
import contextlib
import io
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from app.resolver import Resolver, ResolveError, valid_url  # noqa: E402


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: resolve_cli.py <fuckingfast.co URL>", file=sys.stderr)
        return 2
    url = sys.argv[1].strip()
    if not valid_url(url):
        print(f"not a fuckingfast.co link: {url}", file=sys.stderr)
        return 2

    # Camoufox/browserforge print diagnostic lines (e.g. "Skipping unknown
    # patch ...") straight to stdout during browser setup. A Java caller only
    # wants one clean JSON line on stdout, so redirect stdout to a buffer for
    # the whole resolve and forward anything it wrote to stderr instead —
    # only our own json.dumps() below ever reaches the real stdout.
    captured = io.StringIO()
    resolver = Resolver()
    try:
        with contextlib.redirect_stdout(captured):
            signed_url, filename = resolver.resolve(url)
    except (ResolveError, Exception) as e:
        noise = captured.getvalue()
        if noise:
            print(noise, file=sys.stderr, end="")
        print(f"resolve failed: {e}", file=sys.stderr)
        return 1
    finally:
        resolver.close()

    noise = captured.getvalue()
    if noise:
        print(noise, file=sys.stderr, end="")
    print(json.dumps({"signed_url": signed_url, "filename": filename}))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
