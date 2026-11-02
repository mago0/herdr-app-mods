{
  description = "Herdr.app mods: pr-watch, which wakes the agent that owns a PR when it needs action";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAll (pkgs: rec {
        pr-watch = pkgs.callPackage ./pr-watch/package.nix { };
        default = pr-watch;
      });
    };
}
