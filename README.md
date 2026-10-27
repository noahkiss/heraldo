<picture>
  <source media="(prefers-color-scheme: dark)" srcset="design/icon/export/logo/readme-banner-dark.png">
  <img alt="Heraldo" src="design/icon/export/logo/readme-banner-light.png" width="640">
</picture>

# Heraldo

A self-hosted notification hub. Services post events to one server. The server
stores every event and delivers it to an iPhone and Mac app you build yourself.

Status: stage 4. The server stores and serves every event. It can push to
Apple devices through APNs, but push is off until `HERALDO_PUSH=on`.

## What it will do

- One event shape, [envelope v1](schema/envelope.v1.schema.json), for messages,
  incidents, digests, progress, check-ins and status values.
- Levels from passive to time-sensitive, threads per source, action buttons,
  images and links.
- Incidents that go from failure to recovery in one thread.
- Live Activities for long-running jobs.
- Dead-man's-switch check-ins.
- A full history that the apps and the `heraldo` CLI sync by cursor.

## Layout

| Path | Contents |
|---|---|
| `schema/` | Envelope v1 JSON Schema and the OpenAPI 3.1 document. The source of truth. |
| `api/` | Go types generated from `schema/` |
| `cmd/heraldo-server` | The server |
| `cmd/heraldo` | The CLI |
| `internal/` | Server packages: store, validation, auth, ingest rules, the tee import and replay; `client`, the API client the CLI uses |
| `apple/` | Xcode project (XcodeGen): the Heraldo app for iOS and macOS, its extensions, HeraldoKit |
| `deploy/` | A generic compose file, an example env file and an example rules file |
| `docs/` | Design notes |

## Build

Go 1.27 or later:

```sh
go vet ./... && go test ./...
go run ./cmd/heraldo-server        # listens on :7842, database heraldo.db
go run ./cmd/heraldo version
go generate ./...                  # regenerate api/types.gen.go after a schema change
```

Apple (Xcode 27, XcodeGen):

```sh
apple/scripts/check.sh             # builds for the iOS Simulator and macOS, runs HeraldoKit tests
apple/scripts/test-server.sh       # HeraldoKit tests against a local heraldo-server
```

Container (the published image is `ghcr.io/noahkiss/heraldo`, linux/amd64):

```sh
cp deploy/heraldo.example.env deploy/heraldo.env    # then add a token, below
docker compose -f deploy/compose.yml up --build
```

## Run

1. Make a token for each sender and each reader, and hash it:

   ```sh
   openssl rand -hex 32 > sender.token
   heraldo-server hash-token < sender.token
   ```

2. List the hashes in a token file, one `<name> <scope> <sha256>` per line:

   ```text
   # name    scope  sha256 of the token (64 hex characters)
   backup    write  <sha256-of-sender.token>
   phone       read   <sha256-of-phone.token>
   phone-write write  <sha256-of-phone-write.token>
   ```

   `write` may post events, `read` may list and fetch them, and `admin` may do
   both. A `read` token also moves the read cursors and registers devices for
   push. The server stores only the hashes.

   Each app device holds a token pair, as the CLI does: a `read` token to sync
   history, mark events read and register for push, and a `write` token for
   the notification actions (Ack) and for sending an event.

   A fourth field, `trusted-ingest`, marks a bridge such as apprise-tee. Its
   `details.tee.hold` becomes the event's `hold`, and its `dedup` ids never
   expire. [docs/api.md](docs/api.md) has the details.

3. Start the server with `HERALDO_TOKENS_FILE` pointing at the file, and post an
   event:

   ```sh
   curl -H "Authorization: Bearer $(cat sender.token)" \
     -d '{"source":"backup","kind":"message","title":"Backup finished"}' \
     http://localhost:7842/v1/events
   ```

[docs/api.md](docs/api.md) lists the endpoints.

## Configuration

The server reads its configuration from the environment.

| Variable | Default | Meaning |
|---|---|---|
| `HERALDO_ADDR` | `:7842` | Listen address, `host:port` |
| `HERALDO_DB` | `heraldo.db` (`/data/heraldo.db` in the image) | SQLite database path. The server runs it in WAL mode, so keep the `-wal` and `-shm` files beside it. |
| `HERALDO_TOKENS_FILE` | none | Token file, one `<name> <scope> <sha256> [trusted-ingest]` per line |
| `HERALDO_TOKENS` | none | The same entries inline, separated by commas. Set this or the file, not both. |
| `HERALDO_RULES` | none | Ingest rules file (YAML) that types untyped inputs (no `kind`); without it they are stored as `message`. The server reloads it within 5 s of a change; a file that does not parse keeps the rules in force and logs an error. A broken file at startup stops the server. Format: [docs/api.md](docs/api.md#ingest-rules), example: `deploy/rules.example.yaml`. |
| `HERALDO_PUSH` | `off` | `on` sends APNs pushes to the registered devices. Off, the server registers devices and pushes nothing. |
| `HERALDO_APNS_KEY_FILE` | none | The APNs auth key, the `.p8` file from Apple. Mount it read-only. |
| `HERALDO_APNS_KEY` | none | The same key inline, the `.p8` file's PEM text, for a secret store that passes values as environment variables. A value with no line breaks has each literal `\n` turned into one. Set this or the file, not both. |
| `HERALDO_APNS_KEY_ID` | none | The key's id, 10 characters. |
| `HERALDO_APNS_TEAM_ID` | none | The Apple developer team id. |

With no tokens, the server refuses every call except `/healthz`.

Set a key (`HERALDO_APNS_KEY_FILE` or `HERALDO_APNS_KEY`), its id and the team
id, or none; a partial set stops the server. With `HERALDO_PUSH=on` and no key,
or a key that does not load,
the server logs an error, keeps running and pushes nothing. `/healthz` reports
`push` as `off`, `on` or `no-key`.

## Operations

| Command | Does |
|---|---|
| `heraldo-server` | Runs the server. SIGTERM or SIGINT shuts it down cleanly. |
| `heraldo-server backup <path>` | Copies the database to a new file with SQLite's online backup. The server keeps running. The copy is one file in rollback-journal mode, with no `-wal` or `-shm` beside it. |
| `heraldo-server import-tee <dir>` | Backfills an apprise-tee catalogue (`notifications-YYYY-MM.jsonl`), typed by `HERALDO_RULES` when set. A rerun stores nothing twice. |
| `heraldo-server replay-tee [-rules <file>] <dir>` | Runs a catalogue through a rules file (default `HERALDO_RULES`) in memory and reports how many recoveries paired with their failure, the unpaired ones, the hits per rule and every source and tag no rule matched. It opens no database. |
| `heraldo-server hash-token` | Prints the SHA-256 of a token read from stdin. |
| `heraldo-server healthcheck` | Exits 0 when the local `/healthz` answers. The image's `HEALTHCHECK` runs it. |

Logs are JSON lines on stderr. A request line never includes the query string,
because `/v1/send` may carry a token there.

To restore, stop the server and replace the database file with a backup. Delete
any `-wal` and `-shm` files left beside the old database first.

## CLI

`heraldo` is an inbox over a server, and a drop-in for a notify script.

| Command | Does |
|---|---|
| `heraldo peek [--mark]` | Lists the events after the read cursor. `--mark` moves the cursor past them. With no cursor yet, it lists the newest events. |
| `heraldo show [--json] <id>` | Prints one event with its full body. |
| `heraldo ls` | Lists the newest events (20 by default, oldest first). Filters: `--kind`, `--source`, `--host`, `--key`, `--tag`, `-q` (full text). `--open` lists the incidents that are not resolved. |
| `heraldo held` | Lists events held for triage (`hold.state` is `held`). |
| `heraldo checkins` | Lists the last run of every check. |
| `heraldo tail` | Prints new events as they arrive. It takes the `ls` filters, `--unread` (start at the read cursor), `--mark` (move the cursor; not with filters) and `--exec <command>`. |
| `heraldo ack <key>`, `heraldo resolve <key>` | Posts an `ack` or `resolve` event for an open incident. `-m` adds a note; `--force` posts for a key with no open incident. |
| `heraldo send` | Posts a notification to an Apprise-compatible notify URL. |

Every list command takes `--json` and prints each event's stored JSON on one
line.

Settings come from the environment first, then from an env file:
`$XDG_CONFIG_HOME/heraldo/env` (`~/.config/heraldo/env`), or the file
`HERALDO_ENV_FILE` names. The file holds `KEY=VALUE` lines. Keep it mode 600.

| Variable | Meaning |
|---|---|
| `HERALDO_URL` | The server root, such as `https://heraldo.example.net` |
| `HERALDO_TOKEN` | A `read` token |
| `HERALDO_WRITE_TOKEN` | A `write` token, for `ack` and `resolve` |
| `HERALDO_CURSOR` | The read cursor's name. Default: `inbox`, the cursor every client shares. Set another name for a separate cursor. |
| `HERALDO_SOURCE` | The `source` of `ack` and `resolve` events. Default: `heraldo-cli`. |
| `HERALDO_SEND_URL`, `APPRISE_URL` | `send`'s notify URL; the first one set wins |
| `NOTIFY_EXTRA_JSON` | `send`: extra JSON fields for the payload, such as `"_nonce": "abc"` |

Read cursors live on the server. One cursor is one unread state: every machine
and app that reads `inbox` shares it, as with email. A cursor only moves
forward.

`tail` is poke-then-fetch. The server sends a poke on `/v1/stream` for each
stored event; the CLI then lists every event after its cursor. When the stream
breaks, the CLI reconnects and fetches what it missed. Against a server with no
stream, it polls every second.

### tail --exec

`tail --exec <command>` runs the command once for each event it prints, one at
a time, for at most a minute each. The command must be listed in
`$XDG_CONFIG_HOME/heraldo/exec-allow`: one absolute path per line, `#` for
comments. The CLI refuses the file when group or others can write to it.

No shell runs. The command gets the event's JSON on stdin, and these variables:
`HERALDO_EVENT_ID`, `_SOURCE`, `_KIND`, `_ACTION`, `_KEY`, `_HOST`, `_LEVEL`,
`_TITLE`, `_TAGS` (comma-separated) and `_TS`. It does not get
`HERALDO_TOKEN` or `HERALDO_WRITE_TOKEN`.

### send

`heraldo send` keeps the contract of a notify script that posts to Apprise:

- `-t`/`--title` (default `System Notification`), `-m`/`--message`. Without
  `-m`, the message comes from stdin; with a terminal on stdin, `send` fails.
- `--tag`, `--source`, `--url`, `--image` and `--level` add those fields, for an
  apprise-tee front door.
- With no notify URL set, `send` does nothing and exits 0.
- Success prints `✅ Notification sent successfully.` and exits 0. Success
  means HTTP 200 with a `Sent` line in the answer.
- Anything else prints `❌ Failed to send notification. HTTP Status: <code>` and
  exits 1. A request that got no answer reports `000`.

## iOS app

The app ships no token. In Settings, enter the server URL and the device's
read and write tokens. The app keeps them in iCloud Keychain, so they reach
the person's other devices; the notification extension reads them through the
app group.

- **Inbox**: history synced by cursor on launch and on foreground, search,
  filters by kind and source. A swipe marks events read up to that one; the
  menu marks all read. Both move the shared `inbox` cursor, so the CLI and the
  app share one unread state. The badge is the cursor's `unread`.
- **Incidents**: the open incidents from `/v1/state`, with Ack.
- **Push**: the app registers its APNs token with `POST /v1/devices` on every
  launch, and again when a permission changes. A Debug build reports
  `sandbox`; a TestFlight or App Store build reports `production`.
- **Notifications**: `incident` has Ack and Open; `event` has Open. The
  extension fetches the full event by id and adds the image. With the
  Communication Notifications entitlement (not on the App ID yet), it would
  also show the source's `icon` as the sender avatar.
- **Shortcuts**: Send Event (with the write token) and Get Recent History (JSON).
- `heraldo://event/<id>` opens one event.

## Mac app

The same app for macOS 15 or later, signed with Developer ID and notarized.
A GitHub release carries it as `Heraldo-<version>-macos.zip`; the Homebrew
cask installs it:

```sh
brew install --cask noahkiss/tap/heraldo-app
```

- **Menu bar**: the unread count, the open incidents with Ack, and the failing
  check-ins. A click on one opens it in the history window.
- **History window**: the inbox and incidents of the iOS app. Settings is in
  the app menu (Command-comma).
- **Credentials**: the same iCloud Keychain item as the iPhone, so tokens saved
  on one device reach the other.
- **Push**: the Mac registers its own APNs token with `platform` `macOS` and
  environment `production`, and reports the same permission health. The Dock
  icon shows the `unread` badge.
- The app syncs every minute, on wake and on a push. Closing the window keeps
  the menu bar item.

A Mac build without a Developer ID profile signs ad hoc. It has no push, and it
keeps the credentials in this Mac's login keychain. See
`apple/Config/Local.example.xcconfig`, step 3.

## License

MIT. See [LICENSE](LICENSE).
