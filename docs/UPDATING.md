# Updating NotiBridge

NotiBridge can check the project's GitHub releases page for a newer version,
download it, verify it, and install it.

---

## How it works

```
Check  →  Download  →  Verify  →  Stage  →  Install
                                   (still the old version)
```

Each stage is separate on purpose:

1. **Check** — the app asks the GitHub API for the newest stable release that
   is newer than the running version and publishes a build for this Mac's
   architecture.
2. **Download** — the archive for your architecture is downloaded together with
   the release's `SHA256SUMS` file.
3. **Verify** — the archive's SHA-256 checksum must match the published one.
   When the build you are running embeds the project's Ed25519 **public key**,
   the checksum file must also carry a valid signature made with the matching
   **private key**; otherwise the update is refused.
4. **Stage** — the verified archive is unpacked next to the running app. The
   old version keeps running: a downloaded update is *not* an installed one.
5. **Install** — a small helper process waits for NotiBridge to quit, swaps
   the application bundle, and starts the new version.

**Your settings and your bot token are never touched.** They live in
`~/Library/Application Support/NotiBridge/` and in the macOS Keychain, both
outside the application bundle.

## What the checks mean

| Check | What it proves | What it does **not** prove |
|---|---|---|
| SHA-256 checksum | The download arrived intact and matches what the release published | Who published it — anyone can publish a checksum for their own file |
| Ed25519 signature over `SHA256SUMS` | The checksum file was signed by the holder of the project's private key | That the code in the archive is good — only that it is the release the maintainer signed |

A checksum alone is therefore *not* proof of publisher identity. Builds that
embed the public key refuse to update when the release carries no valid
signature, so a signed release keeps working for everyone.

## Checking for updates

Updates are only checked **when you ask** — there is no background polling, so
NotiBridge makes no network requests other than to the Telegram Bot API.

- **Menu bar** → **Check for updates…** (the settings page opens on the
  *Updates* section).
- **Settings page** → *Updates* → **Check for updates**.

The *Updates* section distinguishes the states honestly:

| State | Meaning |
|---|---|
| *Up to date* | The running version is the newest release |
| *Version X available* | A newer release was found; nothing has been downloaded yet |
| *Downloaded and verified* | The new version is staged and ready to install — the running version is still the old one |
| *Installed* | The bundle was replaced and the app was restarted |

## Installing an update

1. **Check for updates** → **Download update**. The app downloads, verifies and
   stages the new version.
2. **Install update**. NotiBridge asks you to quit; the helper swaps the bundle
   and reopens the app.

If the application folder is not writable (for example when the app was moved
somewhere unusual, or installed by an administrator), NotiBridge does **not**
pretend the update was installed. It tells you to update manually and shows the
steps.

## Updating manually

Download the newer archive from the
[releases page](https://github.com/spdeepak/Notibridge/releases), quit
NotiBridge, replace `NotiBridge.app` in `/Applications` with the new one, and
open it again. Your settings and token are kept: they live outside the bundle.

```bash
cd ~/Downloads
unzip NotiBridge_<version>_arm64.zip
# quit NotiBridge first
rm -rf /Applications/NotiBridge.app
mv NotiBridge.app /Applications/
open /Applications/NotiBridge.app
```

## Troubleshooting the updater

| Message | Meaning | Fix |
|---|---|---|
| `the GitHub API rate limit was reached` | Too many unauthenticated API requests from this IP | Wait a few minutes and check again |
| `it publishes no arm64 build` | The release has no build for your Mac | Install the matching archive manually |
| `checksum mismatch for …` | The download does not match the published checksum | Do not install; re-download and report it |
| `the release signature is not valid for the embedded public key` | The release is not signed by the key your build expects | Do not install; report it on the issue tracker |
| `release X publishes no signature and the embedded public key requires one` | The release was published without a signature while your build expects one | Do not install; report it on the issue tracker |
| `manual installation required` | The application folder is not writable | Update manually as described above |

The update helper writes its own log to
`/tmp/notibridge-update.log` when it cannot complete the swap.

## Keeping the signing key

The private key that signs release checksum files lives in the repository
secret `UPDATE_SIGNING_KEY` and must **stay the same for every release**.

Once a release has been signed, an installed build refuses any release without
a matching signature. Rotating the key therefore breaks in-place updates for
the versions that embed the old public key: those users must update manually
once, from a release that carries a signature made with the *new* key — which
they cannot verify, since their build embeds the old one. **Do not rotate the
key.** See [docs/RELEASING.md](RELEASING.md) for how it is generated and used.
