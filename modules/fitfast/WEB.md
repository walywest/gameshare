# FitFast web UI (direct links only)

A small password-protected web page for sharing FitFast with someone else.
They paste a `fitgirl-repacks.site` page (or `fuckingfast.co` links) and get
back the signed `https://dl.fuckingfast.co/dl/...` URLs, which need no
Cloudflare cookies.

Links are fetched over plain HTTP with a Chrome-like TLS fingerprint
(`curl_cffi`), about 0.3 s each, so a whole game takes seconds. The Camoufox
stealth browser is only a fallback, used when the site asks for a captcha. **The server downloads nothing.** The
visitor copies the links into JDownloader, aria2, a browser, or any other tool.

## Run it

```sh
devenv shell
camoufox sync && camoufox fetch     # first time only
python fitfast_web.py               # or: make web   /   devenv up
```

It listens on `0.0.0.0:8787` by default. Forward that port and allow it in the
firewall.

On the login prompt, any username works. The password is `PASS` from the
repo's `.env` file (gitignored); the server won't start without it:

```sh
echo 'PASS=choose-something-long' > .env
```

## Settings (environment variables, defaults in `devenv.nix`)

| Variable                | Default   | Meaning                                         |
|-------------------------|-----------|-------------------------------------------------|
| `FITFAST_WEB_HOST`      | `0.0.0.0` | interface to bind                               |
| `FITFAST_WEB_PORT`      | `8787`    | port                                            |
| `FITFAST_WEB_FAST_PARALLEL` | `4`   | browserless resolves at once (~0.3 s each)      |
| `FITFAST_WEB_PARALLEL`  | `2`       | fallback browsers at once (~300–500 MB RAM each)|

At most 3 requests run at once (others get "server is busy"), up to 400 links
each. Results stay in memory for 6 hours.

## Things to know

- **Plain HTTP.** The password and links are sent unencrypted. For anything
  beyond a trusted friend, put it behind a TLS reverse proxy (Caddy, nginx) or
  a tunnel (Tailscale, WireGuard).
- **Signed links expire**, so download them soon after resolving.
- **WSL2:** a router port-forward reaches Windows, not the WSL VM. Use WSL's
  mirrored networking mode, or a `netsh interface portproxy` rule on Windows.
- Logs: `~/.config/FitFast/logs/fitfast.log` (signed URLs are never logged).
