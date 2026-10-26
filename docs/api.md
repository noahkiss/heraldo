# API

The source of truth is [schema/openapi.yaml](../schema/openapi.yaml) and
[schema/envelope.v1.schema.json](../schema/envelope.v1.schema.json). This page
is a summary.

## Endpoints

| Method and path | Does |
|---|---|
| `GET /healthz` | Liveness. No auth. |
| `POST /v1/events` | Stores one envelope. Returns it with `id` and `ts` set. |
| `GET /v1/events?after=&limit=&source=&kind=` | Lists events after a cursor, oldest first. |
| `GET /v1/events/{id}` | Returns one event with its full body. |

Every `/v1` call takes a bearer token.

## Envelope v1

`source` and `kind` are required. `kind` picks the meaning of `key` and `action`:

| kind | key | action |
|---|---|---|
| `message` | optional | none |
| `digest` | optional | none |
| `incident` | required | `open`, `update`, `ack`, `resolve` |
| `progress` | required | `start`, `update`, `end` |
| `checkin` | required | `start`, `success`, `fail`, `log` |

How fields map to APNs:

| Envelope | APNs |
|---|---|
| `thread` | `thread-id` |
| `key` | `apns-collapse-id` |
| `level` | `interruption-level` |
| `body` | Truncated to fit 4 KB. The notification service extension fetches the full text by `id`. |

Examples live in `schema/examples/`. `go test ./api` checks each one against
the schema and the generated Go types.
