{
  description = "claude-auth-proxy - tsnet reverse proxy that injects a Claude subscription token";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = {
    self,
    nixpkgs,
    flake-utils,
    ...
  }:
    flake-utils.lib.eachDefaultSystem (
      system: let
        pkgs = nixpkgs.legacyPackages.${system};
      in {
        packages.default = pkgs.buildGoModule {
          pname = "claude-auth-proxy";
          version = "0.1.0";
          src = ./.;
          vendorHash = "sha256-c/Feb6ftVUd4f1ydKfG2PWoGWBZWO4CtK/+AoIfCMEI=";

          # Pure-Go build: no cgo, so the result is a static binary that drops
          # cleanly into a minimal systemd service.
          env.CGO_ENABLED = "0";
        };

        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
            gotools
            nil
          ];
        };
      }
    );
}
