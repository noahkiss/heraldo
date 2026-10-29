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
| `GET /healthz` | Liveness, with `db`, the event count and, with push on, `unsent` ("Push"). 503 when the database does not answer. No auth. | served |
| `POST /v1/events` | Stores one `EventInput`. Returns the stored `Event` (201), or the first event for a repeated `dedup` (200). | served |
| `GET /v1/events?after=&before=&order=&limit=&source=&kind=&host=&key=&tag=&q=&hold=&project=&routine=` | Lists events after a cursor, in arrival order. `q` is full-text search over title, summary and body. `order=desc` lists newest first. `hold` keeps events whose `hold.state` matches. `project` takes a list (see "Projects"). `routine=exclude` or `only` filters routine events (see "Routine events"). | served |
| `GET /v1/events/{id}` | Returns one event with its full body. | served |
| `GET /v1/send?source=&kind=&...` | Same as `POST /v1/events` for GET-only clients. Scalar fields and `tags` (repeat the parameter) only. | served |
| `GET` or `POST /ping/{slug}`, `/start`, `/fail`, `/log`, `/{exit_code}` | Check-in pings in the healthchecks.io shape, with a write token. `?rid=` pairs runs; `?create=1` creates an unknown check. Answers `OK`. See "Check-ins". | served |
| `GET` or `POST /p/{key}`, `/start`, `/fail`, `/log`, `/{exit_code}` | The same pings with a check's ping key and no bearer token. | served |
| `GET /v1/checkins?project=`, `GET /v1/checkins/{slug}` | Every check, or one: schedule, state, next due time, last run length, last log. | served |
| `PUT /v1/checkins/{slug}` | Creates a check, or replaces a known check's host and schedule. | served |
| `POST /v1/checkins/{slug}/pause`, `/resume` | Pauses or resumes a check. | served |
| `POST`, `DELETE /v1/checkins/{slug}/key` | Mints a check's ping key (replacing the old one), or removes it. | served |
| `POST /notify/{key}`, `POST /v1/notify` | Apprise-compatible ingest: JSON, form or multipart. Answers 200 with a `Sent` line, or 424. See "Apprise compatibility". | served |
| `GET /recent?n=&tag=&source=`, `GET /entry/{id}` | The newest events, or one, in the apprise-tee catalogue's shape. | served |
| `GET /v1/hold?state=&peek=`, `POST /v1/hold/release`, `POST /v1/hold/suppress` | The alert hold: its queue, and closing entries. Also served as `/held`, `/held/release` and `/held/suppress`. See "Hold". | served |
| `GET /v1/state?project=` | Flat arrays of the latest event per key: incidents not resolved, progress not ended, every check-in, set statuses. The shape is a stand-in until stage 12. | served |
| `GET /v1/projects` | The projects the stored events name, with counts. See "Projects". | served |
| `GET /v1/stream` | Server-sent events: one `poke` per stored event, with the newest cursor. | served |
| `GET /v1/cursors/{name}`, `PUT /v1/cursors/{name}` | The read cursor called `name`, shared by every client, with its `unread` count. | served |
| `GET /v1/devices`, `POST /v1/devices` | Lists devices; registers a device or refreshes the one with that APNs token. | served |
| `GET`, `PUT`, `DELETE /v1/devices/{id}` | One device: read, replace its registration, retire it. | served |
| `GET /v1/activities?all=` | Live Activities, one per progress key: running ones, or every one with `all=1`. | served |
| `GET /v1/activities/{key}` | One activity, with its instances on each device and its timeline. The key may hold slashes. | served |
| `PUT /v1/activity-instances/{id}` | The app reports an activity's update token, or that it was dismissed or ended. | served |
| `POST`, `DELETE`, `GET /live-activity/{id}`, `GET /link`, `POST /notify-json/{id}` | Notify! compatibility, for senders written for Notify!. Write token as `?token=`. | served |
| `GET /v1/mutes`, `POST /v1/mutes`, `DELETE /v1/mutes/{id}` | Mutes in force; mute a source or key for a while; end a mute. | served |
| `GET /v1/icons`, `GET`, `PUT`, `DELETE /v1/icons/{name}` | The icon registry: list, fetch an image, store one, remove one. | served |
| `GET /v1/standby/devices` | Every device with its full push tokens, for a standby. Admin, or a `read` token with the `standby` capability. See "Standby". | served |
| `POST /v1/standby/import` | Stores the events a standby stored, by their ids, and pushes none of them. Same token as above. | served |

Auth:

- Every call except `/healthz` and `/p/{key}` takes a bearer token. Each
  token has a scope: `write` (post and send, and the mutes), `read` (list,
  get, state, stream, the read cursors, devices, and the icons), or `admin`
  (both, and storing and removing icons).
- A `read` token may register, update and remove devices, as it may move the
  read cursors. The apps hold a token pair, like the CLI: a `read` token for
  history, cursors and device registration, and a `write` token for the
  notification actions (Ack, Mute 1 h) and for sending an event.
- A missing or unknown token gets 401. A token without the scope gets 403.
- `GET /v1/send` and the Notify! compatibility paths (`/live-activity/{id}`,
  `/link`, `/notify-json/{id}`) also take the token as a `token` query
  parameter, since Notify! senders put it there. A token in the URL ends up in
  access logs, proxy logs and browser history. Use the header when the client
  can set one. Other calls refuse the parameter; the server never logs query
  strings.
- `/ping/{slug}` takes a `write` token in the header, as any post does.
  `/p/{key}` takes a check's ping key instead (see "Ping keys"). Reading
  checks takes a `read` token; `PUT`, pause, resume and the ping key calls
  take a `write` token.
- The server stores each token's SHA-256 only. The README shows the token file.
- The Apprise compatibility paths and the hold paths take a bearer token as
  usual, or two other credentials ("Apprise compatibility", "Sender auth"): a
  token with the `notify-key` capability as the `{key}` of `/notify/{key}`,
  and, with no credential at all, a client address in
  `HERALDO_NOTIFY_TRUSTED_NETS`.
- A write token with the `trusted-ingest` capability is a bridge, such as
  apprise-tee. Its events get two extras:
  - `details.tee.hold`, an object, is copied into `hold`. Only an alert the
    hold kept back (`state: held` without `forwarded: true`) gets
    `store_only: true`; a store-only event is never pushed. Every other
    shape went out on arrival and pushes at its level: `state: bypass`,
    `state: held` with `forwarded: true` (shadow mode, or the dead-man
    tripped), and a later forward, `{forward, ids}` (release, deadline,
    dead-man and the dead-man notice). A `store_only` the bridge sends on
    those is dropped.
  - Its `dedup` id matches every earlier event, not only the last 24 hours.
    The tee sends its catalogue id as `dedup`.
- A `read` token with the `standby` capability is a standby's token on its
  primary. It may read the full push tokens and import events (see
  "Standby"). A plain `read` token sees token suffixes only.

Limits and errors:

- A `POST` body over 1 MiB gets 413. The `body` field may hold 256 KiB, counted
  in bytes.
- `content.steps.current` may not exceed `total`.
- An input that fails the schema gets 400 with the failing fields in `error`.

Paging:

- `next_cursor` comes with every page that has events. Pass it as `after`.
- Each event in a page, `GET /v1/events/{id}` and the answer to a post
  carries its own `cursor`: the `next_cursor` of a page that ends on it. A
  client marks one event read with one `PUT /v1/cursors/{name}`.
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

Routine events:

- A routine event is one the push table ("Push") never pushes by kind and
  action: a check-in that did not fail (`start`, `success` or `log`, with no
  non-zero `exit_code` outside `log`), and every `progress` and `status`
  event. A failing check-in, every message, digest and incident is not
  routine.
- `routine=exclude` leaves routine events out of the list. `routine=only`
  keeps only them. Without `routine` the list holds both, as before.
- The filter runs in the query, so `limit`, `next_cursor` and `before` stay
  correct. An index (`events_main`) keeps a newest-first list without routine
  events fast when check-in pings are most of the store.
- The answer carries a `Heraldo-Routine` header with the filter applied. A
  server before the filter ignores `routine` and sends no header, so a client
  can tell and filter on its own.
- The apps' Inbox, `heraldo ls` and `tail`, and the MCP `events_search` tool
  leave routine events out by default. A check's pings stay in its history:
  `GET /v1/events?key=<slug>` lists them.

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
- With push on, the server makes the `inbox` cursor at start-up when it is
  missing, at the newest event: the history from before push went on is
  read. The app makes its cursor the same way on first launch.
- Every answer carries `unread`: the events after the cursor that push an
  alert (see "Push"). It is the app's badge. Held, store-only and passive
  events (digests and incident resolves among them), incident acks,
  progress, status and passing check-ins do not count, nor does an event a
  mute or an inhibit rule held back.

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
| `live_activity_token` | hex, 32 to 400 characters | ActivityKit's push-to-start token for the app's Live Activities. |

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
`updated_at`, `live_activity_token_suffix` when the app reported one, and for
a retired device `retired_at` and `retired_reason` (`removed`, or the APNs
reason such as `apns: Unregistered`).

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
workers send it. A full queue (1024 events) drops the push and logs it. The
event is stored either way: the app syncs history by cursor.

- At SIGTERM or SIGINT the server stops taking requests, then pushes what is
  still queued. Both steps share 8 seconds, inside a container runtime's
  usual 10 second stop timeout. What is left then waits for the next start.
- At start, the server queues the events of the last hour that have no
  delivery row, such as those still queued when it last stopped. The push
  table decides each one as if it were new. Imported and store-only events
  are left out. A push APNs refused (`failed`) is not retried.
- `/healthz` reports `unsent` when push is on: the events received between
  24 hours and 15 minutes ago that should have pushed an alert and reached no
  device. APNs refused every device, no device is registered, or the push
  never ran. Point a monitor at it and alert above 0. It counts by the
  badge's rule ("Read cursors"), so held, store-only, passive and muted
  events do not count.

Which events push, and how loud:

| kind | action | Push | interruption-level |
|---|---|---|---|
| `message` | none | yes | the event's `level`, default `active` |
| `digest` | none | inbox only (`passive`) | |
| `incident` | `open`, `update`, none | yes | the event's `level` |
| `incident` | `resolve` | only to replace a visible open (`passive`) | |
| `incident` | `ack` | no | |
| `checkin` | `fail`, or any but `log` with a non-zero `exit_code` | yes | the event's `level` |
| `checkin` | `start`, `success`, `log` | no | |
| `progress` | any | no: Live Activities (below) | |
| `status` | any | no (widgets) | |

- Before the table: a held event (`hold.state` is `held` and
  `hold.forwarded` is not `true`), a store-only event
  (`hold.store_only`), and an event past `expires_at` never push.
- After the table: a mute ("Mutes") or an inhibit rule ("Inhibit rules") holds
  back an event that would push. `delivery` records `skipped` per device, with
  the reason in `error`, such as `muted by mute_01J...` or
  `inhibited by host-down: host:web-1 is open`.
- After the table: a `passive` event is inbox-only. It is stored and listed,
  but it shows no notification and does not count in the badge, like
  Pushover's priority -2. A quiet message is one with `level: passive`. An
  ingest rule can set it.
- One exception: a passive event with a `key` pushes when an earlier event
  of that key, since its last resolve, went out visibly (`sent` at a level
  other than `passive`). It carries the same `apns-collapse-id`, so it
  replaces that notification. An incident resolve ("Resolved after 12 min")
  is the usual case. A resolve of an incident that opened passive, or whose
  open a mute held back, pushes nothing.
- `critical` goes out as `time-sensitive`: the app has no critical-alerts
  entitlement. `delivery` records `level: time-sensitive` and the note
  `critical sent as time-sensitive` in `error`.
- A replacing passive push has no sound and `apns-priority` 5; the others
  have 10.

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
    "project": "web",
    "url": "https://example.com/status",
    "image": "https://example.com/graph.png",
    "icon": "uptime",
    "truncated": true
  }
}
```

| Payload | From |
|---|---|
| `alert.title` | `title`, else `source` |
| `alert.body` | `summary`, else `body`. A body is cut at a character boundary, with `…`, so the payload fits 4 KB. An incident resolve says `Resolved after 12 min` instead; a re-alert starts with `Still open after 30 min.`; a digest without a summary shows the first line of its body. |
| `badge` | `unread` of the `inbox` cursor when the push goes out; 0 when there is no `inbox` cursor |
| `sound` | `sound`, default `default`; none for `passive` or `sound: none` |
| `thread-id` | `thread`, else `source` |
| `category` | `incident` for an incident `open`, `update` or no action; `event` for the rest. The app registers both: `incident` has Ack, Mute 1 h and Open; `event` has Mute 1 h and Open. Mute 1 h on an incident mutes its key; on a check-in, its check's incident key `checkin:<key>`; on any other event, its source. |
| `mutable-content` | Always 1, so the notification service extension runs. |
| `interruption-level` | The table above. |
| `relevance-score` | `relevance` |
| `heraldo.icon` | The registry icon for the event: its `icon` when the registry has that name, else the icon named after its `source`. The extension fetches `GET /v1/icons/{name}` and attaches it as the thumbnail when the event has no `image`. |
| `heraldo.icon_url` | The event's `icon`, when it is an `https` URL. The extension fetches it without a token. |
| `heraldo.truncated` | The alert does not carry the full text: the body was cut, or the alert shows the summary of an event that also has a body. The extension fetches `GET /v1/events/{id}`. |

Headers: `apns-push-type: alert`; `apns-topic` from the device; `apns-collapse-id`
is the first 32 hex characters of the SHA-256 of `key`, when the event has one;
`apns-expiration` is `expires_at` when set, and absent otherwise, so APNs keeps
the push for an offline device.

## Live Activities

Kind `progress` drives Live Activities, on every active iOS device that
reported a push-to-start token. They go out only with push on. The sender
posts `start`, `update` and `end` on one key, with `content`
([LiveContent](#livecontent)).

Activities:

- A `start`, or any event on a key with no running activity, begins an
  activity. Its id is `act_` plus the event id. A `start` on a running key
  ends the old activity (`end_reason: restarted`) and begins a new one.
- An `update` merges its content. An `end` merges it and ends the activity
  (`end_reason: sender`). A second `end` changes nothing.
- Two hours without an event on the key, the server ends the activity
  (`end_reason: abandoned`).
- At its end, every activity writes one history event: a `passive` message on
  the activity's key, with the last title, the last status and how long it
  ran, such as `Done · ran 3h 20m`. `details.heraldo.activity` holds the id,
  the times, the end reason, the number of links and the last content.
- Every progress event is stored as usual. `GET /v1/activities/{key}` lists
  them since the start as `timeline`, at most 500; `GET /v1/events?key=` lists
  them all.

On each device an activity is a chain of instances. Each instance is one
ActivityKit activity, with its own id (`lai_` plus a ULID) in its attributes:

| state | Means | The server |
|---|---|---|
| `starting` | The push-to-start went out; no update token yet. | Waits for the token. |
| `active` | The app reported the update token. | Pushes updates to it. |
| `ended` | The server ended it, or the system did (the app reported `ended`). | After a system end, starts the next link while the activity runs. |
| `dismissed` | The person removed it (the app reported `dismissed`). | Never starts it again on that device. |
| `never-started` | No update token within 15 minutes, or APNs refused the push-to-start token. | Starts it again on the next update, three times at most per device. |
| `dead` | APNs refused its update token (410, `BadDeviceToken`, `ExpiredToken`). | Never starts it again on that device. |

- A refused push-to-start token is cleared from the device. A new one, sent
  with the next registration, starts every running activity on that device.
- An update token that comes after `never-started` revives the instance when
  it is still the newest on the device. Otherwise the server ends it.
- An update token for an instance the server ended while it was starting (an
  end, an abandon or a restart before the app reported) gets the end push at
  once, with `dismissal-date` now. The activity came up on the device anyway,
  and would stay there without it.

Pushes:

| Push | When | apns-priority | Notes |
|---|---|---|---|
| start | A new activity or a new link | 10 | To the push-to-start token. `alert` with the title and the body or status; the first start on a device sounds when the start's `level` is set and not `passive`. A chain link, a retry, and a start from a resync (a new push-to-start token, a server restart) have no sound. Expires after 15 minutes. |
| update, catch-up | The app reported a token | 10 | At once, to that instance. |
| update | Any other update, a new `status` or `tint` included | 5 | At most one per 15 seconds per activity. Updates in between merge; the last content goes out when the 15 seconds are up. A changing status text, such as a layer counter, cannot raise the rate or the priority. |
| end | The activity ended, a link was handed over, or a start replaced it | 10 | `dismissal-date` is now plus `keep_for` when set, now on a hand-off, else the system default. |

- Headers: `apns-push-type: liveactivity`, `apns-topic: <bundle id>.push-type.liveactivity`.
- `stale-date` is now plus `stale_after` when the content has it, else twice
  the gap between the sender's last two events (15 seconds to 1 hour; 10
  minutes before there are two). An update push expires at its stale date.
- Chain: Apple ends an activity after 8 hours. At 7.5 hours the server starts
  the next link with the same content, then ends the old one with
  `dismissal-date` now. A failed start leaves the old link up. Without a
  push-to-start token there is no next link.
- Retries: APNs gets one try per push; the server never sleeps on a retry.
  After a push-to-start fails with 429, 5xx or no answer, the server waits
  30 seconds before the next one to that device for that activity, doubling to
  10 minutes. A failed update goes again with the next update, or after 15
  seconds. A failed end goes again after 30 seconds, doubling, until 10
  minutes. A refused token (410, `BadDeviceToken`, `ExpiredToken`) is never
  retried.
- `timestamp` is the push time in Unix seconds, and always later than the
  last push to the same instance: an end in the same second as an update gets
  the next second, so the device does not drop it as old.
- Resync: on a restart, and when a device reports a new push-to-start token,
  the server pushes the current content of every running activity and starts
  it, silently, where it is missing. An activity with no event for two hours
  is abandoned instead of started.

The payload, a start:

```json
{
  "aps": {
    "timestamp": 1793091600,
    "event": "start",
    "content-state": {
      "title": "bracket.3mf", "symbol": "printer.fill", "tint": "blue",
      "progress": 0.42, "status": "Printing", "ends_at": 1793127600,
      "metrics": [{"label": "Nozzle", "value": "215", "unit": "°C"}]
    },
    "stale-date": 1793092800,
    "attributes-type": "ProgressAttributes",
    "attributes": {"key": "print/42", "instance": "lai_01JBA6Y8Q2K9M3T4V5W6X7Y8Z9", "source": "printer"},
    "alert": {"title": "bracket.3mf", "body": "Printing", "sound": "default"},
    "input-push-token": 1
  }
}
```

- `content-state` has the LiveContent fields except `stale_after` and
  `keep_for`. `ends_at` is Unix seconds, because ActivityKit decodes the state
  without the app's date format.
- `attributes-type` and the attribute names are the app's
  `ProgressAttributes`. Renaming either breaks push-to-start.

The app's side:

- It sends ActivityKit's push-to-start token as `live_activity_token` in the
  device registration, and again when it changes.
- For each activity, it sends the update token with
  `PUT /v1/activity-instances/{id}` and `{"device": "<id>", "token": "<hex>"}`,
  and later `{"state": "dismissed"}` or `{"state": "ended"}`. A report that
  fails goes again on the next foreground.
- `PUT` answers the instance. 400 for a bad body or another device's
  instance, 404 for an unknown id, 503 with push off. A report on an instance
  the server already closed changes nothing.
- A read token is not bound to a device, and `device` is optional. Any read
  token can report on any instance, and so can mark it dismissed or replace
  its token. Every token belongs to the owner, so this is an accepted limit.

`GET /v1/activities/{key}` answers an `Activity`: `key`, `id`, `state`
(`active` or `ended`), `source`, `content` (merged), `started_at`,
`updated_at`, `ended_at`, `end_reason`, `instances` (each with `id`,
`device`, `chain`, `state`, `end_reason` and times) and `timeline` (`id`,
`received_at`, `action`, `content` per event). `GET /v1/activities` lists the
same without `timeline`.

## Notify! compatibility

A sender written for Notify!'s Live Activity API, such as notify-bambuddy,
works with heraldo after two changes: the base URL, and a heraldo write token
as its device token. The device id can be any text. Each call becomes a
`progress` event from the token's name as `source`, and heraldo runs the
activity as above.

| Call | heraldo |
|---|---|
| `POST /live-activity/{deviceId}?token=&new=1` | `start` on a new key `notify-la-<ulid>`. Answers `{success, activityId, expiresAt}`; the activity id is the key. |
| `POST /live-activity/{activityId}?token=` | `update`. 410 when the activity ended, or every device's copy was dismissed or died. |
| `DELETE /live-activity/{activityId}?token=` | `end`, with the body's final content and `keepFor`. Ending an ended activity answers 200. 403 for an unknown id, as Notify! does. |
| `GET /live-activity/{activityId}?token=` | `{activityId, state, endReason, content, expiresAt}`. `state` is `starting`, `active`, `dismissed` or `ended`; `endReason` is `dismissed`, `script` (an end or a restart), or `abandoned`. |
| `GET /live-activity/{deviceId}?token=` | `{activities: [...]}`: the token's running compat activities. |
| `GET /link?id=&token=` | `{success: true, type: "device", id, name: "heraldo"}` for a write token. |
| `POST /notify-json/{deviceId}?token=` | A `message`: `text` is the body, `title`, `groupType` the thread, `iconUrl` the icon, `timeSensitive` the level, `project` the project. |

Field mapping, Notify! to LiveContent: `progress` 0–100 to 0–1; `steps` and
`step` to `steps {total, current}`; `endsIn` seconds to `ends_at`, from now;
`tint` hex with or without `#`, alpha first in 8 digits, to `#RRGGBBAA`;
`button {title, url, open}` to an `open` action when `open` is true, else
`http`; `keepFor` to `keep_for`; `title`, `body`, `symbol`, `trailing`,
`status`, `metrics` as they are. null clears a field. heraldo also reads
`project` on a start; an update or an end keeps the activity's project.

- `expiresAt` is ten years out. heraldo hands a long activity over to a new
  one itself, so the sender must not roll it over.
- A missing or unknown token answers 401, not Notify!'s 403.

## Apprise compatibility

A sender written for apprise-api, or for an apprise-tee in front of it, works
with heraldo after one change: its URL. heraldo answers in Apprise's shape, so
a sender that checks for HTTP 200 and a `Sent` line keeps working.

| Call | heraldo |
|---|---|
| `POST /notify/{key}` | Apprise's keyed notify. `key` is `apprise` (Apprise's default key) or a `notify-key` token. |
| `POST /v1/notify` | The same message, `body` required. The answer adds `id` (the event), `delivered` (false when held), `backends` and `hold_id`. |
| `GET /recent?n=50&tag=&source=` | The newest `n` events (1 to 1000), newest last, in the tee catalogue's shape. |
| `GET /entry/{id}` | One event in that shape, by event id. 404 for an unknown id. |

The body is a JSON object, a form (`application/x-www-form-urlencoded`) or
multipart (files are dropped). Its fields:

| Field | Becomes |
|---|---|
| `title`, `body` | `title` (256 characters) and `body` (256 KiB). `/notify/{key}` takes a title alone; an empty post gets 400. |
| `tag`, else `tags` | `tags`, split on commas and spaces; a JSON list works too. The first tag, the rung, picks the level (below). |
| `level` | A rung that replaces `tag`. |
| `source` | `source`. Absent: a title prefix `Name/host ...` or `name: `, else the token's name, else the client address. |
| `project` | `project`, folded to lower case. A name the envelope refuses is dropped; the message is stored. |
| `url`, `image` | `url` (`http` or `https`) and `image` (`https`). A value that is not a web URL is dropped. |
| `type`, `format` | `details.notify.type` and `.format`. |

`details.notify` also records `client` (the connection's address) and `path`
(`/notify/apprise`, `/notify/<key>` or `/v1/notify`).

| Rung | Level |
|---|---|
| `ops` | `critical` (sent as `time-sensitive`) |
| `page`, `alert` | `time-sensitive` |
| `general` | `active` |
| `quiet` | `passive` |
| an envelope level, such as `passive` | that level |
| anything else | none (`active` when pushed) |

The post is untyped, so the ingest rules type it, as they typed the tee's
copies. Answers:

- **200** with `{"error": null, "details": [["INFO", "<time>", "Sent via heraldo as <event id>."]]}`.
  A held alert answers 200 with `Held for triage as h-...; Sent to the hold
  queue, not yet pushed.` It carries `Sent` on purpose: a sender with its own
  Pushover fallback treats a missing `Sent` as a failure, and would page
  around the hold.
- **424** with `"error": "One or more notification could not be sent."` when
  heraldo did not store the event. No detail line says `Sent`.
- **400** for a body that does not parse or has no text. **401** and **403**
  as for any call.

"Sent" means stored, as it did for the tee's heraldo backend: an event with no
registered device, or push off, still answers `Sent`.

`/recent` entries carry `id`, `ts` (epoch seconds), `time`, `client`, `path`,
`tag` (the tags joined with commas, `null` untagged), `title`, `body`,
`source`, `status` (200), `delivered` (false for a held or store-only event),
`kind`, `key`, `level`, `url`, `image`, and `hold`, `hold_id`, `hold_reason`,
`hold_ids` for hold traffic. `tag=none` keeps untagged events; `source`
matches exactly or by the part before `/`, ignoring case. A filtered call reads
at most the newest 5000 events. An event imported from the tee shows the
tee's `client` and `path`.

### Sender auth

Apprise senders post with no header, so these paths accept three credentials,
tried in order:

1. A bearer token, as on any call: `write` to post or close hold entries,
   `read` to read `/recent`, `/entry` and the hold.
2. For `/notify/{key}` only, a token with the `notify-key` capability as
   `{key}`. Give each sender its own, with `write` scope. A key in a URL ends up
   in the sender's config and in proxy logs; heraldo logs the path as
   `/notify/<key>`.
3. With no credential at all, a connection from a network in
   `HERALDO_NOTIFY_TRUSTED_NETS`. It acts with the scope the call needs, under
   the name `trusted-net`, on every compatibility and hold path, closing hold
   entries included.

The trusted network is a convenience with a cost. Anyone who reaches the port
from that network can post, read recent events and close held alerts. heraldo
reads the connection's address, never a forwarding header, so behind a NAT or
a container port proxy every client may arrive from the proxy's address, and
trusting that address trusts all of them. Keep the list empty unless the
network is yours, and prefer keys.

`heraldo send` (the CLI) works against `/notify/{key}`: point its notify URL
there.

## Hold

The hold keeps alert-rung posts from paging until triage has read them, so
the owner gets one diagnosis instead of a push per symptom. It replaces the
apprise-tee hold and keeps its contract.

A post is a candidate when it arrives on the Apprise compatibility ingest with
the rung (`level`, else `tag`) exactly `alert`, ignoring case. Other rungs,
tag lists and `POST /v1/events` are never held.

| `HERALDO_HOLD` | A candidate that matches no bypass rule |
|---|---|
| `off` (default) | Stored and pushed as usual. Nothing is recorded. |
| `shadow` | Stored and pushed, and recorded as an entry with `forwarded` and `shadow` true. |
| `on` | Stored with `hold` `{id, state: "held", forwarded: false}`, so it does not push, and recorded with `forwarded` false. The sender gets 200 with `Sent` (above). |

| Variable | Default | Meaning |
|---|---|---|
| `HERALDO_HOLD_DEADLINE` | `30m` | An entry still `held` this long is forwarded as it is, title prefixed `[not triaged in 30 min]`, and marked `expired`. |
| `HERALDO_HOLD_DEADMAN` | `20m` | When nothing polls the queue this long, the dead-man trips (below). Before the first poll, the server's start counts. |
| `HERALDO_HOLD_BYPASS_FILE` | none | The bypass list. |

### Bypass list

A JSON list of rules; a rule matches when every key it sets matches: `tag`
(equal, ignoring case), `title_regex` and `body_regex` (Go RE2, anywhere in
the text). `why` is a comment. The file format is the tee's
`hold-bypass.json`, so that file moves as it is. A bypassed post pushes at
once and is stored with `hold` `{state: "bypass"}`. Example:
`deploy/hold-bypass.example.json`.

- The server checks the file's modification time on each candidate and every
  30 seconds, and reloads it when it changed.
- A file that does not parse keeps the rules in force and logs an error, so a
  typo cannot start holding what the list exists to let through. A broken
  file at startup stops the server.
- A missing file means no rules, and logs a warning.

### Endpoints

```sh
# The queue: open entries, plus entries closed in the last 24 h. Every GET
# without peek=1 is a triage poll and resets the dead-man.
curl -H "Authorization: Bearer $READ_TOKEN" "$HERALDO_URL/v1/hold"    # ?state=pending: open only
# A look that is not a poll. A person or a CLI must use it, or it would hold
# off the dead-man while triage is dead.
curl -H "Authorization: Bearer $READ_TOKEN" "$HERALDO_URL/v1/hold?peek=1"
# Close a group with one diagnosis. In on, this stores one message at tag.
curl -H "Authorization: Bearer $WRITE_TOKEN" -X POST "$HERALDO_URL/v1/hold/release" \
  -d '{"ids":["h-..."],"verdict":"REAL","title":"...","body":"...","tag":"general"}'
# Close a group and send nothing.
curl -H "Authorization: Bearer $WRITE_TOKEN" -X POST "$HERALDO_URL/v1/hold/suppress" \
  -d '{"ids":["h-..."],"reason":"BLIP: recovered before triage looked"}'
```

`/held`, `/held/release` and `/held/suppress` are the same calls under the
tee's paths, so a triage job switches by changing its base URL.

The queue answers `mode`, `last_poll_ts`, `oldest_unforwarded_ts` (null when
no open or expired entry is waiting to push; a freshness check should age
this), `deadman_active`, and `held`: entries with `id`, `event_id`, `ts`,
`deadline_ts`, `tag`, `title`, `body`, `client`, `source`, `forwarded`,
`shadow`, `status` (`held`, `released`, `suppressed`, `expired`), and once
closed `closed_ts`, `verdict` and `reason`. Times are epoch seconds, as the
tee gave them.

- A release takes `ids`, `verdict` (`REAL`, `BLIP`, `MAINTENANCE`, `FALSE`,
  any case), `title`, `body` and `tag`; otherwise 400. In `on` it stores one
  untyped message from source `triage`, with `hold` `{state: "forward",
  reason: "release", ids, verdict}`, and answers `sent: true` and its
  `event_id`. In `shadow` it only records. When the message cannot be stored,
  the answer is 500 and nothing is closed, so the caller can send it again.
- A suppress takes `ids` and an optional `reason`.
- Both close only open entries. A list with none answers 409; a mixed list
  closes the open ones and lists the rest under `unknown_or_closed`.

### Dead-man and deadline

A loop every 30 seconds:

1. **Deadline.** An entry still `held` past its deadline is forwarded and
   marked `expired`. A shadow entry already pushed, so it is only marked.
2. **Dead-man.** When the dead-man time passes with no poll, the server logs
   `hold: DEAD-MAN TRIPPED`, forwards every entry not yet forwarded, title
   prefixed `[triage not polling]`, and lets new candidates through (stored
   with `hold` `{id, state: "passed"}` and recorded) until the next poll, when
   it logs `hold: dead-man cleared`. In `on` it also stores one `general`
   notice from source `heraldo-hold`, "alert hold off: triage not polling".
3. **Retry.** A forward that cannot be stored is tried again on the next
   tick.

A forward is the original post typed again by the ingest rules, with the
prefix added after typing, so a rule anchored on the title still matches. It
carries `hold` `{state: "forward", reason: "deadline" | "deadman", ids}`. An
entry left from an `on` period is forwarded whatever the current mode, so
switching `on` to `shadow` or `off` never strands an alert.

- The stored event never changes. The entry is the live state: an event held
  on arrival keeps `hold.state` `held` after its release, and
  `?hold=held` on `/v1/events` lists what was held, not what is open.
- A held incident event still opens or updates its incident; the hold only
  stops its push. A held event never re-alerts.
- A rule with `hold: true` ("Ingest rules") gives the event its own
  store-only hold, which wins over every hold state but `held`.

## Incidents

An incident is the events that share a `key` with kind `incident`. The
server keeps one row per key and stores a snapshot of it on each incident
event, in the server field `incident`.

| Event | Incident after it |
|---|---|
| `open`, `update`, no action | A new or resolved incident opens at this event. An acked one stays acked; an open one stays open. |
| `ack` | An open incident is acked. A resolved one stays resolved. |
| `resolve` | The incident resolves at this event. A resolved one keeps its first resolve. A resolve with no open before it opens and resolves at once. |

- `GET /v1/state` lists the incidents that are not resolved, each as its
  latest event; its `incident.state` says open or acked.
- A resolve pushes `Resolved after 12 min` (from `incident.opened_at` and
  `resolved_at`), passive, with the same `apns-collapse-id` as the open. The
  phone replaces the incident's notification with it, so a failure and its
  recovery show as one notification. When the open never showed (it was
  passive, or held back), the resolve is inbox-only.

Re-alerts:

- An incident `open`, `update` or untyped event at level `time-sensitive` or
  `critical`, with `details.heraldo.realert_s` of 60 or more, schedules a
  re-alert. An ingest rule sets `realert_s` with `set.realert` (see "Ingest
  rules").
- A key that starts with `checkin:` never re-alerts: check-in incidents send
  their own reminders (see "Incidents and reminders").
- While the incident is open, the server pushes that event again every
  `realert_s` seconds, with `Still open after 30 min.` before the text and the
  same collapse id. No event is stored; the re-alert's outcome replaces the
  event's `delivery`.
- An ack or a resolve ends the re-alerts. A later open or update with an
  interval starts them again; one without an interval keeps the schedule.
- The server checks every 30 seconds. After downtime it sends one re-alert,
  not one per missed interval. Only a server with push on sends re-alerts.
- A mute or an inhibit rule holds a re-alert back, and the schedule moves on.
- An imported event (`import-tee`) never schedules a re-alert: it is history.

## Mutes

A mute holds back pushes for a while. Matching events are stored, listed and
synced as usual; only the push is skipped. The app's Mute 1 h action makes one.

`POST /v1/mutes` takes a write token and a `MuteInput`:

| Field | Meaning |
|---|---|
| `source` | Mute events from this source. 128 bytes. |
| `key` | Mute events with this key, such as one incident. 255 bytes. |
| `until` | When the mute ends, RFC 3339. |
| `for` | How long the mute lasts, a Go duration such as `1h` or `90m`, instead of `until`. |

- Give `source`, `key` or both. With both, an event must match both.
- Give `until` or `for`, not both. The end is in the future, at most 366 days
  ahead. Otherwise 400.
- The answer, 201, is a `Mute`: `id` (`mute_` plus a ULID), `source`, `key`,
  `until`, `created_at`, `ended_at` and `active`.
- `GET /v1/mutes` takes a write token and lists the mutes in force, soonest
  to end first.
- `DELETE /v1/mutes/{id}` takes a write token and ends the mute now. A mute
  that already ended is returned as it is. 404 for an unknown id.
- A mute on `checkin:<slug>` covers the check's incident and the check's own
  check-in events (kind `checkin`, key `<slug>`). So Mute 1 h on a failed run
  or on the check's down incident silences that one check, both ways, and no
  other.
- Mutes hold back pushes and re-alerts, not storage, the event stream, or the
  read cursors.

## Icons

The icon registry maps a name to an image. An admin token writes it; there
are no open uploads.

- `PUT /v1/icons/{name}` with the image as the body stores or replaces the
  icon: 201 new, 200 replaced. PNG or JPEG, up to 256 KiB; the server checks
  the bytes, not the header. A square image of 256 by 256 pixels or more shows
  best. `name` is 1 to 128 of `A-Z a-z 0-9 . _ -`, starting with a letter or
  digit. Other types get 415.
- `GET /v1/icons/{name}` (read) returns the image. Its `ETag` is the image's
  SHA-256; `If-None-Match` answers 304.
- `GET /v1/icons` (read) lists `{name, content_type, bytes, sha256, updated_at}`.
- `DELETE /v1/icons/{name}` (admin) removes it: 204.
- An event names its icon in `icon`. Without one, an icon named after the
  event's `source` is used. The push carries the name in `heraldo.icon`.

```sh
curl -X PUT --data-binary @backup.png -H "Authorization: Bearer $ADMIN_TOKEN" \
  "$HERALDO_URL/v1/icons/backup"
```

## Standby

A standby is a second heraldo server on another host. It delivers pushes
while the primary's host is down. It runs the same binary with
`HERALDO_ROLE=standby` and its own copy of the APNs key.

| Variable | Default | Meaning |
|---|---|---|
| `HERALDO_ROLE` | `primary` | `standby` runs the server as a standby. |
| `HERALDO_PRIMARY_URL` | none | The primary's root. Needed on a standby. |
| `HERALDO_PRIMARY_TOKEN` | none | The standby's token on the primary: a `read` token with the `standby` capability. Needed on a standby. |
| `HERALDO_STANDBY_INTERVAL` | `1m` | The gap between rounds. |
| `HERALDO_PRIMARY_DOWN_AFTER` | `10m` | How long the primary may go unanswered before the alert. |

Each round, the standby calls the primary. Every call goes from the standby
to the primary; the primary never calls the standby.

1. `GET /v1/standby/devices`. The standby replaces its device table with the
   answer, retired devices included. Apps register with the primary only.
2. If the alert is open, the standby resolves it (see below).
3. `POST /v1/standby/import` with every event the standby stored since the
   last import, oldest first, 100 per call. The standby remembers the last
   imported event in its database, so a restart sends nothing twice.

The standby accepts `POST /v1/events`, `GET /v1/send` and the pings from its
own `write` tokens, types them with its own `HERALDO_RULES`, stores them and
pushes them. It judges no checks: the primary owns them.

The import is idempotent:

- The primary stores each event under the standby's id and records the import.
  An id imported before is skipped.
- A `dedup` id stored before, at any age, is skipped too. A sender that posted
  to both servers is stored once.
- Imported events are history. The primary never pushes them and never
  re-alerts them. Incidents and check-ins derive from them as from any event.
- An event that is not valid on the primary is logged there and counted as
  `rejected`. It does not block the events after it.

Primary down:

- A round that gets no answer, or a 5xx from a proxy, counts as down. A 401
  or 403 counts as up: the primary answered. The standby logs the refusal on
  every round.
- When the primary has not answered for `HERALDO_PRIMARY_DOWN_AFTER`, the
  standby stores and pushes one incident: source `heraldo`, key
  `heraldo:primary`, level `time-sensitive`, title
  `heraldo primary unreachable`.
- The standby raises it once per outage, across restarts. The first round the
  primary answers resolves it (level `passive`), then hands both events back.
- A standby that starts counts from its start: it raises nothing until the
  primary has been silent for the full delay.

Senders reach the standby when the primary fails. `heraldo send` does this
with `HERALDO_FALLBACK_URL` and `HERALDO_FALLBACK_TOKEN` (README, "send").
A script can post to the standby's `POST /v1/events` itself.

## Input and stored events

- **`EventInput`** is what a sender posts. It is the schema root.
- **`Event`** is what the read API returns: `$defs/Event` in the schema.
- The server owns `id`, `received_at`, `cursor`, `hold`, `delivery` and `incident`. An
  input that sets any of them is rejected. `ts` stays the sender's; the server fills it with the
  arrival time when it is missing.

## Check-ins

A check is a job that pings heraldo when it runs. When the pings stop, or a
run fails or takes too long, the check goes down and an incident opens. A
check's slug is the key of its check-in events: `/ping/nightly-db` and a
`POST /v1/events` with `kind: checkin, key: nightly-db` feed the same check.
A slug is 1 to 200 of `A-Z a-z 0-9 . _ : -`.

### Pings

The pings follow healthchecks.io. `GET` and `POST` both work; a `POST` body is
the run's log.

| Path | Records |
|---|---|
| `/ping/{slug}` | A successful run |
| `/ping/{slug}/start` | The start of a run |
| `/ping/{slug}/fail` | A failed run |
| `/ping/{slug}/log` | A log; the check's state does not change |
| `/ping/{slug}/{exit_code}` | A run by exit code, 0 to 255: 0 is success, the rest fail |

| Parameter | Meaning |
|---|---|
| `rid` | Run id, up to 64 characters. Pairs a start with its end, so runs that overlap are measured apart. |
| `create=1` | Creates the check when the slug is unknown. Without it, an unknown slug gets 404. |
| `period`, `cron`, `tz`, `grace`, `max_runtime`, `reminder` | The schedule of a check that `create=1` makes. A duration is seconds or a Go duration such as `90m`. Ignored for a known check. |
| `source`, `host` | The event's `source` (default `checkin`) and `host`. |
| `project` | The event's `project`. Without it, the event takes the check's project (see "Projects"). |

`/p/{key}` takes the same paths after the key, and the same parameters but
`create` (400): a ping key pings a known check only.

```sh
# Once, with a write token: create the check and get its ping URL.
heraldo checkins create nightly-db --cron '0 3 * * *' --tz Europe/Madrid --grace 1h
heraldo checkins key nightly-db   # prints https://heraldo.example.net/p/<key>

# In the job. PING_URL is that URL, kept in the job's env file.
curl -fsS -m 10 "$PING_URL/start?rid=$$"
out=$(run-backup 2>&1); code=$?
printf '%s' "$out" | curl -fsS -m 10 --data-binary @- "$PING_URL/$code?rid=$$"
```

- Each ping stores one event: `kind: checkin`, `key` the slug, `action`
  `start`, `success`, `fail` or `log`, `checkin.rid` and `checkin.exit_code`
  when given. A log is the event's `body`.
- The server keeps the first 100 kB of a body, cut at a character boundary.
  A cut log has `details.heraldo.log_truncated: true`.
- A failed run's event has the title `<slug> failed` or
  `<slug> failed (exit <code>)`, and pushes as the push table says.
- The answer is `200` with `OK`.

### Ping keys

A ping key lets a job ping one check with no bearer token. It is for the
URL that a cron line, a systemd unit or a device webhook carries, where a
token would leak into shell history, process lists and logs.

- `POST /v1/checkins/{slug}/key` (write token) mints a key and answers
  `{"key": "...", "path": "/p/<key>"}` with 201. A key is 24 random bytes,
  base64url: 32 characters.
- The server stores only the key's SHA-256, as it does for tokens. The
  answer is the only time it shows the key. `Check.ping_key_at` says when
  the key was minted.
- Minting again replaces the key; the old one gets 404 at once. That is the
  rotation. `DELETE /v1/checkins/{slug}/key` removes the key.
- A key does one thing: ping its own check. It cannot create a check, read
  anything, or post other events. A leaked cron line can fake runs of that
  one check, and nothing else. Rotate the key to end that.
- An unknown or malformed key gets 404, the same answer, so a probe learns
  nothing.
- The request log shows the path as `/p/<key>/...` and the caller as
  `ping-key:<slug>`, never the key.
- Any write token may mint a key for any check. That grants nothing new: a
  write token can already ping any check through `/ping/{slug}`.
- A check has no key until one is minted. `/ping/{slug}` with a write token
  in the header always works.

Why a key per check, and not the write token in the URL: a write token posts
any event and pings every check, so one leaked cron line would expose all of
them. A per-check key bounds the damage to one check, needs no change to the
token list, and rotates with one call.

### Schedule

| Field | Meaning | Default |
|---|---|---|
| `period_s` | Seconds between pings | |
| `cron`, `tz` | A five-field cron expression and its IANA time zone. Not with `period_s`. | `tz` UTC |
| `grace_s` | How late a ping may be before the check goes down | 3600 |
| `max_runtime_s` | How long a started run may take | none |
| `reminder_s` | Seconds between reminders while the check's incident is open and not acked; 0 sends none | 86400 |

A check with neither `period_s` nor `cron` is unscheduled: time never takes
it down, only a failed or overrun run does. A check that a check-in event
creates takes the event's `checkin.schedule`, or none. After that, the
schedule lives on the server: `PUT /v1/checkins/{slug}` changes it.

### States

| State | Means | Next |
|---|---|---|
| `new` | Known; no run yet. | Any ping. A scheduled check turns `late` at its first slot after it was created or resumed, and `down` (`missed`) at `down_after`, that slot plus `grace_s` |
| `up` | The last run succeeded. | `late` at `next_due` |
| `started` | A run started and has not ended. | `down` (`overran`) at `started_at` plus `max_runtime_s`. Without `max_runtime_s`: `down` (`missed`) at the next scheduled time after `started_at`, plus `grace_s`. Never `late`. |
| `late` | The ping is due; the grace has not run out. | `down` at `down_after` (`next_due` plus `grace_s`) |
| `down` | A run failed (`failed`), ran too long (`overran`), or no ping came (`missed`). `down_reason` says which. A `/start` keeps it `down`, with its `state_since`, and records `started_at`. | `up` on the next success |
| `paused` | Not judged. Pings are still stored and update the times. | `new` on resume |

- `next_due` is the time after the last ended run: plus `period_s`, or the
  next cron time. A failed run counts as a run, so a failing job does not
  also go late.
- A started run is judged on its own start, not on the `next_due` of the
  run before. A nightly job that runs for two hours is not missed while it
  runs. `down_after` shows the started run's deadline.
- On a fall-back night the clock reads one hour twice. A cron slot in that
  hour counts once, at its first reading, as cron runs it: a ping at the
  first 01:31 is due at 01:30 the next day, not an hour later. In a
  spring-forward gap the slot does not exist that day; the next due time is
  the slot on the day after.
- A run's length is the time from its start to its end: with `rid`, the
  start with the same `rid`. A sender's `checkin.duration_ms` takes
  precedence.
- The scheduler judges the checks every 15 seconds. All its state is in the
  database, so a restart picks up where it left off.
- A scheduled check that never pings goes down like one that stopped. Its
  incident reads "`<slug>` never pinged".
- After the server starts, a check goes down for a missed ping only once
  its grace has passed since the start. Pings sent while the server was down
  were lost; this keeps them from raising false alarms.

### Incidents and reminders

A check's incident has the key `checkin:<slug>` and the source `heraldo`.

| When | Event | Level |
|---|---|---|
| The check goes down for `missed` or `overran` | incident `open` | `active` |
| A scheduled check goes down for `failed` | incident `open` | `passive`: the failed run's event already pushed |
| The incident is open, not acked, and `reminder_s` passed since it opened or last reminded | incident `update`, "`<slug>` is still down" | `active` |
| The check is up again, or paused | incident `resolve` | `passive` |

- An unscheduled check's failed run opens no incident. Its event is the
  alert; nothing would resolve an incident. Rule-typed failure posts, such as
  systemd `OnFailure` messages, are such checks.
- `late` raises nothing. The app and CLI show it.
- A ping to a paused check is stored; a failed run's event is `passive`.
- Ping rules ("Ingest rules") can set a run's `level` or hold it, such as
  holding one check's failures for triage.
  Pausing resolves an open incident.
- An acked incident gets no reminders. It still resolves on the next success.

### Jobs on a Mac that sleeps

The server judges arrival times only. It cannot tell a sleeping laptop from
a dead job, so the sender owns sleep:

- launchd runs a `StartCalendarInterval` job once at wake when its time
  passed during sleep. The ping comes late, not never.
- Set `grace_s` to the longest sleep that should raise nothing. A daily job
  on a laptop that sleeps overnight and on weekends: `period_s` 86400,
  `grace_s` 259200 (three days).
- Use `period_s` rather than `cron` for such a job. With `period_s`, the next
  due time counts from the run that came at wake. With `cron`, it stays on the
  clock, so a run that came hours late is due again sooner.
- Pause the check before a long trip, and resume it after: `heraldo checkins
  pause <slug>`.
- Send `/start` only from the job itself. A run cut off by sleep then shows as
  `started`; set `max_runtime_s` above the job's longest run plus a sleep, or
  leave it out.

### Checks API

`GET /v1/checkins` answers `{"checkins": [Check]}` in key order.
`GET /v1/checkins/{slug}` answers one `Check`, or 404.

`Check`: `key`, `host`, `state`, `state_since`, `down_reason`, `schedule`
(the table above, with defaults filled in), `created_at`, `last_ping`,
`last_event` (the id of the last check-in event), `started_at` (the run in
progress), `last_success`, `last_duration_ms`, `next_due`,
`down_after` (when the check goes down without a ping: the grace included,
for `up`, `late` and `started`), `last_log` (the body of the last ping that had one), `last_log_at`,
`ping_key_at` (when the ping key was minted; never the key), `incident` (`open`, `acked` or `resolved`, when the check ever had one), and `project` (the project of its last ping that named one).

`PUT /v1/checkins/{slug}` takes a `CheckConfig`: `host` and the schedule
fields. Every field is replaced; a field left out takes its default. It
answers 201 for a new check and 200 for a known one. A changed schedule moves
`next_due` from the last run. A check that is `late`, or `down` for `missed`,
is judged again under the new schedule.

`POST /v1/checkins/{slug}/pause` and `/resume` answer the check, or 404.

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
| `project` | name, 64 | The project the event belongs to. See "Projects". |
| `key` | string, 255 | Groups related events. |
| `title` | string, 256 | Notification title. |
| `summary` | string, 178 | The one-line push text. A digest pushes only this. |
| `body` | markdown, 256 KiB | Stored in full; a push carries the summary or a truncated copy. |
| `level` | `passive`, `active`, `time-sensitive`, `critical` | Interruption level. Default `active`. `passive` is inbox-only (see "Push"). |
| `thread` | string, 128 | Notification thread. Defaults to the source. |
| `url`, `image` | URI | Tap target and attached image. |
| `icon` | string, 512 | Source icon: an `https` URL, or a name in the icon registry. |
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
that, the check's config lives on the server. See "Check-ins".

### Server fields

| Field | Meaning |
|---|---|
| `id` | Event id: a ULID (26 characters, sortable by time). Live events get the arrival time. |
| `received_at` | When the server stored the event. |
| `cursor` | The event's list cursor (see "Paging"). |
| `hold` | Triage state. |
| `delivery` | Per-device outcome: `sent`, `failed`, or `skipped` (a mute or an inhibit rule held it back), with the level the push carried. See "Push". |
| `incident` | On an incident event: the incident after this event, `{state, opened_at, resolved_at}`. `state` is `open`, `acked` or `resolved`. See "Incidents". |

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

`heraldo-server import-tee [-since YYYY-MM] <dir>` reads every
`notifications-YYYY-MM.jsonl` file in `dir`, or with `-since` that month's
file and later ones, and stores each line as an untyped event. An imported
event never pushes and never schedules a re-alert. When `HERALDO_RULES` is set,
the rules type each event as they would a live post; without rules, or when
no rule matches, it is a `message`. The summary counts
those as `typed`. A rerun skips lines it already stored, so it never retypes
them.

| Catalogue field | Event field |
|---|---|
| `id` | `details.tee.id` and `dedup`. The event id is a ULID made from `ts` and this id, so a rerun makes the same id. A catalogue id that is already a ULID is kept. A line without an `id` gets a legacy id, and the import uses it in the same places. |
| `ts` | `ts` and `received_at` |
| `source` | `source`, or `apprise-tee` when the entry has none |
| `project` | `project`, folded to lower case; dropped when it is not a valid name. The projects map runs after the rules. |
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
sender's other fields as sent. Ping rules (below) apply to the check-in
events of `/ping` and `/p`.

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
  `title`, `body`, `key`, `action`, `kind` and `dedup`. Every field a rule names must match. `tag` matches when
  any one tag does, and `^$` matches an untagged event. A missing field reads
  as the empty string. A rule with no `match` matches everything.
- A capture of `dedup` gives each post its own key, such as
  `key: 'page:{id}'` with `dedup: '^(?P<id>.+)$'`. Each post is then its own
  incident: an ack stops its re-alerts, and the next post alerts again.
- Named groups, `(?P<name>...)`, are captures. `{name}` in `key`, `host`,
  `thread` or `project` is replaced with the captured text. A placeholder
  that names no capture is an error when the file loads.
- `set` takes `kind` (required: `message`, `digest`, `incident` or
  `checkin`), `key`, `action`, `level`, `thread`, `host` and `project`.
  `incident` and `checkin` need a `key`. A field the rule leaves out keeps
  the sender's value. A value longer than the envelope allows is cut. A
  sender's `project` wins over the rule's. A `project` that is not a valid
  name once filled in sets nothing.
- An `incident` rule may also set `realert`, a Go duration of a minute or
  more, such as `15m`. The event stores it as `details.heraldo.realert_s`. It
  re-alerts only a `time-sensitive` or `critical` incident (see "Incidents").
- Any rule may set `hold: true`. The event is stored with `hold`
  `{state: held, reason: "rule <name>", store_only: true}`, so it never
  pushes and lists under `?hold=held`.
- A rule whose `match` names `kind` is a ping rule. It applies only to the
  check-in event a ping makes (`/ping/{slug}` and `/p/{key}`), never to an
  untyped input, and no other rule sees ping events. It keeps the event's
  `kind`, `key` and `action`: its `set` takes `level`, `thread`, `host`,
  `project` and `hold` only. A held failed run still moves the check to `down` and opens
  its incident; the hold stops the run's own push.
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

### Inhibit rules

An `inhibit` list beside `rules` holds back the pushes of child incidents
while a parent incident is open, such as a host that is down muting its
units and containers.

```yaml
inhibit:
  - name: host-down
    parent: '^host:(?P<host>[\w.-]+)$'
    children: '^(unit|container):{host}:'
```

- `parent` is a regular expression on an incident key. `children` is one
  too; `{name}` in it is replaced with the parent's capture, quoted, so a dot
  in a host name matches a dot only.
- While an incident whose key matches `parent` is open or acked, an event
  whose key matches `children` is stored but not pushed. Its `delivery` says
  `skipped` and names the rule and the parent.
- The parent's own events are never held back by its own rule.
- A resolve of the parent ends the hold. A child still open then re-alerts
  on its schedule, if it has one.
- A placeholder that names no capture in `parent` is an error when the file
  loads.

`heraldo-server replay-tee -rules <file> <dir>` runs a catalogue through a
rules file in memory, with no database, and prints:

- the recoveries (incident resolves) that paired with an open incident under
  their key, out of all recoveries, and each unpaired one;
- the incidents still open at the end;
- the hits per rule;
- every source and first tag that no rule matched, with a count and a title;
- the events per project, and every source and first tag left without a
  project. The replay runs the projects map but not key continuity, so a
  keyed event the live server would file by its key can list here.

## Projects

An event may name the `project` it belongs to, such as `web` or `fleet`. A
project is a label: the server keeps no list of projects and accepts any
valid name. An event with no project is "no project".

- A name is 1 to 64 of `a-z`, `0-9`, `.`, `_` and `-`, and starts with a
  letter or a digit. The server folds upper case to lower case and trims
  spaces before it checks the name, on every path. Any other bad name is a
  400 on `POST /v1/events`, `GET /v1/send` and the pings. The Apprise paths,
  `heraldo send` and `import-tee` drop a bad name and keep the message.
- The project comes from, first to last: the sender; a typing rule's
  `set.project`; the `projects` map; the project of the event's key.

### The projects map

`projects`, beside `rules` in the rules file, files the events that have no
project yet. It runs on every input after the rules typed it: untyped and
typed posts, the Apprise and Notify! paths, pings, the server's own events
and `import-tee`. The first entry that matches wins.

```yaml
projects:
  - match: {source: '^printer-'}
    project: printing
  - name: fleet-threads
    match: {thread: '^(units|backups)$'}
    project: fleet
  - match: {source: '^team-(?P<team>[a-z]+)$'}
    project: '{team}'
```

- `match` takes the fields a rule matches, plus `thread`, so an entry can
  match the thread a rule just set. An entry with no `match` matches
  everything.
- `project` takes `{name}` placeholders. A result that is not a valid name
  sets nothing.
- `name` is optional. A name used twice, or a static `project` that is not a
  valid name, is an error when the file loads.

### Key continuity

An `incident`, `checkin`, `progress` or `status` event with no project takes
the project of its key's row: the project of that key's last event that
named one. An ack or a resolve from `heraldo ack`, a progress update, or a
ping with no `project` so lands beside the event that opened the key. A
check-in incident, `checkin:<slug>`, with no project takes its check's
project.

### Aliases

```yaml
aliases:
  infra: fleet
```

`aliases` maps an old project name to its new one. Several old names may map
to one new name (a merge). A name may not map to an alias.

- On ingest, an event that names `infra` is stored as `fleet`.
- On read, `project=fleet` also matches the events stored as `infra`, and
  `project=infra` reads as `project=fleet`.
- `GET /v1/projects` counts `infra`'s events under `fleet`.
- The server never rewrites a stored event's project.

### Filters

`GET /v1/events`, `GET /v1/state` and `GET /v1/checkins` take `project`. Give
a comma-separated list, `project=web,fleet`, or repeat the parameter,
`project=web&project=fleet`. Any one name matches. `-` matches the items with
no project, so `project=-` lists the unfiled events. A bad name is a 400.
`/v1/state` and `/v1/checkins` filter on the key's project, as key
continuity keeps it.

`GET /v1/projects` answers `{"projects": [{"name", "events", "last_at"}],
"unfiled": n}`, by name. `last_at` is when the newest event arrived.

The push payload carries the project as `heraldo.project`.

### Backfill

`heraldo-server backfill-projects [-dry-run] [-rules <file>]` gives the stored
events that have no project one: the projects map's answer for the stored
event, else the project of an earlier event with the same key. It sets only
the `project` column and field, and never pushes. Then each state row with no
project takes the project of its key's newest event that has one. `-rules`
defaults to `$HERALDO_RULES`.

`-dry-run` prints the counts per project and the state rows it would change,
and changes nothing. A rerun changes nothing new. Run `-dry-run` first and
read the counts.
