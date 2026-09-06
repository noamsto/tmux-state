{
  description = "Fast, smart tmux state persistence — replaces resurrect/continuum.";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-parts.url = "github:hercules-ci/flake-parts";
    git-hooks-nix.url = "github:cachix/git-hooks.nix";
    git-hooks-nix.inputs.nixpkgs.follows = "nixpkgs";

    # tmux at a fixed upstream rev for the next-3.8 work. internal/triggers
    # renders a different fragment at 3.8 (monitor hooks — `set-hook -B`), and
    # nixpkgs tmux is still 3.7b, so the integration tests would never exercise
    # that branch against a real server. Kept at the same rev lazytmux pins so
    # both projects test the same tmux. Bump: repoint rev, then
    # `nix flake lock --update-input tmux-upstream`.
    tmux-upstream = {
      url = "github:tmux/tmux/d5afb67a81d8a30379e0d4186ec4b968244393bf";
      flake = false;
    };
  };

  outputs = inputs @ {flake-parts, ...}: let
    # autoreconfHook and bison are already in nixpkgs tmux's nativeBuildInputs,
    # so overriding src to a raw git checkout (no pre-generated configure) just
    # works. The version must be a substring of `tmux -V` output ("tmux
    # next-3.8") for the versionCheckHook to pass — and tmux.ParseVersion reads
    # that same string.
    # --disable-asan: ASan's runtime deadlocks during init on macOS 26
    # (llvm/llvm-project#200447), hanging every tmux call before main().
    # --enable-jemalloc (darwin only): with ASan off, configure requires an
    # explicit choice, and jemalloc's calloc(3) zeroes reliably for the complex
    # codepoints tmux renders.
    mkTmux = pkgs:
      pkgs.tmux.overrideAttrs (old: {
        version = "next-3.8";
        src = inputs.tmux-upstream;
        configureFlags =
          old.configureFlags
          ++ ["--disable-asan"]
          ++ pkgs.lib.optionals pkgs.stdenv.isDarwin ["--enable-jemalloc"];
        buildInputs = old.buildInputs ++ pkgs.lib.optionals pkgs.stdenv.isDarwin [pkgs.jemalloc];
      });
  in
    flake-parts.lib.mkFlake {inherit inputs;} {
      imports = [inputs.git-hooks-nix.flakeModule];

      systems = ["x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin"];

      perSystem = {
        config,
        pkgs,
        lib,
        self',
        ...
      }: {
        pre-commit.settings.hooks = {
          gofmt.enable = true;
          govet = {
            enable = true;
            # integration_test.go has //go:build integration; without the tag the
            # root package has no buildable files, which trips `go vet -C .`.
            excludes = ["^integration_test\\.go$"];
          };
          golangci-lint = {
            enable = true;
            excludes = ["^integration_test\\.go$"];
          };
          typos.enable = true;
          check-merge-conflicts.enable = true;
          trim-trailing-whitespace.enable = true;
        };

        devShells.default = pkgs.mkShell {
          inherit (config.pre-commit) shellHook;
          packages = config.pre-commit.settings.enabledPackages ++ [
            pkgs.go
            pkgs.gopls
            pkgs.gotools
            pkgs.golangci-lint
            (mkTmux pkgs)
            pkgs.fzf
            pkgs.sqlite
          ];
        };

        packages = {
          default = pkgs.buildGoModule {
            pname = "tmux-remux";
            version = "0.4.0";
            src = ./.;
            vendorHash = "sha256-E2vegUZYWbIBSA4O6GprrpiXLg6dpNXyXxtwuVMkVCo=";
            subPackages = ["cmd/tmux-remux"];
            doCheck = true;
            meta = {
              description = "Fast, smart tmux state persistence";
              mainProgram = "tmux-remux";
              license = lib.licenses.mit;
            };
          };
        };

        apps.default = {
          type = "app";
          program = "${self'.packages.default}/bin/tmux-remux";
        };
      };
    };
}
