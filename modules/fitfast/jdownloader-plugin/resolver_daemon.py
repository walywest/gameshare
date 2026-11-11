#!/usr/bin/env python3
"""Persistent local resolver service for the JDownloader plugin.

A FitGirl repack is ~150-250 individual fuckingfast.co part links. Shelling
out to resolve_cli.py once per link would cold-start a whole Camoufox browser
each time — exactly the "do-it-once vs. do-it-per-item" mistake the rest of
this project is built to avoid (see app/resolver.py's docstring: the browser
earns Cloudflare's trust once and should be reused for every click-through
after that).

This script keeps ONE Resolver (one browser) alive for the process's whole
lifetime and serves resolves over a tiny localhost-only HTTP endpoint. Start
it once, leave it running, point the JDownloader plugin at it.

    python resolver_daemon.py [--port 8765]

    GET /resolve?url=<url-encoded fuckingfast.co link>
        200 {"signed_url": "...", "filename": "..."}
        400 {"error": "..."}   bad/missing url param
        502 {"error": "..."}   resolve failed (Cloudflare hiccup, captcha, etc.)

    GET /health
        200 {"ok": true}

Binds to 127.0.0.1 only — never expose this beyond localhost, the endpoint
does no auth and a resolve can be used to fetch a real signed download URL.
"""
from __future__ import annotations
import argparse
import json
import sys
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from app.resolver import Resolver, ResolveError, valid_url  # noqa: E402
from app.log import setup_logging, get_logger  # noqa: E402


class Handler(BaseHTTPRequestHandler):
    resolver: Resolver  # set on the class by main() before serving

    def _json(self, status: int, payload: dict) -> None:
        body = json.dumps(payload).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:  # noqa: N802 (stdlib naming)
        parsed = urllib.parse.urlparse(self.path)
        if parsed.path == "/health":
            self._json(200, {"ok": True})
            return
        if parsed.path != "/resolve":
            self._json(404, {"error": "unknown endpoint"})
            return

        qs = urllib.parse.parse_qs(parsed.query)
        url = (qs.get("url") or [""])[0].strip()
        if not url or not valid_url(url):
            self._json(400, {"error": f"not a fuckingfast.co link: {url!r}"})
            return

        get_logger().info("resolver_daemon: resolving %s", url)
        try:
            signed_url, filename = self.resolver.resolve(url)
        except (ResolveError, Exception) as e:
            get_logger().warning("resolver_daemon: resolve failed for %s: %s", url, e)
            self._json(502, {"error": str(e)})
            return
        self._json(200, {"signed_url": signed_url, "filename": filename})

    def log_message(self, fmt: str, *args) -> None:
        get_logger().info("resolver_daemon: " + fmt, *args)


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--port", type=int, default=8765)
    args = ap.parse_args()

    setup_logging()
    Handler.resolver = Resolver()  # one browser, held for the daemon's lifetime

    server = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    print(f"resolver_daemon listening on http://127.0.0.1:{args.port}  "
          f"(GET /resolve?url=<fuckingfast.co link>)", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        Handler.resolver.close()
        server.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
