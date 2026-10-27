# API

The source of truth is [schema/openapi.yaml](../schema/openapi.yaml) and
[schema/envelope.v1.schema.json](../schema/envelope.v1.schema.json). This page
is a summary.

## Endpoints

An endpoint with a stage number is planned for that stage and has no handler
yet. The spec marks it `x-stage: <n>`.

| Method and path | Does | Stage |
|---|---|---|
| `GET /healthz` | Liveness. No auth. | served |
| `POST /v1/events` | Stores one `EventInput`. Returns the stored `Event` (201), or the first event for a repeated `dedup` (200). | 1 |
| `GET /v1/events?after=&limit=&source=&kind=` | Lists events after a cursor, oldest first. | 1 |
| `GET /v1/events/{id}` | Returns one event with its full body. | 1 |
| `GET /v1/send?source=&kind=&...` | Same as `POST /v1/events` for GET-only clients. Scalar fields and `tags` only. | 1 |
| `GET /ping/{slug}`, `/start`, `/fail`, `/log`, `/{exit_code}` | Check-in pings in the healthchecks.io shape. `?rid=` pairs runs; `?create=1` creates an unknown check. | 6 |
| `POST /notify/apprise`, `POST /notify/{key}` | Apprise-compatible ingest. Answers 200 with `Sent`. | 11 |
| `GET /v1/state` | Flat arrays for the status view: open incidents, running progress, check-ins, set statuses. | 12 |

Auth:

- Every call except `/healthz` takes a bearer token: one per sender for writes,
  one per client for reads.
- `GET /v1/send` also takes the token as a `token` query parameter. A token in
  the URL ends up in access logs, proxy logs and browser history. Use the header
  when the client can set one.

## Input and stored events

- **`EventInput`** is what a sender posts. It is the schema root.
- **`Event`** is what the read API returns: `$defs/Event` in the schema.
- The server owns `id`, `received_at`, `hold` and `delivery`. An input that sets
  any of them is rejected. `ts` stays the sender's; the server fills it with the
  arrival time when it is missing.

## Envelope v1

`source` and `kind` are required. `kind` picks the meaning of `key` and `action`:

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
| `id` | Event id, sortable by arrival. |
| `received_at` | When the server stored the event. |
| `hold` | Triage state. |
| `delivery` | Per-device outcome: `sent`, `failed` or `skipped`, with the level the push carried. |

## APNs mapping

| Envelope | APNs |
|---|---|
| `thread` | `thread-id` |
| `key` | `apns-collapse-id`, as a hash of the key |
| `level` | `interruption-level`. Without the entitlement, `critical` goes out as `time-sensitive` and `delivery` records the downgrade. |
| `expires_at` | `apns-expiration` |
| `relevance` | `relevance-score` |
| `summary`, `body` | The push carries the summary, or the body truncated to fit 4 KB. The notification service extension fetches the full text by `id`. |

## Examples and types

- `schema/examples/valid` and `invalid` hold sender inputs. `stored` holds
  events as the read API returns them.
- `go test ./api` checks each example against the schema and the generated Go
  types.
- `go generate ./...` regenerates `api/types.gen.go`. oapi-codegen cannot follow
  `$defs` references, so `api/internal/bundle` inlines the JSON Schema into the
  OpenAPI document first.
