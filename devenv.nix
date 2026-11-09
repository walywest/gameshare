{ pkgs, ... }:

let
  port = 8080;

in
{
  packages = [
    pkgs.git
    (pkgs.buildGoModule {
      pname = "game-share";
      version = "1";
      src = ./server;
      vendorHash = null;
    })
  ];

  env.SERVE_PORT = toString port;

  # Plain HTTP on purpose: public files, no TLS/auth overhead.
  # Folder comes from the first argument, then $SERVE_DIR, then ./games.
  # With --cd, gs can update the server: it pulls, and exits with 75 to be rebuilt
  # from the repo and restarted here. A build that fails keeps the last good binary.
  scripts.serve.exec = ''
    cd_mode=
    if [[ ''${1:-} == --cd ]]; then cd_mode=1; shift; fi
    dir=''${1:-''${SERVE_DIR:-$DEVENV_ROOT/games}}
    mkdir -p "$dir"
    lan-ip
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
    echo "serve    share a folder on :$SERVE_PORT (serve [--cd] <folder>)"
    echo "gs       terminal client, built on first use (gs http://<server>:$SERVE_PORT)"
    echo "lan-ip   print this machine's URLs"
  '';
}
