# API

The source of truth is [schema/openapi.yaml](../schema/openapi.yaml) and
[schema/envelope.v1.schema.json](../schema/envelope.v1.schema.json). This page
is a summary.

## Endpoints

An endpoint with a stage number is planned for that stage and has no handler
yet. The spec marks it `x-stage: <n>`. Push notifications go out only with
`HERALDO_PUSH=on` (see "Push" below).

| Method and path | Does | Stage |
|---|---|---|
| `GET /healthz` | Liveness, with `db` and the event count. 503 when the database does not answer. No auth. | served |
| `POST /v1/events` | Stores one `EventInput`. Returns the stored `Event` (201), or the first event for a repeated `dedup` (200). | served |
| `GET /v1/events?after=&before=&order=&limit=&source=&kind=&host=&key=&tag=&q=&hold=` | Lists events after a cursor, in arrival order. `q` is full-text search over title, summary and body. `order=desc` lists newest first. `hold` keeps events whose `hold.state` matches. | served |
| `GET /v1/events/{id}` | Returns one event with its full body. | served |
| `GET /v1/send?source=&kind=&...` | Same as `POST /v1/events` for GET-only clients. Scalar fields and `tags` (repeat the parameter) only. | served |
| `GET /ping/{slug}`, `/start`, `/fail`, `/log`, `/{exit_code}` | Check-in pings in the healthchecks.io shape. `?rid=` pairs runs; `?create=1` creates an unknown check. | 6 |
| `POST /notify/apprise`, `POST /notify/{key}` | Apprise-compatible ingest. Answers 200 with `Sent`. | 11 |
| `GET /v1/state` | Flat arrays of the latest event per key: incidents not resolved, progress not ended, every check-in, set statuses. The shape is a stand-in until stage 12. | served |
| `GET /v1/stream` | Server-sent events: one `poke` per stored event, with the newest cursor. | served |
| `GET /v1/cursors/{name}`, `PUT /v1/cursors/{name}` | The read cursor called `name`, shared by every client, with its `unread` count. | served |
| `GET /v1/devices`, `POST /v1/devices` | Lists devices; registers a device or refreshes the one with that APNs token. | served |
| `GET`, `PUT`, `DELETE /v1/devices/{id}` | One device: read, replace its registration, retire it. | served |

Auth:

- Every call except `/healthz` takes a bearer token. Each token has a scope:
  `write` (post and send), `read` (list, get, state, stream, the read
  cursors and devices), or `admin` (both).
- A `read` token may register, update and remove devices, as it may move the
  read cursors. The apps hold a token pair, like the CLI: a `read` token for
  history, cursors and device registration, and a `write` token for the
  notification actions (Ack) and for sending an event.
- A missing or unknown token gets 401. A token without the scope gets 403.
- `GET /v1/send` also takes the token as a `token` query parameter. A token in
  the URL ends up in access logs, proxy logs and browser history. Use the header
  when the client can set one. Other calls refuse the parameter.
- The server stores each token's SHA-256 only. The README shows the token file.
- A write token with the `trusted-ingest` capability is a bridge, such as
  apprise-tee. Its events get two extras:
  - `details.tee.hold`, an object, is copied into `hold` with
    `store_only: true`. A store-only event is never pushed.
  - Its `dedup` id matches every earlier event, not only the last 24 hours.
    The tee sends its catalogue id as `dedup`.

Limits and errors:

- A `POST` body over 1 MiB gets 413. The `body` field may hold 256 KiB, counted
  in bytes.
- `content.steps.current` may not exceed `total`.
- An input that fails the schema gets 400 with the failing fields in `error`.

Paging:

- `next_cursor` comes with every page that has events. Pass it as `after`.
- An empty page means the client is up to date. Keep the last cursor and poll
  with it again.
- The order is arrival at this server. An event imported later still appears
  after every cursor handed out before it.
- `order=desc` lists newest first. Its `next_cursor` is the cursor of the
  oldest event on the page; pass it as `before` for the next page. `before`
  also works with the default order.
- Every list answer has a `Heraldo-Order` header, `asc` or `desc`. A server
  before stage 3 ignores `order`, `before` and `hold` and sends no header, so a
  client can tell.

Read cursors:

- A cursor is one unread state on the server, like an email inbox. Every
  client that names it shares it: the CLI on each machine and the apps.
  `inbox` is the CLI's default name.
- A client moves it with `PUT /v1/cursors/{name}` and
  `{"cursor": "<a list cursor>"}`. `name` is 1 to 64 of `A-Z a-z 0-9 . _ -`.
- Any `read` token may read and move any cursor. The server records which
  token moved it last.
- A cursor only moves forward. A `PUT` at or before the stored cursor changes
  nothing, and the answer is the stored cursor. So clients that share a
  cursor never move it back. A cursor past the newest event gets 400.
- `GET` answers 404 for a name with no cursor.
- Every answer carries `unread`: the events after the cursor that push an
  alert (see "Push"). It is the app's badge. Held and store-only events,
  incident acks, progress, status and passing check-ins do not count.

Event stream:

- `GET /v1/stream` is server-sent events. Each stored event sends
  `event: poke` with `data: {"cursor": "<newest cursor>"}`. The first poke
  comes on connect and carries the current newest cursor.
- A poke carries no event. The client lists the events after its own cursor,
  so a missed poke loses nothing. Pokes coalesce: a slow client gets the newest
  cursor, not every one.
- A `: keep-alive` comment comes every 25 seconds. The server ends open streams
  when it shuts down; a client reconnects and fetches what it missed.
- An event that `import-tee` stores in another process sends no poke.

Dedup:

- A `dedup` id seen in the last 24 hours, from any source, stores nothing. The
  server answers 200 with the first event.
- For a `trusted-ingest` token and for `import-tee`, the window has no end.

## Devices

The apps register each device for push with their `read` token. The server knows a device by its APNs
token and gives it an id, `dev_` plus a ULID. The app keeps the id.

`DeviceRegistration`, the body of `POST /v1/devices` and `PUT /v1/devices/{id}`:

| Field | Type and limit | Meaning |
|---|---|---|
| `token` | hex, 32 to 200 characters, required | The APNs device token. Stored in lower case. |
| `environment` | `sandbox` or `production`, required | The APNs environment of the token. A development build is `sandbox`; TestFlight and App Store builds are `production`. |
| `topic` | string, 255, required | The app's bundle id, sent as `apns-topic`. |
| `name` | string, 128 | The device's name. |
| `platform` | string, 32 | Such as `iOS` or `macOS`. |
| `app_version` | string, 64 | The app's version and build. |
| `permissions` | `{alerts, time_sensitive, live_activities}`, booleans | Permission health. A field left out is unknown. |

- `POST` with an unknown token creates a device: 201. With a known token it
  updates that device: 200. Either way the device is active.
- `PUT /v1/devices/{id}` replaces the registration, token included, such as
  after APNs gave the app a new token. 404 for an unknown id; 409 when another
  device holds the token.
- Both replace every field: a field left out is cleared.
- `DELETE /v1/devices/{id}` retires the device with reason `removed`. Nothing
  is deleted; the device stays in the list with its delivery history.
  Registering its token again brings it back.
- An unknown field, a bad token or a bad environment gets 400.

`Device`, the answer: `id`, `environment`, `topic`, `token_suffix` (the last 8
characters of the token; the server never returns the full token), `name`,
`platform`, `app_version`, `permissions`, `active`, `registered_at`,
`updated_at`, and for a retired device `retired_at` and `retired_reason`
(`removed`, or the APNs reason such as `apns: Unregistered`).

Permission health:

- A permission reported `false` opens an incident with source `heraldo`,
  key `device:<id>:<permission>` and level `active`, such as
  `device:dev_01J...:time_sensitive`. Reporting it `false` again raises
  nothing new.
- Reported `true` later, the incident resolves (level `passive`). Removing the
  device resolves its open permission incidents.
- `<permission>` is `alerts`, `time_sensitive` or `live_activities`.

## Push

Push is off until `HERALDO_PUSH=on`, and needs an APNs key (README,
"Configuration"): a mounted `.p8` file (`HERALDO_APNS_KEY_FILE`) or its PEM text
in `HERALDO_APNS_KEY`. Off, the server stores events and registers devices as
usual and sends nothing. `/healthz` reports `push`: `off`, `on`, or `no-key`
when push is on but no key loads.

The server sends each new event that pushes to every active device, straight
to APNs over HTTP/2, signed with an ES256 provider token. It signs a new token
every 50 minutes, and at once when APNs answers `ExpiredProviderToken`. The
device's `environment` picks the APNs host.

Ingest never waits for APNs. A stored event goes into an in-memory queue and
workers send it. A full queue (1024 events) drops the push and logs it, and a
restart drops queued pushes. The event is stored either way: the app syncs
history by cursor, so a missed push loses nothing.

Which events push, and how loud:

| kind | action | Push | interruption-level |
|---|---|---|---|
| `message` | none | yes | the event's `level`, default `active` |
| `digest` | none | yes | `passive` (silent) |
| `incident` | `open`, `update`, none | yes | the event's `level` |
| `incident` | `resolve` | yes | `passive` |
| `incident` | `ack` | no | |
| `checkin` | `fail`, or any but `log` with a non-zero `exit_code` | yes | the event's `level` |
| `checkin` | `start`, `success`, `log` | no | |
| `progress` | any | no (Live Activities, stage 7) | |
| `status` | any | no (widgets) | |

- Before the table: a held event (`hold.state` is `held`), a store-only event
  (`hold.store_only`), and an event past `expires_at` never push.
- A quiet message is one with `level: passive`. An ingest rule can set it.
- `critical` goes out as `time-sensitive`: the app has no critical-alerts
  entitlement. `delivery` records `level: time-sensitive` and the note
  `critical sent as time-sensitive` in `error`.
- A passive push has no sound and `apns-priority` 5; the others have 10.

Answers from APNs:

| Answer | The server |
|---|---|
| 200 | Records `sent`. |
| 410, or 400 `BadDeviceToken` or `DeviceTokenNotForTopic` | Records `failed` and retires the device with reason `apns: <reason>`. |
| 403 `ExpiredProviderToken` or `InvalidProviderToken` | Signs a new provider token and sends once more. |
| 429, 5xx, or no answer | Retries after 1 s, 4 s, 15 s and 1 min (or `retry-after`, up to 5 min), then records `failed`. |
| Any other | Records `failed` with the reason. |

`delivery` on an event, from `GET /v1/events` and `GET /v1/events/{id}`,
lists the outcome per device: `device`, `outcome`, `level`, `at`, `error`. An
event that did not push has no `delivery`.

### Payload

```json
{
  "aps": {
    "alert": {"title": "Web is down", "body": "nginx failed"},
    "badge": 3,
    "sound": "default",
    "thread-id": "uptime",
    "category": "incident",
    "mutable-content": 1,
    "interruption-level": "time-sensitive",
    "relevance-score": 0.8
  },
  "heraldo": {
    "id": "01JBA6Y8Q2K9M3T4V5W6X7Y8Z9",
    "kind": "incident",
    "source": "uptime",
    "key": "unit:web-1:nginx",
    "action": "open",
    "url": "https://example.com/status",
    "image": "https://example.com/graph.png",
    "truncated": true
  }
}
```

| Payload | From |
|---|---|
| `alert.title` | `title`, else `source` |
| `alert.body` | `summary`, else `body`. A body is cut at a character boundary, with `…`, so the payload fits 4 KB. |
| `badge` | `unread` of the `inbox` cursor when the push goes out |
| `sound` | `sound`, default `default`; none for `passive` or `sound: none` |
| `thread-id` | `thread`, else `source` |
| `category` | `incident` for an incident `open`, `update` or no action; `event` for the rest. The app registers both: `incident` has Ack and Open; `event` has Open. Mute waits for a server-side mute. |
| `mutable-content` | Always 1, so the notification service extension runs. |
| `interruption-level` | The table above. |
| `relevance-score` | `relevance` |
| `heraldo.truncated` | The alert does not carry the full text: the body was cut, or the alert shows the summary of an event that also has a body. The extension fetches `GET /v1/events/{id}`. |

Headers: `apns-push-type: alert`; `apns-topic` from the device; `apns-collapse-id`
is the first 32 hex characters of the SHA-256 of `key`, when the event has one;
`apns-expiration` is `expires_at` when set, and absent otherwise, so APNs keeps
the push for an offline device.

## Input and stored events

- **`EventInput`** is what a sender posts. It is the schema root.
- **`Event`** is what the read API returns: `$defs/Event` in the schema.
- The server owns `id`, `received_at`, `hold` and `delivery`. An input that sets
  any of them is rejected. `ts` stays the sender's; the server fills it with the
  arrival time when it is missing.

## Envelope v1

`source` is required. `kind` picks the meaning of `key` and `action`.
An input without `kind` is untyped: the ingest rules may type it, and the
server stores it as `message` when no rule matches. An untyped input takes
what a `message` takes: no `action`, `content` or `checkin`. An explicit
`kind`, `message` included, is final. A stored event always has a `kind`.

| kind | key | action | content | checkin |
|---|---|---|---|---|
| `message` | optional | none | no | no |
| `digest` | optional | none | no | no |
| `incident` | required | `open`, `update`, `ack`, `resolve` | no | no |
| `progress` | required | `start`, `update`, `end` | optional | no |
| `checkin` | required | `start`, `success`, `fail`, `log` | no | optional |
| `status` | required | `set` or `clear`, required | required on `set` | no |

`status` is a named live value for widgets and the status view. It never sends
an alert push, only a widget push.

Sender fields:

| Field | Type and limit | Meaning |
|---|---|---|
| `ts` | date-time | When the event happened. |
| `source` | string, 128 | The sender. One thread and icon per source by default. |
| `host` | string, 128 | The machine the event is about. Status views group by host. |
| `key` | string, 255 | Groups related events. |
| `title` | string, 256 | Notification title. |
| `summary` | string, 178 | The one-line push text. A digest pushes only this. |
| `body` | markdown, 256 KiB | Stored in full; a push carries the summary or a truncated copy. |
| `level` | `passive`, `active`, `time-sensitive`, `critical` | Interruption level. Default `active`. |
| `thread` | string, 128 | Notification thread. Defaults to the source. |
| `url`, `image` | URI | Tap target and attached image. |
| `icon` | string, 512 | Source icon: a URL or a symbol name. |
| `sound` | string, 64 | `default`, `none`, or a sound bundled with the app. |
| `tags` | up to 16 strings of 64 | Labels for rule matching, such as Apprise tags. |
| `dedup` | string, 128 | Idempotency id. A repeat within 24 hours is dropped. |
| `expires_at` | date-time | The event is stale after this. |
| `relevance` | number 0–1 | Rank against other events from the app. |
| `actions` | up to 4 `Action` | Notification buttons. |
| `content` | `LiveContent` | Live Activity or widget content. |
| `checkin` | `Checkin` | Run details for kind `checkin`. |
| `details` | object | Namespaced extras, such as `{"backup": {"bytes": 1024}}`. |

### Action

`id`, `title` and `type` are required.

| type | Handled by | url | method |
|---|---|---|---|
| `open` | The device opens `url`. | required | not allowed |
| `http` | The device calls `url`, so it can reach LAN hosts. | required | `GET` or `POST`, default `POST` |
| `server` | heraldo. `id` is `ack`, `resolve` or `mute`. | not allowed | not allowed |

`foreground` brings the app forward on tap. `destructive` styles the button.

### LiveContent

Shared by Live Activities (`progress`) and widgets (`status`). On an update, a
field left out keeps its value and a field set to `null` clears it.

| Field | Limit |
|---|---|
| `title` | 120 |
| `symbol` | SF Symbol name |
| `tint` | `#RRGGBB`, `#RRGGBBAA`, or a system color name |
| `body` | 300 |
| `progress` | 0–1 |
| `steps` | `{total, current}` |
| `ends_at` | date-time, absolute |
| `trailing`, `status` | 40 each |
| `metrics` | up to 6 of `{label, value, unit, color}` |
| `button` | one `Action` |
| `stale_after` | seconds |
| `keep_for` | seconds the ended activity stays, on `end` |

### Checkin

`{rid, exit_code, duration_ms, schedule}`. `rid` pairs a start with its end.
`schedule` is `{period_s}` or `{cron, tz}`, plus `grace_s` and `max_runtime_s`.
The server uses `schedule` only to create a check it does not know yet; after
that, the check's config lives on the server.

### Server fields

| Field | Meaning |
|---|---|
| `id` | Event id: a ULID (26 characters, sortable by time). Live events get the arrival time. |
| `received_at` | When the server stored the event. |
| `hold` | Triage state. |
| `delivery` | Per-device outcome: `sent` or `failed`, with the level the push carried. `skipped` is reserved for device settings. See "Push". |

## APNs mapping

| Envelope | APNs |
|---|---|
| `thread` | `thread-id` |
| `key` | `apns-collapse-id`: the first 32 hex characters of the key's SHA-256 |
| `level` | `interruption-level`. Without the entitlement, `critical` goes out as `time-sensitive` and `delivery` records the downgrade. |
| `expires_at` | `apns-expiration` |
| `relevance` | `relevance-score` |
| `summary`, `body` | The push carries the summary, or the body truncated to fit 4 KB. The notification service extension fetches the full text by `id`. |
| `thread`, `source` | `thread-id`: `thread`, else `source` |

"Push" above has the full payload.

## Examples and types

- `schema/examples/valid` and `invalid` hold sender inputs. `stored` holds
  events as the read API returns them.
- `go test ./api` checks each example against the schema and the generated Go
  types.
- `go generate ./...` regenerates `api/types.gen.go`. oapi-codegen cannot follow
  `$defs` references, so `api/internal/bundle` inlines the JSON Schema into the
  OpenAPI document first.

## Backfill from apprise-tee

`heraldo-server import-tee <dir>` reads every `notifications-YYYY-MM.jsonl` file
in `dir` and stores each line as an untyped event. When `HERALDO_RULES` is set,
the rules type each event as they would a live post; without rules, or when
no rule matches, it is a `message`. The summary counts
those as `typed`. A rerun skips lines it already stored, so it never retypes
them.

| Catalogue field | Event field |
|---|---|
| `id` | `details.tee.id` and `dedup`. The event id is a ULID made from `ts` and this id, so a rerun makes the same id. A catalogue id that is already a ULID is kept. A line without an `id` gets a legacy id, and the import uses it in the same places. |
| `ts` | `ts` and `received_at` |
| `source` | `source`, or `apprise-tee` when the entry has none |
| `title`, `body` | `title`, `body`, cut to the envelope limits |
| `tag` | `tags`, split on commas and spaces |
| `url`, `image` | `url`, `image` |
| `level` | `level` when it is an envelope level, else `details.tee.level` |
| `hold`, `hold_id`, `hold_reason`, `hold_ids` | `hold` as `{state, id, reason, ids, store_only: true}` |
| `client`, `path`, `status`, `delivered`, `ms`, `backends`, `time` | `details.tee` |

A rerun stores nothing twice. An entry whose id an earlier live event already
used as `dedup` is skipped, so live forwarding and the backfill store each
record once. A line that is not JSON, has no `ts`, or does not make a
valid envelope, is logged and skipped, and counts as `bad` in the summary.

The tee wrote lines without an `id` until it learned ids. Such a line gets the
legacy id `legacy-` plus the first 20 hex characters of the SHA-256 of the
line, without its line ending. A rerun over the same files makes the same
legacy ids, so it stores nothing twice. The tee's own ids start with `n-`, so a
legacy id never matches one. The summary counts these lines as `legacy_ids`.

## Ingest rules

Most legacy senders post a title, a body and a rung tag, nothing more.
Ingest rules turn such a post into a typed event: an incident that opens or
resolves, a digest, a check-in, or a message with another level.
`HERALDO_RULES` names the rules file; without it every untyped input is
stored as a `message`.

The rules apply to an untyped input, one without `kind`, on
`POST /v1/events`, `GET /v1/send` and `import-tee` (catalogue lines are
untyped). A sender that sets `kind`, `message` included, is never retyped.
An untyped input that no rule matches is stored as `message`, with the
sender's other fields as sent.

```yaml
rules:
  - name: unit-failed
    match:
      title: '^(?P<host>[\w.-]+): \d+ unit\(s\) failed'
      body: '^(?P<unit>[^\s,]+)'
    set: {kind: incident, action: open, key: 'unit:{host}:{unit}', host: '{host}'}
  - name: unit-recovered
    match:
      title: '^(?P<host>[\w.-]+): \d+ unit\(s\) recovered'
      body: '^(?P<unit>[^\s,]+)'
    set: {kind: incident, action: resolve, key: 'unit:{host}:{unit}', level: passive}
```

- Rules run in file order. The first rule that matches wins; the rest are
  not tried.
- `match` takes Go RE2 regular expressions on `source`, `host`, `tag`,
  `title` and `body`. Every field a rule names must match. `tag` matches when
  any one tag does, and `^$` matches an untagged event. A missing field reads
  as the empty string. A rule with no `match` matches everything.
- Named groups, `(?P<name>...)`, are captures. `{name}` in `key`, `host` or
  `thread` is replaced with the captured text. A placeholder that names no
  capture is an error when the file loads.
- `set` takes `kind` (required: `message`, `digest`, `incident` or
  `checkin`), `key`, `action`, `level`, `thread` and `host`. `incident` and
  `checkin` need a `key`. A field the rule leaves out keeps the sender's
  value. A value longer than the envelope allows is cut.
- The stored event records the rule in `details.heraldo.rule`.
- When a rule's result is not a valid envelope (a capture came out empty, so
  an incident has no key), the server logs a warning and stores the event as
  a `message`.
- The server checks the file every 5 seconds and reloads it when its content
  changed. A file that does not parse, or is gone, keeps the rules in force
  and logs an error. A file that does not parse at startup stops the server.

An incident's recovery pairs with its failure only when both rules build the
same key. One event carries one key, so a post that lists several items
(units, containers) can key on one of them only.

`heraldo-server replay-tee -rules <file> <dir>` runs a catalogue through a
rules file in memory, with no database, and prints:

- the recoveries (incident resolves) that paired with an open incident under
  their key, out of all recoveries, and each unpaired one;
- the incidents still open at the end;
- the hits per rule;
- every source and first tag that no rule matched, with a count and a title.
