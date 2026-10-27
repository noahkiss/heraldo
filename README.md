<picture>
  <source media="(prefers-color-scheme: dark)" srcset="design/icon/export/logo/readme-banner-dark.png">
  <img alt="Heraldo" src="design/icon/export/logo/readme-banner-light.png" width="640">
</picture>

# Heraldo

A self-hosted notification hub. Services post events to one server. The server
stores every event and delivers it to an iPhone and Mac app you build yourself.

Status: early scaffold. The server answers `/healthz` and nothing else yet.

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
| `internal/` | Server packages |
| `apple/` | Xcode project (XcodeGen): the Heraldo app for iOS and macOS, its extensions, HeraldoKit |
| `deploy/` | A generic compose file and an example env file |
| `docs/` | Design notes |

## Build

Go 1.27 or later:

```sh
go test ./...
go run ./cmd/heraldo-server        # listens on :7842; set HERALDO_ADDR to change it
go run ./cmd/heraldo version
go generate ./...                  # regenerate api/types.gen.go after a schema change
```

Apple (Xcode 27, XcodeGen):

```sh
apple/scripts/check.sh             # builds for the iOS Simulator and macOS, runs HeraldoKit tests
```

Container:

```sh
cp deploy/heraldo.example.env deploy/heraldo.env
docker compose -f deploy/compose.yml up --build
```

## Configuration

The server reads its configuration from the environment.

| Variable | Default | Meaning |
|---|---|---|
| `HERALDO_ADDR` | `:7842` | Listen address, `host:port` |

## License

MIT. See [LICENSE](LICENSE).
