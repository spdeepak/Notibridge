# Installing NotiBridge

NotiBridge is distributed as a plain `.zip` containing `NotiBridge.app`.

> **The released app is not signed with an Apple Developer ID and is not
> notarized.** macOS will therefore show a warning the first time you open it.
> That is expected and is explained in [Opening the app](#opening-the-app-for-the-first-time).

---

## 1. Pick the right build

| Mac | File |
|---|---|
| Apple Silicon (M1/M2/M3/M4/M5) | `NotiBridge_<version>_arm64.zip` |
| Intel | `NotiBridge_<version>_amd64.zip` |

Check which one you need with **Apple menu → About This Mac** ("Chip"). If the
release page also offers `NotiBridge_<version>_universal.zip`, that one runs on
both.

## 2. Download and verify

1. Open the [releases page](https://github.com/spdeepak/Notibridge/releases) and
   download the `.zip` for your Mac.
2. Verify the download against the `SHA256SUMS` file published with the
   release:

   ```bash
   cd ~/Downloads
   shasum -a 256 NotiBridge_0.1.0_arm64.zip
   # compare the output with the matching line in SHA256SUMS
   ```

   A matching checksum proves the file arrived intact and was not changed in
   transit. **It does not prove who published it** — anyone can publish a file
   together with its own checksum. When the release also contains
   `SHA256SUMS.sig`, the in-app updater verifies the checksum file against the
   public key embedded in the application, which does establish the publisher
   (see [docs/UPDATING.md](UPDATING.md)).

3. Unzip it:

   ```bash
   unzip NotiBridge_0.1.0_arm64.zip
   ```

## 3. Install

Move `NotiBridge.app` into your **Applications** folder:

```bash
mv NotiBridge.app /Applications/
```

The app lives in the menu bar only (no Dock icon). It starts when you open it;
there is no login item yet — add `NotiBridge.app` under
**System Settings → General → Login Items** if you want it to start with the
Mac.

## 4. Grant Full Disk Access

On macOS 15 (Sequoia) and later the Notification Center database is protected
by TCC, so NotiBridge needs **Full Disk Access** to read notifications:

1. Open **System Settings → Privacy & Security → Full Disk Access**.
2. Add **NotiBridge** (the `.app` in `/Applications`) and enable the switch.
3. **Quit NotiBridge completely** (menu bar icon → Quit NotiBridge) and open it
   again — TCC grants only apply to newly launched processes.

Full Disk Access is the only permission NotiBridge asks for. It never attempts
to work around TCC; when access is denied it tells you so and points here.

## 5. Configure Telegram

1. Create a bot with [@BotFather](https://t.me/BotFather) and copy the token.
2. Send `/start` to the bot — bots cannot message you first.
3. Find your chat ID (open `https://api.telegram.org/bot<TOKEN>/getUpdates` and
   read `"chat":{"id":...}`).
4. Open **NotiBridge → Settings…**, paste token and chat ID, tick *Forwarding
   enabled*, and **Save settings**.
5. Press **Test connection** and then **Send test message**.

---

## Opening the app for the first time

Because the app is unsigned and unnotarized, Gatekeeper blocks the first
launch with *"NotiBridge cannot be opened because the developer cannot be
verified"*.

**Use Apple's documented per-app approval flow:**

1. In the Finder, **Control-click** (or right-click) `NotiBridge.app` →
   **Open**.
2. Confirm **Open** in the dialog. macOS remembers the approval for that app.

If the app will not open that way (for example because it was moved after the
first attempt), open **System Settings → Privacy & Security**; a section
reports that the app *"was blocked to protect your Mac"* and offers
**Open Anyway**.

**Only do this for an app you obtained from the project's GitHub releases page
and whose checksum matched.** Approving an app you did not verify removes the
only protection macOS offers against a tampered download.

<details>
<summary>Last resort: removing the quarantine flag from the command line</summary>

Only if the Finder route is unavailable (e.g. on a machine where you manage
Gatekeeper yourself):

```bash
xattr -dr com.apple.quarantine /Applications/NotiBridge.app
```

This removes the quarantine flag from **that one app only**. It does not
disable Gatekeeper, and it does not affect any other application. Never run
`spctl --master-disable` or turn off Gatekeeper system-wide to work around
this — that removes the protection for every application on the Mac.

</details>

---

## Uninstalling

Quit NotiBridge from the menu bar, then:

```bash
rm -rf /Applications/NotiBridge.app
rm -rf ~/Library/Application\ Support/NotiBridge      # settings, queue, secrets file
security delete-generic-password -s NotiBridge -a bot_token   # keychain token
```

Apple's Notification Center database is never touched.

## Building from source instead

```bash
git clone https://github.com/spdeepak/Notibridge.git
cd Notibridge
make check          # gofmt, vet, tests
make bundle         # → dist/NotiBridge.app (ad-hoc signed)
open dist/NotiBridge.app
```

Locally built copies are not quarantined, so they open without any Gatekeeper
warning. See [docs/RELEASING.md](RELEASING.md) for how the released archives
are produced.
