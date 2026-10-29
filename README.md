<picture>
  <source media="(prefers-color-scheme: dark)" srcset="design/icon/export/logo/readme-banner-dark.png">
  <img alt="Heraldo" src="design/icon/export/logo/readme-banner-light.png" width="640">
</picture>

# Heraldo

A self-hosted notification hub. Services post events to one server. The server
stores every event and delivers it to an iPhone and Mac app you build yourself.

Status: in use. The server stores and serves every event, and pushes it to
Apple devices through APNs with `HERALDO_PUSH=on`. Push is off by default.

## What it is for

heraldo is one front door for everything in your life you want to hear about:
servers, backups, printers, home automation, coding agents, anything that can
post a request. Everything comes in. Very little interrupts you.

Three principles follow, and every feature is judged against them:

- **Store everything, show what matters.** Every event is kept and searchable.
  The main view shows only what you care about now. Routine traffic, such as
  check-in pings and progress updates, has its own home and stays out of the
  main stream.
- **Many ways to filter.** Project, source, kind, host, level, hold state and
  free text all narrow the view. Each kind of event has a place where it
  belongs: incidents, check-ins, Live Activities and the inbox.
- **Interrupt only when it needs you.** Pushes are graded from passive
  (inbox only) to time-sensitive. Holds, mutes, inhibit rules and retries keep
  noise off the phone. A long-running job shows as a Live Activity, not a
  stream of notifications.

## What it does

- One event shape, [envelope v1](schema/envelope.v1.schema.json), for messages,
  incidents, digests, progress, check-ins and status values.
- Levels from passive to time-sensitive, threads per source, action buttons,
  images and links.
- Incidents that go from failure to recovery in one thread.
- Live Activities for long-running jobs.
- Dead-man's-switch check-ins.
- An Apprise-compatible front door: an Apprise sender changes only its URL.
  An alert hold lets a triage job read alerts before they page.
- A full history that the apps and the `heraldo` CLI sync by cursor. Routine
  events (passing check-ins, progress, status) stay out of the main list;
  each check keeps its own history.
- Projects: events filed by project, with a project filter and per-project
  mutes.
- Saved views: named filters every client shares, with optional bundling
  that folds a burst of pushes into one notification.
- A daily digest at a set time, and a flag on incidents open for days with
  no update. Both are off by default.
- Attachments: screenshots, logs and PDFs stored with an event. The apps show
  images and text inline; agents read them over MCP.

## Layout

| Path | Contents |
|---|---|
| `schema/` | Envelope v1 JSON Schema and the OpenAPI 3.1 document. The source of truth. |
| `api/` | Go types generated from `schema/` |
| `cmd/heraldo-server` | The server |
| `cmd/heraldo` | The CLI |
| `internal/` | Server packages: store, settings, validation, auth, ingest rules, the tee import and replay; `client`, the API client the CLI uses; `mcptools`, the MCP tools |
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
   both. Only `admin` changes server settings and icons. A `read` token also
   moves the read cursors and registers devices for push. The server stores
   only the hashes.

   Each app device holds a token pair, as the CLI does: a `read` token to sync
   history, mark events read and register for push, and a `write` token for
   the notification actions (Ack) and for sending an event.

   A fourth field, `trusted-ingest`, marks a bridge such as apprise-tee. Its
   `details.tee.hold` becomes the event's `hold`, and its `dedup` ids never
   expire. `notify-key` instead lets a `write` token be the key in an Apprise
   URL, `/notify/<token>`, for a sender that cannot set a header.
   [docs/api.md](docs/api.md) has the details.

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
| `HERALDO_TOKENS_FILE` | none | Token file, one `<name> <scope> <sha256> [capability]` per line. The capabilities are `trusted-ingest`, `notify-key` and `standby` ([docs/api.md](docs/api.md)). |
| `HERALDO_TOKENS` | none | The same entries inline, separated by commas. Set this or the file, not both. |
| `HERALDO_RULES` | none | Ingest rules file (YAML) that types untyped inputs (no `kind`), and can set the level or a hold on ping events; without it untyped inputs are stored as `message`. The server reloads it within 5 s of a change; a file that does not parse keeps the rules in force and logs an error. A broken file at startup stops the server. Format: [docs/api.md](docs/api.md#ingest-rules), example: `deploy/rules.example.yaml`. |
| `HERALDO_PUSH` | `off` | `on` sends APNs pushes to the registered devices. Off, the server registers devices and pushes nothing. |
| `HERALDO_APNS_KEY_FILE` | none | The APNs auth key, the `.p8` file from Apple. Mount it read-only. |
| `HERALDO_APNS_KEY` | none | The same key inline, the `.p8` file's PEM text, for a secret store that passes values as environment variables. A value with no line breaks has each literal `\n` turned into one. Set this or the file, not both. |
| `HERALDO_APNS_KEY_ID` | none | The key's id, 10 characters. |
| `HERALDO_APNS_TEAM_ID` | none | The Apple developer team id. |
| `HERALDO_NOTIFY_TRUSTED_NETS` | none | CIDRs, separated by commas, whose clients may use the Apprise compatibility and hold paths with no credential. Read the risk in [docs/api.md](docs/api.md#sender-auth) first. |
| `HERALDO_HOLD` | `off` | The alert hold: `off`, `shadow` or `on` ([docs/api.md](docs/api.md#hold)). |
| `HERALDO_HOLD_BYPASS_FILE` | none | The hold's bypass list, a JSON file, reloaded when it changes. Example: `deploy/hold-bypass.example.json`. |
| `HERALDO_HOLD_DEADLINE` | `30m` | How long an alert waits for triage before it is forwarded as it is. |
| `HERALDO_HOLD_DEADMAN` | `20m` | How long the hold waits for a triage poll before it stops holding. |
| `HERALDO_MCP_HTTP` | `off` | `on` serves the MCP tools at `POST /v1/mcp`, for agents that reach the server over HTTP. A read token gets the read tools; a write token gets the write tools too. See [docs/mcp.md](docs/mcp.md#over-http). |
| `HERALDO_ROLE` | `primary` | `standby` runs a standby that copies the primary's devices and pushes when the primary is down. It also needs `HERALDO_PRIMARY_URL` and `HERALDO_PRIMARY_TOKEN`. See [docs/api.md](docs/api.md#standby). |

| `HERALDO_SECRET_KEY_FILE` | none | A 32-byte key (`openssl rand -hex 32 > secret.key`) that encrypts secrets written through the settings API. Without it, the server refuses such writes; a secret can still come from its env var. Mount it read-only. |

With no tokens, the server refuses every call except `/healthz`.

### Server settings

Quiet hours, the badge scope and features that later stages switch on (the LLM, the digest, the
stale sweep, flap grouping) are server settings. Every feature is off by default; the badge counts what needs attention.
`GET /v1/settings` and `heraldo settings ls` list every key with its value,
default and source.

- One precedence order, highest first: a launch flag, an env var, a value
  stored through the API, the built-in default.
- Each key has an env alias and a flag alias: `llm.base_url` is
  `HERALDO_LLM_BASE_URL` and `heraldo-server --llm-base-url`.
- A key set by a flag or env var is locked. A write through the API answers
  409 `locked_by_env` and names the variable.
- An `admin` token writes settings: `PUT` and `DELETE /v1/settings/{key}`,
  `heraldo settings set|unset`, or the MCP tool `settings_write`. A new value
  takes effect without a restart.
- A secret, such as `llm.token`, has no flag. Set it with
  `HERALDO_LLM_TOKEN_FILE` (or `HERALDO_LLM_TOKEN`), as with the APNs key, or
  with `heraldo settings set llm.token - < token.txt`, which the server stores
  encrypted with `HERALDO_SECRET_KEY_FILE`. Reads report only whether it is
  set.

[docs/api.md](docs/api.md#settings) lists the keys.

Set a key (`HERALDO_APNS_KEY_FILE` or `HERALDO_APNS_KEY`), its id and the team
id, or none; a partial set stops the server. With `HERALDO_PUSH=on` and no key,
or a key that does not load,
the server logs an error, keeps running and pushes nothing. `/healthz` reports
`push` as `off`, `on` or `no-key`.

## Operations

| Command | Does |
|---|---|
| `heraldo-server [--<setting> <value> ...]` | Runs the server. SIGTERM or SIGINT shuts it down cleanly: it pushes what is still queued first. Each flag sets and locks one server setting, such as `--llm-mode shadow`; `heraldo-server serve -h` lists them. |
| `heraldo-server backup <path>` | Copies the database to a new file with SQLite's online backup. The server keeps running. The copy is one file in rollback-journal mode, with no `-wal` or `-shm` beside it. |
| `heraldo-server import-tee [-since YYYY-MM] <dir>` | Backfills an apprise-tee catalogue (`notifications-YYYY-MM.jsonl`), typed by `HERALDO_RULES` when set. `-since` reads that month and later only. A rerun stores nothing twice, and an imported event never pushes. |
| `heraldo-server replay-tee [-rules <file>] <dir>` | Runs a catalogue through a rules file (default `HERALDO_RULES`) in memory and reports how many recoveries paired with their failure, the unpaired ones, the hits per rule, every source and tag no rule matched, and the events per project. It opens no database. |
| `heraldo-server backfill-projects [-dry-run] [-rules <file>]` | Gives the stored events with no project one, from the rules file's projects map (default `HERALDO_RULES`) and from earlier events with the same key. `-dry-run` prints the counts and changes nothing. See [docs/api.md](docs/api.md#projects). |
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
| `heraldo peek [--mark]` | Lists the events after the read cursor. `--mark` moves the cursor past them. With no cursor yet, it lists the newest events. `--project` keeps the unread events in those projects; not with `--mark`. |
| `heraldo show [--json \| --md] <id>` | Prints one event with its full body; `why`, one line on why it did or did not push; and an `attached:` line per attachment (type and size). `--md` prints the event as markdown to share, the same text as the apps' Share button, ending with its `heraldo://event/<id>` link. |
| `heraldo why [--json] <id>` | Prints why an event did or did not push, one line per push stage: hold, expiry, push table, mutes, inhibit rules, delivery per device. See [docs/api.md](docs/api.md), "Why it pushed". |
| `heraldo attachment get [-o file] <id>` | Writes an attachment's bytes to a new file, or to stdout. It never overwrites a file. |
| `heraldo ls` | Lists the newest events (20 by default, oldest first). Filters: `--kind`, `--source`, `--host`, `--key`, `--tag`, `--project` (a comma-separated list; `-` for none), `-q` (full text). `--open` lists the incidents that are not resolved, with `(flapping)` on a key that flaps; `--open --stale` only those with no update for the server's `stale.after` (72h). Routine events (passing check-ins, progress, status) stay out unless `--routine` is given, or `--kind` or `--key` names them. `--view <name>` applies a saved view; other filters replace its fields, and the view's own routine filter applies. |
| `heraldo held` | Lists events held for triage (`hold.state` is `held`). |
| `heraldo hold` | Shows the alert hold's queue: mode, dead-man, and each entry. `--pending` lists open entries only. The call is a look, not a triage poll; `--poll` makes it a poll, for a triage job. |
| `heraldo hold release --verdict V --title t --body b --tag t <id>...` | Closes held entries with one diagnosis. In mode `on` it stores one message. Needs `HERALDO_WRITE_TOKEN`. |
| `heraldo hold suppress [--reason r] <id>...` | Closes held entries and sends nothing. Needs `HERALDO_WRITE_TOKEN`. |
| `heraldo state` | Prints the badge, the incidents not resolved, the progress not ended, the check-ins and the statuses. `--project` keeps those projects. |
| `heraldo digest [--send]` | Prints the daily digest as it would go out now: incidents, stale incidents, check-ins, finished jobs, held entries, counts per project. `--send` sends it now, besides the scheduled one; it needs `HERALDO_WRITE_TOKEN`. See [docs/api.md](docs/api.md#digest). |
| `heraldo projects` | Lists the projects the stored events name, with counts, and the events with none (`-`). |
| `heraldo activities [--all]` | Lists the running Live Activities; `--all` adds ended ones. `activities show <key>` prints one with its devices and timeline. |
| `heraldo devices` | Lists the registered devices: name, platform, token suffix, active or retired, and the Focus filter in force. `devices retire <id>` stops pushes to one. |
| `heraldo mute` | Mutes a source, key or project for a while: `--source`, `--key`, `--project`, and `--for` (default `1h`) or `--until`. Needs `HERALDO_WRITE_TOKEN`. |
| `heraldo mutes`, `mutes end <id>` | Lists the mutes in force, or ends one now. Snoozes show `(snooze)`. Needs `HERALDO_WRITE_TOKEN`. |
| `heraldo snooze <key or event id>` | Mutes one incident key, `--for` (default `1h`) or `--until`, then pushes its last event again if it is still open. An event id names its event's key. Needs `HERALDO_WRITE_TOKEN`. |
| `heraldo views [ls]` | Lists the saved views: name, bundle and filter. |
| `heraldo views save [flags] <name>` | Saves a view, or replaces the one with that name. It takes the `ls` filters except `--key`, plus `--level`, `--routine`, `--bundle 15m`, `--max-visible`, `--icon` and `--position`. Needs `HERALDO_WRITE_TOKEN`. |
| `heraldo views rm <name>` | Removes a saved view. Needs `HERALDO_WRITE_TOKEN`. |
| `heraldo checkins` | Lists every check: state, next due time, last run length, first line of the last log. `--project` keeps the checks in those projects. `checkins show <slug>` prints one check with its full last log. |
| `heraldo checkins create <slug>` | Creates a check, or replaces a known check's schedule: `--period` or `--cron` with `--tz`, `--grace`, `--max-runtime`, `--reminder`, `--host`. Needs `HERALDO_WRITE_TOKEN`. |
| `heraldo checkins pause <slug>`, `resume <slug>` | Pauses or resumes a check. Needs `HERALDO_WRITE_TOKEN`. |
| `heraldo checkins key [--remove] <slug>` | Prints a new ping URL, `<HERALDO_URL>/p/<key>`, for the check; the old key stops working. `--remove` removes the key. Needs `HERALDO_WRITE_TOKEN`. |
| `heraldo tail` | Prints new events as they arrive. It takes the `ls` filters, `--unread` (start at the read cursor), `--mark` (move the cursor; not with filters) and `--exec <command>`. |
| `heraldo ack <key>`, `heraldo resolve <key>` | Posts an `ack` or `resolve` event for an open incident. `-m` adds a note; `--force` posts for a key with no open incident. |
| `heraldo send` | Posts a notification to an Apprise-compatible notify URL, with an optional heraldo fallback. `--attach <file>` posts to the heraldo API instead. |
| `heraldo mcp [--read-only]` | Serves the MCP tools for coding agents on stdio. The server can serve them over HTTP too (`HERALDO_MCP_HTTP`). See [docs/mcp.md](docs/mcp.md). |
| `heraldo settings [ls]`, `get <key>` | Lists the server settings, or prints one value. The source and any lock go to stderr. |
| `heraldo settings set <key> <value>`, `unset <key>` | Stores a value, or puts the key back to its default. A secret key takes `-` and reads stdin, never argv. Needs `HERALDO_ADMIN_TOKEN`. |

Every list command takes `--json` and prints each event's stored JSON on one
line.

Settings come from the environment first, then from an env file:
`$XDG_CONFIG_HOME/heraldo/env` (`~/.config/heraldo/env`), or the file
`HERALDO_ENV_FILE` names. The file holds `KEY=VALUE` lines. Keep it mode 600.

| Variable | Meaning |
|---|---|
| `HERALDO_URL` | The server root, such as `https://heraldo.example.net` |
| `HERALDO_TOKEN` | A `read` token |
| `HERALDO_WRITE_TOKEN` | A `write` token, for `ack`, `resolve`, changing checks and minting ping keys |
| `HERALDO_ADMIN_TOKEN` | An `admin` token, for `settings set` and `unset`, and the MCP tool `settings_write`. Default: the write token. |
| `HERALDO_CURSOR` | The read cursor's name. Default: `inbox`, the cursor every client shares. Set another name for a separate cursor. |
| `HERALDO_SOURCE` | The `source` of `ack` and `resolve` events. Default: `heraldo-cli`. |
| `HERALDO_SEND_URL`, `APPRISE_URL` | `send`'s notify URL; the first one set wins |
| `HERALDO_PROJECT` | `send`: the project, when `--project` is not given |
| `NOTIFY_EXTRA_JSON` | `send`: extra JSON fields for the payload, such as `"_nonce": "abc"` |
| `HERALDO_FALLBACK_URL` | `send`: a heraldo server, such as a standby, for when the notify URL fails |
| `HERALDO_FALLBACK_TOKEN` | `send`: a `write` token for the fallback |

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
`HERALDO_EVENT_ID`, `_SOURCE`, `_KIND`, `_ACTION`, `_KEY`, `_HOST`, `_PROJECT`, `_LEVEL`,
`_TITLE`, `_TAGS` (comma-separated) and `_TS`. It does not get
`HERALDO_TOKEN` or `HERALDO_WRITE_TOKEN`.

### send

`heraldo send` keeps the contract of a notify script that posts to Apprise:

- `-t`/`--title` (default `System Notification`), `-m`/`--message`. Without
  `-m`, the message comes from stdin; with a terminal on stdin, `send` fails.
- `--tag`, `--source`, `--url`, `--image` and `--level` add those fields, for an
  apprise-tee front door.
- `--project`, else `HERALDO_PROJECT`, adds `project`, folded to lower case.
  A name that is not valid is dropped with a warning on stderr; the message
  still goes. `send` never reads a project from the working directory.
- With no notify URL set and no `--attach`, `send` does nothing and exits 0.
- `--attach <file>`, repeatable, stores each file with the message (images,
  text, PDF or JSON, 5 MiB each). The notify URL takes no files, so `send`
  then uploads to `HERALDO_URL` with `HERALDO_WRITE_TOKEN` and posts the
  message there as kind `message`. Without both variables it fails.
- Auth: a URL that carries its own key (`/notify/<key>`, a `token`
  parameter, or a user in the URL) is sent as it is. Otherwise, with
  `HERALDO_WRITE_TOKEN` set, `send` adds it as `Authorization: Bearer`, so
  `HERALDO_SEND_URL=<server>/v1/notify` works with no key in the URL. An
  `APPRISE_URL` gets the token only when it is on `HERALDO_URL`'s server, so
  the token never goes to another Apprise service.
- Success prints `✅ Notification sent successfully.` and exits 0. Success
  means HTTP 200 with a `Sent` line in the answer.
- Anything else prints `❌ Failed to send notification. HTTP Status: <code>` and
  exits 1. A request that got no answer reports `000`.
- Each connection gets 5 s to open, so a host that is down fails fast.
- With `HERALDO_FALLBACK_URL` and `HERALDO_FALLBACK_TOKEN` set, a failed notify
  URL is retried as `POST /v1/events` on that server. The event has kind
  `message`; `--tag` becomes `tags`; a `--level` that is an envelope level
  becomes `level`, any other `--level` becomes the first tag; `project` is
  kept. When the
  fallback stores it, `send` prints the sent line and names the fallback on
  stderr. When both fail, the failed line carries the notify URL's status.

## iOS app

The app ships no token. In Settings, enter the server URL and the device's
read and write tokens. The app keeps them in iCloud Keychain, so they reach
the person's other devices; the notification extension reads them through the
app group.

- **Inbox**: history synced by cursor on launch and on foreground, search,
  filters by kind, source, project and hold state (the Held filter shows the
  triage verdict on each row). Routine events (passing check-ins, progress,
  status) stay out: the app syncs its cache with `routine=exclude`, so pings
  do not crowd real history out of the 3000 events it keeps. On a server
  before the filter the app drops them itself. Show routine events in the
  filter menu, or a kind filter of their kind, asks the server for them. The
  filters you choose are saved and synced (see "Display"). Each row shows its project. A swipe marks events read up to that one; the
  menu marks all read. Both move the shared `inbox` cursor, so the CLI and the
  app share one unread state. The Inbox count is the cursor's `unread`; the icon badge is its `badge`.
- **Incidents**: open incidents first, then acknowledged ones, from
  `/v1/state`, with Ack; then a Stale section, the incidents the server marks
  stale (`stale.enabled`); then the ones resolved in the last day, with how
  long each was open. With more than one project, each project gets a section.
- **Check-ins**: every check from `/v1/checkins`, with its state, next due
  time, last run length and last log. A swipe pauses or resumes a check (write
  token). The tab's badge counts the checks that are down. With more than one
  project, each project gets a section. A check's History lists its pings.
- **Event detail**: History lists every event of the event's key: a check's
  pings, an activity's progress, an incident's updates. Mute project for 1 h
  in the menu, and in an incident's context menu, mutes every event of the
  event's project (write token). Delivery says why the event did or did not
  push, stage by stage, and the outcome on each device.
- **Push**: the app registers its APNs token with `POST /v1/devices` on every
  launch, and again when a permission changes. A Debug build reports
  `sandbox`; a TestFlight or App Store build reports `production`.
- **Notifications**: `incident` has Ack, Snooze 1 h, Mute 1 h and Open;
  `event` has Mute 1 h and Open. Mute 1 h mutes an incident's key, or an
  event's source (`POST /v1/mutes`, write token). Snooze 1 h mutes the
  incident's key and pushes it again in an hour if it is still open
  (`remind: true`). An incident's context menu has Snooze 1 h too, and a
  flapping incident's row shows a "flapping" badge. A resolve replaces the incident's
  notification with "Resolved after 12 min". A passive event shows no
  notification; it waits in the inbox. The extension fetches the full
  event by id and attaches the image, else the source's icon from the
  server's registry. A digest keeps its one line; a tap opens the full text.
- **Settings**: the mutes in force, with End. Quiet hours and the badge are
  server settings, shown here for every device: with an admin token (an
  optional field beside the read and write tokens) the app changes them;
  without one they are read-only.
- **Focus filter**: Settings > Focus > a Focus > Focus Filters > Heraldo
  picks the projects and the level (Time Sensitive and up, or Critical only)
  that break through that Focus. While the Focus is on, the server sends this
  device's other pushes at `active`, which the Focus silences; critical always
  breaks through. Off by default. The system Focus works as usual without one.
- **Badge**: the icon badge follows the server's `badge.scope` (default:
  open incidents, down checks and unread time-sensitive alerts). The Inbox
  tab keeps its own unread count.
- **Saved views**: the Inbox filter menu has Save as View, which saves the
  current filter under a name (write token), and on iOS a View picker that
  scopes the list to a view. On the Mac, the sidebar lists the views under
  Projects; selecting one scopes the list, and its context menu has Delete.
  A view with a bundle folds a burst of its pushes into one notification
  ([docs/api.md](docs/api.md), "Saved views").
- **Display**: these preferences sync to your other devices through the
  iCloud key-value store, with a copy on the device:
  - Group notifications: by thread or source (the default), or by project.
    By project, the notification extension threads each notification as
    `project:<name>`; an event with no project keeps the server's thread.
  - Open on: the tab or saved view the app opens on.
  - The Inbox's filters, Show routine events included.
  - Share to project: the project that sharing to Heraldo offers first. It
    is the share sheet's last project too, so a share on one device sets it
    on the others after the app's next sync.

  Text size, layout, the Dock setting, the push token, permissions and the
  caches stay on each device. A build without the iCloud key-value
  entitlement keeps the preferences on the device, and Settings says so.
- **Widgets**: on the Lock Screen, Needs attention (circular, rectangular and
  inline) counts open incidents and down checks. On the Home Screen or Lock
  Screen, Project shows one project's open incidents, down checks and newest
  events; edit the widget to choose the project. The widgets read the server
  with the shared read token, and fall back to the copy the app saves after
  each sync. Each sync and each push reloads them.
- **Live Activities**: a `progress` key shows as a Live Activity on the Lock
  Screen, in the Dynamic Island and in the Watch Smart Stack. The server starts
  it with push-to-start, so the app need not be open. The app reports its
  push-to-start token with the registration, and each activity's update token
  and dismissal (docs/api.md, "Live Activities"). The inbox leaves progress
  events out; each activity leaves one message when it ends, and its History
  lists the progress.
- **Share in**: Heraldo is in the share sheet. It takes text, a link,
  images and files (images, plain text, PDF or JSON; 5 MB each, 8 at most;
  a larger image goes up as a smaller JPEG). The sheet has a title, a project
  picker (from `/v1/projects`; it remembers the last one) and "Add to open
  incident" (from `/v1/state`), which posts `kind incident, action update` on
  that key. It posts level `passive` with source `share:<device>`, with the
  files as attachments (docs/api.md, "Attachments"). It needs the write token.
- **Share out**: Share on event detail sends the event as markdown, ending
  with its `heraldo://event/<id>` link. `heraldo show --md` prints the same
  text.
- **Shortcuts**: Send to Heraldo (with the write token; optional files,
  project and open incident), Get Event (markdown, JSON or its link, from an
  id or a `heraldo://event/` link), Get Recent History (JSON), Get Open
  Incidents (open and acknowledged, as text or JSON), and Start, Update, End
  and Get Live Activity, with Notify!'s parameters.
- `heraldo://event/<id>` opens one event. `heraldo://open?url=<url>` opens the
  URL; a Live Activity's open button uses it.

## Mac app

The same app for macOS 15 or later, signed with Developer ID and notarized.
A GitHub release carries it as `Heraldo-<version>-macos.zip`; the Homebrew
cask installs it:

```sh
brew install --cask noahkiss/tap/heraldo-app
```

- **Window**: three columns. The sidebar lists the homes, Inbox, Incidents,
  Check-ins, Live Activities and Held, each with its count. Held shows only
  while the hold has open entries. Below them, Projects lists each project
  from `/v1/projects`; a red mark shows an open incident or a down check. The
  middle column lists the chosen home or project, and the right column shows
  the selected event, incident, check, activity or held alert. Return or a
  double-click opens the item in its own window.
- **Toolbar**: search, the Inbox filters (kind, source, project, hold,
  routine events), refresh, and Mark all read.
- **Held** reads the hold with `peek=1`, so viewing it never counts as a
  triage poll.
- **Keyboard**: the menus list every shortcut.

  | Menu | Command | Shortcut |
  |---|---|---|
  | Edit | Find | Command-F |
  | View | Refresh | Command-R |
  | View | Inbox, Incidents, Check-ins, Live Activities, Held | Command-1 to Command-5 |
  | Event | Open in New Window | Command-O |
  | Event | Open Link | Shift-Command-L |
  | Event | Mark Read up to Here | Shift-Command-U |
  | Event | Mark All Read | Option-Shift-Command-U |
  | Event | Acknowledge | Shift-Command-A |
  | Event | Pause Check or Resume Check | Shift-Command-P |
  | Event | Mute Project for 1 h | none |

  The arrow keys move the selection. The Event menu's writes need the write
  token.
- **Menu bar**: a bell with the badge count, as on the Dock. The bell gains a badge while
  the count is above 0, and fills while an incident is open or a check is down.
  The panel shows a one-line summary, the open incidents with Ack, the checks
  that are down, and the running Live Activities. A click on an item opens it
  in the window.
- **Settings** is in the app menu (Command-comma).
- **Share**: Heraldo is in every app's Share menu, with the same sheet as on
  the iPhone. Services > Send to Heraldo sends the selected text or link as a
  passive event under the share sheet's last project. Share on event detail
  sends the event as markdown with its link.
- **Credentials**: the same iCloud Keychain item as the iPhone, so tokens saved
  on one device reach the other. The share extension reads that item too, so
  it needs its own Developer ID profile with the app group
  (`apple/Config/Local.example.xcconfig`); an unsigned build's extension
  cannot read this Mac's login keychain item and asks you to open the app.
- **Push**: the Mac registers its own APNs token with `platform` `macOS` and
  environment `production`, and reports the same permission health. The Dock
  icon shows the cursor's `badge` (`badge.scope`).
- The app syncs every minute, on wake and on a push. Closing the window keeps
  the app running in the menu bar. Quit is in the menu bar item's menu.
- **Dock**: with "Show in Dock only while a window is open" on (Settings, on by
  default), the Dock icon and the Command-Tab entry go away when the last
  window closes, the Settings window included. Opening a window from the menu
  bar, a notification or the app itself brings them back. The setting stays on
  this Mac.
- **Display**: the Inbox filters and Open on sync with the iPhone through
  iCloud (see "iOS app"). Open on can name a home or a saved view in the
  sidebar. With Last used, the window opens on the home it showed last. Notification grouping by project is iOS only: the
  Mac has no notification extension.
- **Widgets**: the Mac widgets are a placeholder for now. The Mac widget
  extension has no app group, so it cannot read the app's copy.

### Provision from the shell

`apple/scripts/provision.sh` saves the URL and tokens without a paste, for a
setup script or an agent. The Mac app saves them to the synced Keychain item,
so they also reach the iPhone.

```sh
HERALDO_URL=https://heraldo.example.com \
HERALDO_READ_TOKEN_CMD='<command that prints the read token>' \
HERALDO_WRITE_TOKEN_CMD='<command that prints the write token>' \
  apple/scripts/provision.sh
```

- The script quits the app and launches it once with `--provision-stdin`. It
  passes `{"url", "read_token", "write_token"}` as JSON through a FIFO to the
  app's stdin (`open --stdin`).
- The script prints the app's one log line, `provision: saved ...` or
  `provision: failed: ...`, and exits 0 or 1.

The app keeps six rules for this path:

1. **Write-only.** Nothing reads the credentials back out, prints them or
   exports them. The log names the outcome and the server host, never a
   token.
2. **Launch-time only.** Only the `--provision-stdin` flag with stdin reaches
   it. No URL scheme, App Intent, XPC service or file drop does, so a web page
   or another app's link cannot set the credentials.
3. **Checked before saving.** The read token must pass the "Save and test"
   check. The write token must pass a scope check: an empty `POST /v1/events`,
   which the server refuses with 400 after the token check and before it
   stores anything. On any failure the app saves nothing.
4. **No silent takeover.** When settings for a different server exist, the app
   refuses unless it also gets `--replace` (`HERALDO_PROVISION_REPLACE=1`). New tokens
   for the same server need no flag.
5. **Visible.** After a save the app posts a local notification, "Heraldo
   settings were set from the command line", with no values.
6. **Pipe only.** stdin must be a pipe or FIFO. The app refuses a terminal, so
   no secret is typed where scrollback can keep it.

A Mac build without a Developer ID profile signs ad hoc. It has no push, and it
keeps the credentials in this Mac's login keychain. See
`apple/Config/Local.example.xcconfig`, step 3.

## License

MIT. See [LICENSE](LICENSE).
