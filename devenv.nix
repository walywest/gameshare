{ pkgs, lib, ... }:

# One shell for the whole homelab: the game-share server and its gs client, and
# every service declared in homelab.json (FitFast for now). The server starts
# those services itself, each inside `devenv shell`, so this file is what they
# run with and a change here reaches them on their next restart.

let
  port = 8080;

  # Camoufox's patched Firefox and Playwright's node driver are prebuilt,
  # dynamically linked binaries. On NixOS they run through nix-ld
  # (programs.nix-ld.enable = true; see modules/fitfast/NIXOS.md), which reads
  # NIX_LD_LIBRARY_PATH. Other distros use their own libraries. LD_LIBRARY_PATH
  # is left alone on purpose: Nix's libraries need a newer glibc than an
  # Ubuntu's, and Firefox dies at launch when it mixes the two.
  runtimeLibs = with pkgs; [
    stdenv.cc.cc.lib
    zlib glib nss nspr dbus atk cups libdrm expat libxkbcommon
    gtk3 pango cairo gdk-pixbuf
    fontconfig freetype
    alsa-lib mesa
    xorg.libX11 xorg.libXcomposite xorg.libXdamage xorg.libXext
    xorg.libXfixes xorg.libXrandr xorg.libXrender xorg.libxcb
    xorg.libXtst xorg.libXi
  ];

in
{
  packages = [
    pkgs.git
    pkgs.aria2 # FitFast downloads with it
    pkgs.unar  # and extracts with it
    (pkgs.buildGoModule {
      pname = "game-share";
      version = "1";
      src = ./server;
      vendorHash = null;
    })
  ];

  languages.python = {
    enable = true;
    venv = {
      enable = true;
      requirements = builtins.readFile ./modules/fitfast/requirements.txt;
    };
  };

  env.SERVE_PORT = toString port;
  env.NIX_LD_LIBRARY_PATH = lib.makeLibraryPath runtimeLibs;
  env.FITFAST_WEB_FAST_PARALLEL = "4"; # browserless resolves at once (~0.3s each)
  env.FITFAST_WEB_PARALLEL = "2";      # fallback browsers at once (~300-500MB RAM each)
  dotenv.disableHint = true;           # .env holds secrets the apps read themselves

  # Plain HTTP on purpose: LAN only, no TLS/auth overhead.
  # Folder comes from the first argument, then $SERVE_DIR, then ./games.
  # With --cd, the server can update itself: it pulls, and exits with 75 to be
  # rebuilt from the repo and restarted here. A build that fails keeps the last
  # good binary. Services restart on their own without leaving this loop.
  scripts.serve.exec = ''
    cd_mode=
    if [[ ''${1:-} == --cd ]]; then cd_mode=1; shift; fi
    dir=''${1:-''${SERVE_DIR:-$DEVENV_ROOT/games}}
    mkdir -p "$dir"
    lan-ip
    export GAMESHARE_REPO=$DEVENV_ROOT
    [[ -n $cd_mode ]] || exec game-share "$SERVE_PORT" "$dir"

    bin=$DEVENV_STATE/game-share-cd
    while :; do
      if CGO_ENABLED=0 GOTOOLCHAIN=local ${pkgs.go}/bin/go build -C "$DEVENV_ROOT/server" -o "$bin.new" .; then
        mv -f "$bin.new" "$bin"
      elif [[ -x $bin ]]; then
        echo "serve: build failed, running the last good build" >&2
      else
        exit 1
      fi
      GAMESHARE_CD=$DEVENV_ROOT GAMESHARE_GO=${pkgs.go}/bin/go "$bin" "$SERVE_PORT" "$dir"
      code=$?
      [[ $code == 75 ]] || exit $code
      echo "serve: restarting with the update" >&2
    done
  '';

  # Built on first use, so a machine that only serves never compiles the client.
  # go build is a no-op until client/ changes.
  scripts.gs.exec = ''
    bin=$DEVENV_STATE/gs
    [[ -x $bin ]] || echo "Building gs, first run only..." >&2
    CGO_ENABLED=0 GOTOOLCHAIN=local ${pkgs.go}/bin/go build -C "$DEVENV_ROOT/client" -o "$bin" . || exit 1
    exec "$bin" "$@"
  '';

  scripts.lan-ip.exec = ''
    ip -4 -o addr show scope global | awk '{print $2": http://"substr($4,1,index($4,"/")-1)":'"$SERVE_PORT"'"}'
  '';

  processes.game-share.exec = "serve";

  enterShell = ''
    echo "serve    share a folder on :$SERVE_PORT and run the homelab services (serve [--cd] <folder>)"
    echo "         the control page is http://<server>:$SERVE_PORT/"
    echo "gs       terminal client, built on first use (gs http://<server>:$SERVE_PORT)"
    echo "lan-ip   print this machine's URLs"
  '';
}
