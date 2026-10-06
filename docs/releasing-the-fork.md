# Releasing this fork

This fork is `wobsoriano/techo5-muse`. It adds [Muse](muse.md) to TECHO5 and publishes its own releases
for the Echo Show, signed with its own key. [building.md](building.md#releases-maintainer) describes
upstream's release, which runs from Windows. This page is the same job from a Mac with no Docker and
one Show on the network.

`v1.0.0-muse.1` was cut this way on 2026-10-06. Steps marked **not verified** were worked out from the
scripts and have not been run.

## What differs from upstream

- The daemon's updater reads `https://github.com/wobsoriano/techo5-muse/releases`
  (`echod/internal/update/releases_cronos.go`) and trusts this fork's public key
  (`echod/internal/update/trust.go`, and the same key as `RELEASE_KEY` in `tools/techo5lib.py`).
- `tools/install-show.py` (`REPO`), `tools/release.ps1` and the attestation check in
  `tools/linux/deploy-rootfs.sh` name this repository.
- The Dot and the Spot are not released here. `releases_dot.go` and `releases_spot.go` still name
  upstream's repositories, and a `-tags dot` or `-tags spot` build of this fork refuses their
  manifests because the key does not match. Do not install one.

A device that runs upstream's image trusts upstream's key, so it never takes a release of this
fork by itself. Move a device over once by hand, with `deploy-rootfs.sh --install` (step 3).

Until the fork has a release, a device's update check gets a 404 for `manifest.json`. `get` in
`echod/internal/update/manifest.go` returns the error, `Check` in
`echod/internal/feature/firmware/firmware.go` logs `checking for an update failed` and stops, and
Home Assistant is offered nothing.

## The signing key

The private key is a file that holds the base64 of a 32-byte ed25519 seed, which is what
`mkmanifest -sign-key` reads. It lives outside the repository at
`~/.config/techo5-release/sign.key` (mode 0600, in a directory of mode 0700). `sign.pub` beside it
is the public half.

Back the key up somewhere that is not this Mac. Without it you cannot publish an update that an
installed device accepts, and the only way forward is a new key and a reinstall by hand on every
device. Never commit it, and do not make it a GitHub Actions secret.

## What a release carries

| File | Who reads it | How it is made |
|---|---|---|
| `echod-arm` | the manifest's `binaries.arm` | `deploy-rootfs.sh` builds it as `bin/echod-arm` |
| `techo5-rootfs-<version>.tar.gz` | every Show, to update its spare slot | built on the Show by `deploy-rootfs.sh` |
| `manifest.json`, `manifest.json.sig` | every Show and `install-show.py` | `mkmanifest` |
| `SHA256SUMS` | people, by hand | `shasum` |
| `techo5-boot-<version>.img` | `install-show.py`, for a new unit only | optional, see [New units](#new-units) |

Three helpers go into the root filesystem from `bin/` when they are there.

- `bin/techo5-pico-arm`, the built-in voice. `tools/linux/build-pico.sh` builds it on a Mac with
  zig (`brew install zig`).
- `bin/techo5-aec-arm` and `bin/techo5-librespot-arm`, the echo canceller and the Spotify receiver.
  Neither builds on a Mac. Copy them out of an upstream release's root filesystem, where they are
  `usr/local/bin/techo5-aec` and `usr/local/bin/techo5-librespot`.

## Cut a release

Use a version of the form `vX.Y.Z-muse.N`, where `vX.Y.Z` is the upstream release the branch is
based on and `N` goes up with each release of the fork. A device ranks `v1.0.0-muse.2` above
`v1.0.0-muse.1`, and `v1.0.1-muse.1` above both (`echod/internal/update/version.go`). How Home
Assistant's update card shows a version with a suffix is **not verified**.

1. Check the tree. Run the tests on Linux or let the tag's workflow run them, since
   `go test ./...` does not build on macOS.

   ```sh
   cd echod
   go test -race -count=1 ./internal/config/ ./internal/lib/... ./internal/update/ ./cmd/mkmanifest/
   GOOS=linux GOARCH=arm GOARM=7 go vet ./...
   cd ..
   ```

2. Make sure the three helpers are in `bin/`.

   ```sh
   ls -l bin/techo5-pico-arm bin/techo5-aec-arm bin/techo5-librespot-arm
   ```

3. Build the daemon and the root filesystem, and install it on the Show to try it. The root
   filesystem is built on the Show itself over SSH.

   ```sh
   V=v1.0.0-muse.1
   HOST=<the Show's address> tools/linux/deploy-rootfs.sh --version $V --install --reboot
   ```

   Try the release on the device now. Say the wake word, finish a turn, and check that the device
   comes back after a reboot.

4. Bring the tarball back. It is the file the device just installed. The device has no sftp, so
   copy it over plain SSH and compare its sha256 with the device's.

   ```sh
   ssh root@<the Show's address> "cat /data/techo5-linux/techo5-rootfs-$V.tar.gz" > bin/techo5-rootfs-$V.tar.gz
   tar -tzf bin/techo5-rootfs-$V.tar.gz | grep -E '^(\./)?vendor/.' && echo "STOP: vendor tree inside"
   ```

   A root filesystem that carries a vendor tree must not be published. The tree is Amazon's.

5. Write and sign the manifest. This command was run against stand-in files with the fork's key,
   and `tools/techo5lib.py` accepted the signature.

   ```sh
   R=https://github.com/wobsoriano/techo5-muse/releases
   (cd echod && go run ./cmd/mkmanifest -version $V -title "TECHO5 $V" -notes "<what changed>" \
     -release-url $R/tag/$V -from $R/download/$V \
     -arm ../bin/echod-arm -rootfs-arm ../bin/techo5-rootfs-$V.tar.gz \
     -out ../bin/manifest.json -sign-key ~/.config/techo5-release/sign.key)
   (cd bin && shasum -a 256 echod-arm techo5-rootfs-$V.tar.gz manifest.json manifest.json.sig > SHA256SUMS)
   ```

6. Tag and publish.

   ```sh
   git tag $V && git push fork $V
   gh release create $V --repo wobsoriano/techo5-muse --title $V --notes "<what changed>" --latest \
     bin/echod-arm bin/techo5-rootfs-$V.tar.gz bin/manifest.json bin/manifest.json.sig bin/SHA256SUMS
   ```

   `--latest` matters. Devices on the stable channel read `releases/latest`, and GitHub does not
   count a prerelease as latest.

7. Move the dev channel too, if any device follows it. Devices on the dev channel read the `dev`
   release's manifest. Create that release once, then replace its two files with each release.

   ```sh
   gh release upload dev bin/manifest.json bin/manifest.json.sig --repo wobsoriano/techo5-muse --clobber
   ```

8. Check from the outside.

   ```sh
   curl -sLo /tmp/m.json $R/latest/download/manifest.json && grep '"version"' /tmp/m.json
   ```

   Then ask a Show that runs the previous release of the fork to check for an update, from its
   update entity in Home Assistant.

## New units

`install-show.py` flashes a boot image, and it uses only one that a manifest signed by this fork's
key names. The Muse branch does not change the kernel, so upstream's boot image is the right one.
Two ways to give the installer one. The first is **not verified**; the second is what
`v1.0.0-muse.1` did, for the Show 5 2nd gen only:

- Pass it yourself. Download `techo5-boot-<version>.img` from an upstream release and run
  `python3 tools/install-show.py --boot <that file>`.
- Publish it with the fork's release. Copy the same file to `bin/techo5-boot-$V.img`, add
  `-asset ../bin/techo5-boot-$V.img` to the `mkmanifest` command, and add the file to
  `gh release create` and to `SHA256SUMS`. The image holds a GPL-2.0 kernel, and
  [NOTICE](../NOTICE) already says where its source is.

## Could GitHub Actions build it all?

Partly, today. `.github/workflows/build.yml` runs on a `v*.*.*` tag, tests all three device builds
on Linux and builds an attested `echod-arm`. To ship that binary in place of a local build, push
the tag first, then:

```sh
gh run download --repo wobsoriano/techo5-muse -n techo5-$V -D bin
PREBUILT_DAEMON=bin/echod-arm HOST=<the Show's address> tools/linux/deploy-rootfs.sh --version $V --install
```

The tag's workflow ran and passed for `v1.0.0-muse.1`, which is the only place the three device
builds are tested on Linux. Shipping its binary in place of a local build is **not verified**.

The root filesystem could be built on a runner too. `deploy-rootfs.sh --out` builds it on x86_64
Linux with `qemu-user-static`, `binfmt-support` and Alpine's static `apk`, and the same runner can
build all three helpers. No workflow does that yet, here or upstream. Signing stays on your own
computer either way.

The `dashcast image` workflow also runs on a release tag and pushes to upstream's container
registry name, which this fork cannot write to. Turn that workflow off in the fork's Actions
settings, or expect it to fail on every tag.
