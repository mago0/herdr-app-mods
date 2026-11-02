{ lib, buildGoModule }:

buildGoModule {
  pname = "herdr-pr";
  version = lib.trim (lib.fileContents ./VERSION);

  src = lib.fileset.toSource {
    root = ./.;
    fileset = lib.fileset.unions [
      ./go.mod
      ./VERSION
      ./cmd
      ./internal
      ./systemd
      ./README.md
    ];
  };

  # Standard library only.
  vendorHash = null;
  subPackages = [ "cmd/herdr-pr" ];

  postInstall = ''
    share=$out/share/herdr-app-mods/pr-watch
    mkdir -p $share
    cp -r systemd README.md $share/
    if [ -d mod ]; then cp -r mod $share/; fi
  '';

  meta = {
    description = "Records which Herdr agent owns which GitHub PR and wakes it when the PR needs action";
    homepage = "https://github.com/mago0/herdr-app-mods";
    license = lib.licenses.mit;
    mainProgram = "herdr-pr";
    platforms = lib.platforms.unix;
  };
}
