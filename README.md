# NotiBridge

**macOS Notification Center → Telegram, from your menu bar.**

NotiBridge is a lightweight, local-only macOS utility that reads notifications
from the Notification Center database, forwards them to a Telegram chat, and
never sends anything anywhere else. No cloud backend, no telemetry, no account.

```
┌─────────────┐   read-only    ┌──────────┐  dedup + queue  ┌────────────┐
│ Notification │ ─────────────▶ │ NotiBridge│ ──────────────▶ │ Telegram   │
│ Center (db2) │                │ (menu bar)│   HTTPS only    │ Bot API    │
└─────────────┘                └──────────┘                  └────────────┘
```

---

## ⚠️ Read this first: what NotiBridge can and cannot do

**There is no official macOS API to read all notifications.** NotiBridge reads
Apple's *undocumented* Notification Center SQLite database
(`~/Library/Group Containers/group.com.apple.usernoted/db2/db` on macOS 15+):

- **Not guaranteed.** The schema, location and access rules are undocumented
  and change between macOS releases. NotiBridge detects the schema at runtime
  and reports mismatches instead of failing silently.
- **Permission-gated.** On macOS 15 (Sequoia) and later the database sits in a
  TCC-protected group container. Reading it generally requires **Full Disk
  Access** for NotiBridge (see [macOS permissions](#macos-permissions)).
  Even with Full Disk Access, access is not guaranteed on every release.
- **Not a keylogger.** You only get what apps put in notification payloads —
  and only for notifications the system actually records.
- **Not every transient event appears.** Some system alerts, banners shown by
  sandboxed apps, or silently-swallowed notifications may never reach the
  database.
- **Runs only while awake.** The app cannot run while the Mac sleeps. It
  detects wake-up (poll-gap detection), logs it, and picks up notifications
  delivered while it was asleep — they are still in the database. Screen
  *lock* is not a problem: polling continues while locked.

Verify access any time with:

```bash
notibridge diagnose
```

**Delivery semantics:** Telegram's `sendMessage` has no idempotency key.
NotiBridge retries transient failures (timeouts, 5xx, 429), but if a request
times out *after* Telegram accepted it, a retry can create a duplicate
message. This is at-least-once delivery, not exactly-once.

---

## Features

- 🪧 Menu bar app (no Dock icon) with live status: monitoring, queue counts,
  database access
- 🔁 Polls the Notification Center database (default every 2 s, configurable)
- 🔐 Reads Apple's database **strictly read-only**; never modifies it
- 🔁 Stable deduplication by notification UUID, with a documented
  content-fingerprint fallback for records without UUIDs — no duplicates
  across polls *or* restarts
- 📦 Durable delivery queue in NotiBridge's own SQLite database, with bounded
  exponential backoff + jitter, and a retry budget for persistent failures
- ✅ Test connection (`getMe`) and **Send test message** actions
- ⏸️ Pause/resume instantly; master forwarding switch (persisted)
- 🔒 Forwards **only while the screen is locked** (on by default): notifications
  that arrive while you are at the Mac are ignored
- 🚫 App exclusions (`com.apple.*` wildcards supported)
- 🧹 Configurable local history retention (default 14 days) + one-click
  **Clear local history**
- 🔑 Bot token in the **macOS Keychain** (with a warned 0600-file fallback
  for headless environments)
- ⬆️ In-app update check against the GitHub releases page: download, verify
  (SHA-256 + Ed25519 signature), install — only ever when you ask for it
- 🛟 Structured logs with automatic secret redaction; notification bodies are
  never logged

## Requirements

- Apple Silicon or Intel Mac, macOS 12 or later (developed and tested on
  macOS 27 / Tahoe; the db2 layout is used on macOS 10.13–26)
- Go 1.22+ to build from source
- A Telegram bot token ([@BotFather](https://t.me/BotFather)) and a chat ID

## Install

Download the archive for your Mac from the
[releases page](https://github.com/spdeepak/Notibridge/releases), verify it
against the published `SHA256SUMS`, and move `NotiBridge.app` into
`/Applications`.

```bash
shasum -a 256 NotiBridge_0.1.0_arm64.zip   # compare with SHA256SUMS
unzip NotiBridge_0.1.0_arm64.zip
mv NotiBridge.app /Applications/
```

> The released app is **not signed with an Apple Developer ID and is not
> notarized**, so macOS shows an "unidentified developer" warning on the first
> launch. [docs/INSTALL.md](docs/INSTALL.md) explains how to open it with
> Apple's per-app approval flow — without weakening Gatekeeper.

Full instructions, including the Full Disk Access grant and the Telegram
setup, are in [docs/INSTALL.md](docs/INSTALL.md).

## Updating

NotiBridge can check the releases page for a newer version, download it,
verify it and install it — **only when you ask for it** (menu bar → *Check for
updates…*). There is no background polling.

The download is verified against the release's SHA-256 checksum, and, when the
build embeds the project's public key, the checksum file must also carry a
valid Ed25519 signature. A downloaded update is staged first and installed in a
separate step; your settings and bot token are never touched.

See [docs/UPDATING.md](docs/UPDATING.md).

## Build and run

```bash
# development build + run with verbose logs
make dev

# or manually
go build -o bin/notibridge ./cmd/notifyrelay
./bin/notibridge run -debug

# checks: formatting, vet, all tests
make check

# release binary for Apple Silicon
make release          # → dist/NotiBridge

# assemble dist/NotiBridge.app (ad-hoc signed, LSUIElement) and its archive
make bundle           # → dist/NotiBridge_0.1.0_arm64.zip
open dist/NotiBridge.app

# other architectures
make package-amd64
make package-universal
```

Quit from the **menu bar icon → Quit NotiBridge** (shuts down workers and
database connections cleanly). `Ctrl-C` also works in development runs.

### First-run: getting your Telegram chat ready

1. In Telegram, open **@BotFather** → `/newbot` → follow the prompts → copy
   the **bot token** (looks like `123456789:AA...`).
2. Open a chat with your new bot and send `/start`. *(Required — bots cannot
   message you before you message them.)*
3. Find your **chat ID** the official way: send any message to your bot, then
   open
   `https://api.telegram.org/bot<YOUR-TOKEN>/getUpdates`
   in a browser and read `"chat":{"id":...}`. For groups/supergroups the id
   is a large negative number like `-1001234567890`. NotiBridge handles full
   64-bit IDs, and also accepts `@channelusernames`.
4. Click the **NotiBridge menu bar icon → Settings…**
5. Paste the bot token and chat ID, tick *Forwarding enabled*, **Save
   settings**, then **Test connection** and **Send test message**.
6. Run **Run database diagnostics** on the same page (or
   `notibridge diagnose` in a terminal) to verify Notification Center access.

The saved token is **masked** — the UI never shows it again. Leave the token
field blank when saving to keep the stored one.

## macOS permissions

On **macOS 15 (Sequoia) and later**, the Notification Center database lives in
a TCC-protected group container. In practice this means:

1. Open **System Settings → Privacy & Security → Full Disk Access**.
2. Enable it for **NotiBridge**, or for the app you launch from while
   developing from source — your **terminal** (Terminal, iTerm) or your
   **IDE** (GoLand, VS Code, …) when using its built-in terminal. TCC
   attributes the process to that launcher, not to the `notibridge` binary,
   so an entry for the binary has no effect.
3. Fully quit and restart the app (TCC grants apply to fresh process launches;
   for an IDE, quit the IDE itself).

Notes:

- Full Disk Access is the only permission NotiBridge needs. It is *necessary*
  on Sequoia+ but Apple documents no guarantee that it is *sufficient* —
  the database is private API. NotiBridge never attempts to bypass TCC; when
  access fails it says so and tells you what to check.
- Notification *contents* are not separately permission-gated: if you can read
  the database, you can read what apps stored in it. That's exactly why
  NotiBridge treats the data as sensitive (see Privacy below).

## Forwarding only while the screen is locked

By default NotiBridge **forwards a notification only while your screen is
locked**. While you are at the Mac, notifications are **ignored**: they are not
sent to Telegram — and they are **not postponed** either. The polling watermark
still advances, so a notification that arrived while you were working is never
delivered later.

Turn the restriction off in the menu bar (**Only forward while the screen is
locked**) or on the settings page (**Only forward notifications while the
screen is locked**). The menu bar and the settings page also show the current
lock state, so "nothing arrives" is never a mystery.

The state comes from the macOS session dictionary (`CGSSessionScreenIsLocked`)
and needs no extra permission. The check happens when a poll picks the
notification up: a notification that arrives while the screen is locked but is
read a moment after you unlocked the Mac is treated as ignored.

## How deduplication works

| Record has UUID | Dedup key |
|---|---|
| yes (normal case) | `u:<record-uuid>` — stable across polls and restarts |
| no (rare) | `f:<sha256(app, request-id, timestamp, title, subtitle, body)>` |

`rec_id` is **never** used as a durable key — it can change when Notification
Center reorganizes its database.

**Fingerprint limitation:** two legitimate notifications with identical app,
request id and text arriving in the *same second* hash identically and only
one is forwarded. This is inherent to content fingerprinting; UUIDs cover
virtually all real notifications.

## Retries and delivery state

- Every discovered notification is persisted (`pending`) before sending.
- Transient errors (timeouts, network, HTTP 5xx, 429) → exponential backoff
  with ±50 % jitter (5 s → 10 min cap), honoring Telegram's `retry_after`.
- After `max_attempts` (default 8) or a permanent API error (e.g. *chat not
  found*), the delivery is marked `failed` with a sanitized error.
- Pending deliveries survive restarts and are resumed automatically.
- Pause/forwarding-off stops *sending*; queued items are kept and delivered on
  resume (see the pause confirmation message).
- History (sent + failed) is purged after `history_retention_days`
  (default 14). **Clear local history** from the menu or the web page deletes
  everything immediately.

## Privacy and security

- **Forwarding to Telegram sends notification contents to Telegram's
  servers.** Think about 2FA codes, private messages and work data before
  enabling it. Exclude noisy/sensitive apps with the exclusions list.
- The bot token is stored in the **macOS Keychain**. If the Keychain is
  unavailable, NotiBridge falls back to `secrets.json` (mode 0600, inside
  `~/Library/Application Support/NotiBridge/`) and the UI shows a warning.
- The config file (`config.json`, mode 0600) never contains the token.
- Logs redact the token automatically and never include notification bodies.
- Telegram traffic goes to `https://api.telegram.org` only. The only other
  request NotiBridge ever makes is to the GitHub API — and only when you
  explicitly ask it to check for updates. No background traffic, no telemetry,
  no analytics.
- Apple's database is opened read-only (`mode=ro`) and never written,
  vacuumed, or checkpointed by NotiBridge.
- All local data lives in `~/Library/Application Support/NotiBridge/`
  (`config.json`, `state.db`, maybe `secrets.json`). Delete the folder to
  remove everything.

## Project structure

```
cmd/notifyrelay/                    entry point (run | diagnose | version)
internal/
  app/                              application service: poller, delivery
                                    worker, retention, lifecycle
  config/                           settings + validation + secret stores
  dedup/                            dedup keys (UUID + fingerprint)
  filters/                          app exclusion rules
  logging/                          slog setup + secret redaction
  menubar/                          menu bar UI (fyne/systray) + icon
  notifications/
    model.go                        shared notification model
    parser/                         binary plist decoding
    source/                         source interface + macOS db2 reader
  queue/                            durable delivery queue + backoff
  storage/                          app-owned SQLite + migrations
  telegram/                         Bot API client + HTML-safe formatting
  update/                           release check, verification, install
  webui/                            loopback settings/activity page
assets/icons/NotiBridge.icns         generated app icon (see `make icons`)
image.png                            icon source of truth
packaging/Info.plist                 app bundle metadata (LSUIElement)
scripts/make_icons/                  icon generator
scripts/package_app.sh               release .app assembly + zip
scripts/sign_release/                Ed25519 signing of release checksums
docs/INSTALL.md                      installing, Gatekeeper, first launch
docs/UPDATING.md                     how the updater works
docs/RELEASING.md                    how a release is cut
docs/TROUBLESHOOTING.md              when things go wrong
```

The core (`app`, `queue`, `notifications`, `telegram`, `storage`) has no
dependency on the menu bar or web UI, so it can be reused (e.g. by a future
Nexflow integration) via `app.Service`.

## Testing

```bash
gofmt -w .
go test ./...
go vet ./...
```

All automated tests are hermetic: SQLite fixture databases replace the real
Notification Center store, an `httptest` server replaces Telegram, and a file
secret store replaces the Keychain. **No test touches the real database,
Keychain, or network.**

Platform-specific checks that ran on this machine (Apple Silicon, macOS 27):
`go build ./...`, `go vet ./...`, full `go test ./...`, `make bundle`
(including `codesign --verify`), an arm64 **and** amd64 package build with
checksum verification, `notibridge diagnose`, and a SIGTERM smoke-run of the
app. **Live reads of the real Notification Center database could not be
verified on the development machine** — TCC denied access until Full Disk
Access is granted manually (exactly the situation `diagnose` reports). See
[docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md).

The GitHub Actions workflows (`.github/workflows/ci.yml` and
`release.yml`) run the same checks on every push and publish the releases.

## Known limitations

- Notification capture is best-effort (see the disclaimer above).
- At-least-once Telegram delivery; rare duplicates on ambiguous timeouts.
- Fingerprint dedup can collapse two byte-identical notifications in the same
  second (records *without* UUIDs only).
- The app bundle is **ad-hoc signed only** — not signed with an Apple
  Developer ID and not notarized. Gatekeeper warns about the first launch of a
  downloaded copy; locally built copies run fine. See
  [docs/INSTALL.md](docs/INSTALL.md).
- The update check is **manual** — NotiBridge never polls for updates in the
  background.
- With the screen lock restriction on (the default), notifications that arrive
  while the screen is unlocked are **dropped, not queued**.
- Single-user, single-chat, no cloud sync (by design for the MVP).

## License

Private/unpublished unless otherwise stated by the author.
