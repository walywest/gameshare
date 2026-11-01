{ pkgs, ... }:

let
  port = 8080;
in
{
  packages = [
    (pkgs.buildGoModule {
      pname = "game-share";
      version = "1";
      src = ./server;
      vendorHash = null;
    })
    (pkgs.buildGoModule {
      pname = "gs";
      version = "1";
      src = ./client;
      vendorHash = "sha256-rp0wYd9JLN3KL3W++Sjcebsbbo7Ru8OsTi70nbiO6AI=";
    })
  ];

  env.SERVE_PORT = toString port;

  # Plain HTTP on purpose: public files, no TLS/auth overhead.
  # Folder comes from the first argument, then $SERVE_DIR, then ./games.
  scripts.serve.exec = ''
    dir=''${1:-''${SERVE_DIR:-$DEVENV_ROOT/games}}
    mkdir -p "$dir"
    lan-ip
    exec game-share "$SERVE_PORT" "$dir"
  '';

  scripts.lan-ip.exec = ''
    ip -4 -o addr show scope global | awk '{print $2": http://"substr($4,1,index($4,"/")-1)":'"$SERVE_PORT"'"}'
  '';

  processes.game-share.exec = "serve";

  enterShell = ''
    echo "serve    share a folder on :$SERVE_PORT (serve <folder>)"
    echo "gs       terminal client (gs http://<server>:$SERVE_PORT)"
    echo "lan-ip   print this machine's URLs"
  '';
}
