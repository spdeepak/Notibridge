# NotiBridge troubleshooting

## Quick health checks

```bash
notibridge diagnose      # Notification Center database access report
notibridge version       # confirm the binary you are running
```

Logs go to stderr (menu bar app: watch them with
`./bin/notibridge run -debug` from a terminal). Logs never contain
notification bodies or your bot token.

---

## "Notification database inaccessible"

`diagnose` prints this when reading `~/Library/Group Containers/group.com.apple.usernoted/db2/db`
fails. On macOS 15 (Sequoia) and later this is almost always **TCC**:

1. **System Settings → Privacy & Security → Full Disk Access** → enable the
   app you launch from: **NotiBridge** (the `.app`), your terminal app when
   running from source, or your **IDE** (GoLand, VS Code, …) when you use its
   built-in terminal.
2. **Quit the app completely** and relaunch — TCC grants only apply to new
   process launches. For an IDE this means quitting the IDE itself, not just
   closing the project window.
3. Re-run `notibridge diagnose`.

TCC attributes the process to the application that launched it — your
terminal or IDE — **not** to the `notibridge` binary. Adding `bin/notibridge`
(or a terminal other than the one you are using) to the list therefore has no
effect: grant Full Disk Access to the launcher you actually use.

Other possibilities:

| Symptom | Cause | Fix |
|---|---|---|
| `database not found` | No notification has been recorded yet, or an exotic macOS layout | Trigger a notification, wait, re-run `diagnose` |
| `schema not understood` | Apple changed the db2 schema in a newer macOS release | Please report the macOS version; NotiBridge lists the tables it found to make updating the discovery code easy |
| `unable to open database file (14)` | TCC denial (even when `stat` works) or unreadable WAL sidecars | Grant Full Disk Access to the app that launched the command, then fully quit and relaunch it |
| `Accessible: false` from an IDE's terminal but fine in Terminal.app/iTerm | The IDE — not the terminal — is the responsible app | Grant Full Disk Access to the **IDE** and fully quit it |
| Works in Terminal but not the app | FDA granted to the terminal only | Grant FDA to `dist/NotiBridge.app` itself |

**NotiBridge never writes to Apple's database.** If you suspect otherwise,
the source is opened with `mode=ro`; you can verify with
`fs_usage` or by checking that Notification Center behaves normally.

---

## Telegram errors

| Error (from activity view) | Meaning | Fix |
|---|---|---|
| `401 Unauthorized` | Bot token invalid or revoked | Re-copy the token from @BotFather, save settings again |
| `404 Not Found` | Token malformed | Same as above |
| `400 Bad Request: chat not found` | You have not `/start`ed the bot, wrong chat ID, or the bot was removed from the group | Send `/start` to the bot; re-check the chat ID via `getUpdates`; for groups add the bot to the group |
| `400: chat_id is invalid` | Not a 64-bit integer or `@username` | Enter the full numeric ID (e.g. `-1001234567890`) |
| `403 Forbidden` | The bot is blocked by the user or cannot post in the chat | Unblock the bot / check group permissions |
| `429 Too Many Requests` | Rate limited | Nothing to do — NotiBridge honors `retry_after` automatically. If constant, lower the poll interval or exclude noisy apps |
| `transport error: connection timed out` | Network hiccup; the message *may* have been sent | NotiBridge retries. Duplicates are possible in this ambiguous case (documented limitation) |

Failed deliveries appear in **Settings → Recent activity** with attempt
counts and sanitized errors; the queue retries automatically until the
budget (`max_attempts`) is exhausted.

## The menu bar icon is missing

- The app is running but you are out of menu bar space (notch Macs!). Close
  other menu bar items or use a compact mode.
- Launching `dist/NotiBridge` (the raw binary) from a terminal shows the tray
  only while that terminal session lives.

## Settings page will not open / "missing session token"

The page is served on a random loopback port with a one-session token. Use
**Settings…** from the menu bar (it opens the correct URL). If your browser
blocked it, copy the URL from the log line `settings page ready`.

## Keychain problems

- First run in a fresh environment may prompt to allow access to the
  "NotiBridge" keychain item — allow it.
- Headless/CI environments fall back to `secrets.json` (0600) and the UI
  shows a warning badge. The file is plaintext on disk (protected by file
  permissions and FileVault at rest); prefer the Keychain when available.

## Pausing, forwarding, and "nothing is sent"

Check the menu bar status line:

| Status | Meaning |
|---|---|
| `not configured` | Bot token or chat ID missing → open Settings |
| `forwarding disabled` | Master switch off → toggle in the menu or Settings |
| `paused` | Paused; queued items are delivered on resume |
| `notification database inaccessible` | See the TCC section above |
| `monitoring & forwarding` | Healthy; check Recent activity for per-message outcomes |

## Startup / Gatekeeper notes

Released builds are **not signed with an Apple Developer ID and are not
notarized** — they carry an ad-hoc signature only. `make bundle` produces the
same kind of bundle locally.

- Running `open dist/NotiBridge.app` on the build machine works.
- A copy downloaded from the releases page is quarantined, so Gatekeeper shows
  *"cannot be opened because the developer cannot be verified"*.
- The supported way to open it: **Control-click → Open** in the Finder, then
  confirm **Open**. If that is not offered, **System Settings → Privacy &
  Security → Open Anyway**.
- As a last resort for a single app:
  `xattr -dr com.apple.quarantine /Applications/NotiBridge.app`.

Only approve an app whose checksum matched the published `SHA256SUMS`, and
only from the project's releases page. Never disable Gatekeeper system-wide
(`spctl --master-disable`) for this — that removes the protection for every
application on the Mac.

See [docs/INSTALL.md](INSTALL.md) for the complete installation walkthrough.

## The updater does not find a new version

- NotiBridge only checks **when you ask**: menu bar → *Check for updates…*.
  There is no background polling, so nothing is ever fetched silently.
- The check talks to the GitHub API. Unauthenticated requests are rate limited
  per IP; the error message says so when the limit is hit — wait a few
  minutes.
- Only stable releases are considered. A pre-release (tagged `v0.2.0-rc.1`)
  is ignored until it is published as a normal release.

## The updater refuses to install an update

| Message | Fix |
|---|---|
| `checksum mismatch for …` | Do not install. Re-download and report it — the archive does not match the published checksum |
| `the release signature is not valid for the embedded public key` | Do not install. The release is not signed by the key this build expects; report it |
| `release X publishes no signature and the embedded public key requires one` | The release was published without a signature; report it |
| `manual installation required` | The application folder is not writable. Replace `/Applications/NotiBridge.app` by hand — see [docs/UPDATING.md](UPDATING.md) |

The helper that performs the swap logs to `/tmp/notibridge-update.log`.

## Reset everything

```bash
# stop the app first (menu → Quit)
rm -rf ~/Library/Application\ Support/NotiBridge   # config, queue, secrets file
security delete-generic-password -s NotiBridge -a bot_token   # keychain token
```

The Notification Center database is untouched by any of this.

## Reporting issues

Please include: macOS version (`sw_vers`), how you run NotiBridge
(bundle/terminal), `notibridge diagnose` output, and relevant log lines.
Redact chat IDs if you prefer — the error messages are already token-safe.
