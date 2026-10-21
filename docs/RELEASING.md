# Releasing NotiBridge

The release is fully automated: pushing a version tag builds the archives,
checksums, signature and the GitHub Release. No Apple Developer account, no
code signing certificate and no notarization are involved.

---

## What you need

- A GitHub repository with Actions enabled (the default).
- Write access to the repository, so you can push a tag.
- Optionally the repository secret `UPDATE_SIGNING_KEY` (see
  [Signing](#signing)).

## Versioning

The version lives in one place: `internal/app/service.go`.

```go
var Version = "0.1.0"
```

The Makefile reads it from there, and the release workflow refuses to publish a
tag that does not match it. So a release is:

1. Edit `var Version` in `internal/app/service.go`.
2. Commit and push.
3. Tag the commit and push the tag.

```bash
# 1. bump the version
sed -i '' 's/var Version = "0.1.0"/var Version = "0.2.0"/' internal/app/service.go

# 2. verify
make check
make bundle

# 3. commit, tag and push
git add internal/app/service.go
git commit -m "Release 0.2.0"
git tag v0.2.0
git push origin main
git push origin v0.2.0
```

The push of the tag triggers `.github/workflows/release.yml`.

Versions follow [SemVer](https://semver.org/): `MAJOR.MINOR.PATCH`, with an
optional `-suffix` for pre-releases (`v0.2.0-rc.1`). Pre-releases are tagged
but marked as such in GitHub, and the in-app updater ignores them.

## What the release workflow does

`.github/workflows/release.yml` runs on every `v*.*.*` tag:

1. Reads the version from the tag and checks it against `internal/app/service.go`.
2. Builds `NotiBridge_<version>_arm64.zip` and
   `NotiBridge_<version>_amd64.zip` (unsigned, ad-hoc signed, `.zip` archives).
3. Writes `SHA256SUMS` with the checksum of every archive.
4. Signs `SHA256SUMS` with the Ed25519 key when `UPDATE_SIGNING_KEY` is set.
5. Verifies the archives and the embedded version.
6. Creates the GitHub Release with the archives, `SHA256SUMS` and
   `SHA256SUMS.sig`, and a short installation note.

No secrets other than `GITHUB_TOKEN` (automatic) are needed for an unsigned
release.

## Signing

The updater can verify that a release was published by the project, not merely
that the download was not corrupted. That requires an Ed25519 key pair:

- the **public key** is embedded into the application at build time, and
- the **private key** signs the `SHA256SUMS` file of every release.

Generate the key pair once, on your own machine:

```bash
# private key (keep this secret, never commit it)
openssl genpkey -algorithm ed25519 -out update_key.pem

# the value to store as the UPDATE_SIGNING_KEY repository secret
openssl pkey -in update_key.pem -outform DER | tail -c 32 | base64 | tr -d '\n'

# the public key to embed (the workflow derives it itself from the secret)
go run ./scripts/sign_release pubkey update_key.pem
```

Store the base64 seed as the repository secret **UPDATE_SIGNING_KEY**
(*Settings → Secrets and variables → Actions → New repository secret*). The
workflow derives the public key from it and passes it to the build, so the
public key never has to be stored separately.

Without the secret the workflow still publishes `SHA256SUMS` — but without a
signature. Installed builds that embed a public key then refuse to update,
because an unsigned checksum file proves nothing about the publisher.

**Never change the key.** See the warning in
[docs/UPDATING.md](UPDATING.md#keeping-the-signing-key).

To sign a release by hand (for example to repair a failed workflow run):

```bash
go run ./scripts/sign_release sign update_key.b64 SHA256SUMS
gh release upload v0.2.0 SHA256SUMS.sig
```

## Reproducing a release locally

```bash
make check                      # gofmt, vet, tests
make bundle                     # dist/NotiBridge.app + dist/NotiBridge_<version>_arm64.zip
make package-amd64              # dist/NotiBridge_<version>_amd64.zip
make package-universal          # one archive with both architectures
```

`scripts/package_app.sh` does the whole job:

- generates `assets/icons/NotiBridge.icns` from `image.png` when it is missing,
- builds for the requested architecture (`arm64`, `amd64` or `universal`),
- assembles `NotiBridge.app` with the icon, the bundle metadata and the
  version,
- signs the bundle ad-hoc (`codesign --sign -`),
- zips it with `ditto` so the bundle keeps its structure and permissions.

The binary is built with the version injected:

```
-ldflags "-X github.com/spdeepak/notibridge/internal/app.Version=<version>"
```

## Regenerating the icon

`image.png` in the repository root is the icon source of truth and is never
modified by the build. (Its contents are JPEG data despite the `.png` name —
the generator decodes by content, not by extension.)

```bash
make icons
```

This writes `assets/icons/NotiBridge.icns` and the intermediate
`assets/icons/NotiBridge.iconset/` (git-ignored). The `.icns` file is
committed, so a normal build does not need `iconutil`.

## Checklist before pushing a tag

- [ ] `internal/app/service.go` contains the new version
- [ ] `gofmt -l .` is empty, `go vet ./...` is clean, `go test ./...` passes
- [ ] `make bundle` produces a working `dist/NotiBridge.app`
- [ ] the release notes describe real user-visible changes
- [ ] the `UPDATE_SIGNING_KEY` secret is present (unless you deliberately
      publish an unsigned release)

## After the release

1. Watch the **Release** workflow in the Actions tab.
2. Download one of the published archives and check that it opens.
3. Install it on a second Mac and confirm the Gatekeeper warning is the one
   described in [docs/INSTALL.md](INSTALL.md).
