# AGENTS.md — NotiBridge engineering guide

Instructions for AI coding sessions working on this repository. Follow them
unless a task explicitly requires a deviation; deviations must be explained
in the response.

## Project overview

NotiBridge is a standalone **macOS menu bar utility** (Go, Apple Silicon,
macOS 12+) that reads notifications from the local, **undocumented**
Notification Center SQLite database and forwards them to a Telegram chat via
the official Bot API.

- **Users:** a single local user configuring one bot + one chat. No accounts,
  no cloud, no telemetry.
- **Status:** MVP, working end to end. It must remain **independent of
  Nexflow**; the core (`app`, `queue`, `notifications`, `telegram`,
  `storage`) must stay UI-free so it can be reused by Nexflow or other
  frontends later.
- **Honesty rule:** the app must never claim capabilities it does not have.
  Notification capture is best-effort (TCC, schema drift); delivery is
  at-least-once (Telegram has no idempotency key). Both are documented in
  `README.md` and must stay documented in code comments and user-facing
  messages.

## Architecture

Each package has exactly one responsibility. Keep it that way.

| Package | Responsibility |
|---|---|
| `internal/notifications` | Shared `Notification` model only. No I/O. |
| `internal/notifications/source` | `Source` interface (`Poll`, `Diagnose`, `Close`) + `MacOSSource`: path detection, read-only SQLite open, schema discovery, watermark pagination. Never parses payloads. |
| `internal/notifications/parser` | Decodes binary/XML plists into `Notification`. Tolerates missing/malformed fields; never panics. |
| `internal/filters` | App-exclusion rules (`com.example.*` wildcards). Immutable, rebuilt on config change. |
| `internal/dedup` | Computes durable keys: `u:<uuid>` when a record UUID exists, else `f:<sha256 fingerprint>`. Contains the documented fingerprint-collision limitation. |
| `internal/storage` | The app's own SQLite DB (`state.db`): migrations (`schemaVersion` + `migrations` slice), `deliveries` + `meta` tables, purge/clear. Single pooled connection (see Concurrency). |
| `internal/queue` | Durable delivery queue: atomic `Enqueue` (INSERT OR IGNORE = dedup), `ClaimDue`, `MarkSent`/`MarkRetry`/`MarkFailed`, jittered exponential `BackoffDelay`, `Counts`, `Recent`. |
| `internal/telegram` | Bot API client (`getMe`, `sendMessage`), typed `APIError`/`TransportError` with `Retryable()`, token redaction, `Format()` (HTML-escaped message rendering). |
| `internal/config` | `Config` load/save/validate, `SecretString` (unprintable), `SecretStore` interface with Keychain (`KeyringStore`) and 0600-file (`FileStore`) backends. |
| `internal/app` | `Service`: wires everything; poller, delivery worker, retention loops, watermark persistence, status snapshots, pause/resume, lifecycle. No UI imports. |
| `internal/menubar` | fyne/systray menu + runtime-generated icon. Renders `app.Status`, forwards actions. |
| `internal/webui` | Loopback (127.0.0.1, random port, random session token) settings/activity page. |
| `internal/logging` | slog handler that redacts registered secrets from messages *and* attributes. |
| `cmd/notifyrelay` | Entry point: `run` (default), `diagnose`, `version`. Flags: `-debug`. Env: `NOTIBRIDGE_HOME`, `NOTIBRIDGE_LOG_LEVEL`. |

Data flow: `MacOSSource.Poll` → `parser.Parse` → `filters` → `dedup.Key` →
`queue.Enqueue` (persist) → worker claims due rows → `telegram.Format` +
`SendText` → `MarkSent`/`MarkRetry`/`MarkFailed`.

## Repository structure

- Packages live under `internal/` — nothing is importable outside the module,
  by design. Do not move packages to `pkg/`.
- Platform-specific code stays behind interfaces: `source.Source`,
  `config.SecretStore`, and the Telegram sender used by the worker.
  Menu bar and web UI are replaceable frontends over `app.Service`.
- Tests sit next to their packages (`*_test.go`). App-level end-to-end tests
  use injectable fakes from `app.Options` (`Source`, `Secrets`,
  `TelegramBaseURL`, `DBPath`).
- `packaging/Info.plist` + `scripts/package_app.sh` produce
  `dist/NotiBridge.app` (LSUIElement, ad-hoc signed). `Makefile` targets:
  `build dev test vet fmt check diagnose release bundle clean`.

## Go conventions

- Idiomatic Go: small packages, explicit interfaces at real seams (source,
  secret store, sender), no speculative abstractions or plugin systems.
- Errors: never swallowed. Wrap with context (`fmt.Errorf("...: %w", err)`).
  User-facing errors must be actionable (e.g. TCC hints), sanitized, and must
  never contain the bot token or notification bodies.
- Context: every blocking operation takes `ctx`; use per-operation timeouts
  (poll 10 s, send 25 s, shutdown 8 s). Shutdown must complete in seconds.
- Concurrency: shared mutable state behind `sync.Mutex` or atomics. The app
  DB uses **one pooled connection** (`SetMaxOpenConns(1)`) — this was a
  deliberate fix for intermittent in-process `SQLITE_BUSY`; do not raise it
  without a compelling reason and load tests.
- Dependencies: keep the list small and maintained (current: `fyne.io/systray`,
  `zalando/go-keyring`, `howett.net/plist`, `modernc.org/sqlite`). Justify any
  addition. Run `go mod tidy` after import changes.
- Formatting/vet are mandatory: `gofmt -w .`, `go vet ./...`. No `//nolint`
  noise; fix the code.
- No global mutable state, no giant files, no `init()` side effects.
- Logging: structured (`log/slog`) via `logging.New`; register secrets with
  `logging.RegisterSecret`; default level Info; never log bodies/titles.

## macOS-specific constraints

- The Notification Center DB (`~/Library/Group Containers/group.com.apple.usernoted/db2/db`
  on macOS 15+; `$(getconf DARWIN_USER_DIR)com.apple.notificationcenter/db2/db`
  earlier) is **undocumented and TCC-protected**.
- **Read-only, always.** Open with `mode=ro`; never INSERT/UPDATE/DELETE/VACUUM
  Apple's database, WAL sidecars included.
- **Never hardcode a username or assume a fixed schema.** `MacOSSource`
  discovers tables/columns at runtime and fails with a diagnostic listing what
  it found. Extend `discoverSchema` rather than adding fixed queries.
- Access failures must be *reported, not silent*: `Diagnose()` feeds the CLI,
  the menu bar, and the web UI. Preserve the Full Disk Access hints.
- The app cannot run while the Mac sleeps; the poller detects long gaps and
  logs resumption. Screen lock is not sleep — polling continues.
- Menu bar specifics: `systray.Run` blocks on the main goroutine; on macOS,
  `Quit()` does **not** fire `onExit` — `menubar.Run` wraps cleanup in
  `sync.Once` and also calls it after `Run` returns. Keep that guard.
- The web UI exists because macOS has no lightweight native text-input path
  from a tray menu; do not add a second UI framework (Wails etc.) for the MVP.

## Notification processing

- **Dedup key = record UUID when present** (`u:<hex>`), else a documented
  fingerprint `f:<sha256(app, request-id, timestamp, title, subtitle, body)>`.
  **Never use `rec_id` as a durable key** — it can be renumbered.
- Deduplication state is the `deliveries.source_key` unique constraint; it
  survives restarts. Discovery ≠ delivery: a record is marked `sent` only
  after Telegram confirms acceptance.
- Watermark (meta key `source_watermark`) is saved *after* successfully
  enqueuing a poll batch; first polls respect `Options.Lookback` (default
  5 min) so a fresh install does not replay history.
- Parsing failures skip the record with a debug log (rec_id only, no
  payload). Excluded apps are counted, not logged individually.

## Telegram integration

- Official HTTPS Bot API only (`https://api.telegram.org`); no third-party
  clients, no scraping. Base URL is injectable for tests.
- Token lives in the macOS Keychain (`KeyringStore`); the 0600 file fallback
  (`FileStore`) exists for headless environments and must be surfaced as a
  warning in the UI. The token never enters `config.json`, logs, or errors
  (`[redacted]` everywhere).
- Chat IDs are 64-bit (string-encoded) or `@username`; never assume 32-bit.
- Messages use `parse_mode=HTML`; every user-controlled value goes through
  the escaper in `telegram.Format`. No Markdown assumptions.
- Handle 429 via `parameters.retry_after` (also as a backoff floor), 5xx and
  timeouts as retryable, other 4xx as permanent. Retry budget
  (`queue.Options.MaxAttempts`, default 8) then `failed`.
- Delivery is **at-least-once**: a timeout after Telegram accepted a message
  can duplicate on retry. Do not add "exactly-once" claims or complex
  dedup-on-retry without solving the API's missing idempotency key.

## Security and privacy

- Never commit credentials, tokens, `secrets.json`, or real fixtures. The
  `.gitignore` already excludes `secrets.json`, `*.db*`, `bin/`, `dist/`.
- Notification content is sensitive: it is stored only in `state.db` for
  retry purposes, purged after `history_retention_days` (default 14), clearable
  via UI, and **never logged**.
- Forwarding transmits content to Telegram — this must remain documented in
  the README and any first-run guidance.
- No telemetry, no analytics, no network calls beyond the Telegram API.
- Configuration and secrets files are 0600 in a 0700 directory; keep it that
  way.

## Testing

- Hermetic tests only. **Never** use the real Notification Center DB, the real
  Keychain, or the real network:
  - Source: fixture SQLite DBs built in-test with the real db2 layout
    (`source/macos_test.go` → `createFixtureDB`).
  - Telegram: `httptest` servers (`telegram/client_test.go`,
    `app/service_test.go` → `telegramCapture`).
  - Secrets: `config.NewFileStore(t.TempDir())` passed via
    `app.Options.Secrets`. **Never call `ApplyConfig` with a bot token in
    tests without overriding `Secrets` — it would write to the developer's
    real Keychain.**
  - Isolation: set `NOTIBRIDGE_HOME` to a temp dir in tests that touch
    config paths.
- Keep coverage of the required behaviors: plist parsing (incl. malformed),
  schema discovery and read-only guarantees, UUID/fingerprint dedup, dedup
  across polls and restarts, queue persistence and status transitions,
  backoff bounds, Telegram success/4xx/429/5xx/transport errors, token
  redaction, HTML escaping, config validation, secret redaction, graceful
  shutdown.
- New behavior needs a test in the same change. Flaky sleeps should be
  replaced with condition polling (`waitFor` helper in `app` tests).

## Build and verification

Run before declaring any change done, and report only what actually ran:

```bash
gofmt -w .          # then confirm: gofmt -l . is empty
go vet ./...
go test ./...       # prefer go test -count=1 ./... when behavior changed
make release        # darwin/arm64 build
make bundle         # dist/NotiBridge.app + ad-hoc codesign verify
./bin/notibridge diagnose   # live DB access check (needs Full Disk Access)
```

Never claim a check passed unless it was executed successfully in this
session. State explicitly which platform-specific checks could not run.

## UI and lifecycle

- Menu actions must work, not decorate: pause/resume, forwarding toggle,
  send test message, settings, activity, diagnostics, clear history, quit.
  If an action cannot work yet, remove it or label the state honestly.
- `app.Service.Start`/`Shutdown(ctx)` own all workers; `menubar.Run`'s
  cleanup callback must keep running exactly once. SIGINT/SIGTERM quit
  gracefully.
- Validate configuration on every save (`Config.Validate`) and show the
  error verbatim in the UI. The stored bot token is never echoed back into
  any form or status output (`Status.ChatIDMasked` for the chat ID).

## Scope control

Out of scope for the MVP — do not add without an explicit user request:
Nexflow integration, cloud sync/accounts, a hosted backend, webhooks, AI
summarization, rule engines beyond app exclusions, analytics dashboards,
auto-update, plugin systems, Windows/Linux support, multiple UI frameworks,
Homebrew publishing, code signing/notarization infrastructure.

## AI coding workflow

1. Read the relevant package (and its test) before editing. Match existing
   naming, error style, and comment tone.
2. Keep the change focused; no drive-by refactors, no new deps without
   justification in the reply.
3. Reuse existing seams: `source.Source`, `config.SecretStore`,
   `app.Options` injection. Prefer fakes over build tags or env hacks.
4. Update tests and user-facing docs (`README.md`, `docs/TROUBLESHOOTING.md`)
   in the same change when behavior or commands shift.
5. Run the verification commands above; fix what they surface (the repo's
   history includes real fixes from exactly this loop: NULL-safe scans,
   systray `onExit`, `SQLITE_BUSY`).
6. If you must deviate from this guide or the README's documented limits
   (dedup key scheme, delivery semantics, read-only guarantee, secret
   storage), say so explicitly and justify it in the final response.

## Definition of done

A change claiming completion must have, and demonstrate:

- [ ] `gofmt -l .` empty, `go vet ./...` clean, `go test ./...` passing — run
      in this session.
- [ ] A new notification is forwarded to Telegram end to end (or the test
      equivalent through the fake sender).
- [ ] Re-polling and restarts never re-forward the same record (UUID dedup +
      persisted state).
- [ ] Delivery state transitions (`pending`→`sent`/`failed`), retries with
      bounded backoff, and restart recovery still hold.
- [ ] Credentials remain in the secret store; no secret or notification body
      in logs, errors, config files, or the UI.
- [ ] Apple's database is still opened strictly read-only; access failures
      still produce actionable diagnostics.
- [ ] README/troubleshooting updated for any user-visible change; known
      limitations still stated honestly.
