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

## Quick start

```bash
# 1. bump the version (the one source of truth)
sed -i '' 's/var Version = "0.1.0"/var Version = "0.2.0"/' internal/app/service.go

# 2. check locally
make check && make bundle

# 3. commit, push, tag, push the tag
git commit -am "Release 0.2.0"
git push origin release-phase
git tag v0.2.0
git push origin v0.2.0      # <- this triggers the workflow
```

Pushing the tag starts the **Release** workflow on
<https://github.com/spdeepak/Notibridge/actions>, which builds the archives,
writes the checksums, signs them and publishes the GitHub Release. Watch it in
the Actions tab — the workflow uses the runner's own `gh`, so the GitHub CLI
does not have to be installed on your machine.

## Releasing from the GitHub web UI (no terminal)

Everything below happens in the browser. The version bump must exist in the
tagged commit, so the first step is a commit made through GitHub's file
editor.

1. **Bump the version.** Open `internal/app/service.go` on the `release-phase`
   branch on GitHub → click the pencil → change
   `var Version = "0.1.0"` to the new version → *Commit changes* directly to
   `release-phase`.
2. **Let CI check it.** The *CI* workflow runs on that push
   (`release-phase` is in its branch list). Wait for it to go green. You can
   also start it by hand: *Actions → CI → Run workflow → branch
   `release-phase`*.
3. **Create a draft release.** *Releases → Draft a new release*. Choose
   `release-phase` in the branch dropdown, type the new tag (for example
   `v0.2.0`) in the tag field, add a title, then *Create draft*.
   **The tag is created here — that tag push is what starts the workflow.**
4. **Watch the workflow finish.** *Actions → Release*. It builds the archives,
   writes the checksums, signs them, uploads everything to the draft release
   and publishes it.
5. The release appears on the Releases page with the archives attached.

Do not press *Publish release* yourself in step 3 — the workflow publishes it
once the assets are attached. (If you do publish it early, the workflow fills
in the existing release instead of failing; the assets land on the release you
published.)

Note that the version check in the workflow compares the tag with the version
in the tagged commit, so the bump from step 1 has to be part of the commit the
tag points at.

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
git push origin release-phase      # the branch you release from
git tag v0.2.0
git push origin v0.2.0             # <- this triggers the workflow
```

The push of the tag triggers `.github/workflows/release.yml`. Push the commit
**before** the tag: the workflow checks out the tagged commit and compares its
version against the tag.

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

# the base64 seed: this is the value of the UPDATE_SIGNING_KEY secret, and
# the key file that scripts/sign_release reads
openssl pkey -in update_key.pem -outform DER | tail -c 32 | base64 | tr -d '\n' > update_key.b64

# the public key to embed (the workflow derives it itself from the secret)
go run ./scripts/sign_release pubkey update_key.b64
```

`sign_release` reads a **base64** key file (the 32-byte seed or the 64-byte
private key) — it does not read PEM files.

Store the content of `update_key.b64` as the repository secret
**UPDATE_SIGNING_KEY** (*Settings → Secrets and variables → Actions → New
repository secret*). The workflow derives the public key from it and passes it
to the build, so the public key never has to be stored separately.

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

## If the release shows only "Source code (zip)"

Every GitHub Release offers *Source code (zip)* and *Source code (tar.gz)*.
Those are snapshots of the **repository**, not the application — the
application archives (`NotiBridge_<version>_arm64.zip`) are attached by the
workflow.

If only the source archives are offered, the workflow uploaded nothing: open
the **Release** run in the Actions tab and read the failed step. The usual
cause is a tag that disagrees with the version in `internal/app/service.go` —
the workflow refuses to publish a release whose version would differ from the
source, so the version has to be bumped *before* the tag is created:

```bash
sed -i '' 's/var Version = "1.0.0"/var Version = "1.0.1"/' internal/app/service.go
git commit -am "Release 1.0.1" && git push origin main
git tag v1.0.1 && git push origin v1.0.1
```

The same happens if someone deletes the uploaded assets in the release editor.
*Insights → Audit log* shows who changed the release. To put them back,
re-run the workflow by pushing the tag again — note that **deleting a tag
also deletes its release**, so the workflow recreates the release from
scratch (same tag, same notes, freshly uploaded assets):

```bash
git push origin :refs/tags/v1.0.1   # delete the tag
git push origin v1.0.1              # push it again -> the workflow re-runs
```
