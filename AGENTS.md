# heraldo

A self-hosted notification hub: one Go server stores every event and delivers it
to a native iOS and macOS app. Later, a status overview on the same event stream.
The plan and its stages live in bm (project `heraldo`).

## Layout

| Path | Contents |
|---|---|
| `schema/` | `envelope.v1.schema.json` and `openapi.yaml`. The one source of truth for the API. |
| `api/` | Go types generated from `schema/` (`types.gen.go`; never edit it by hand) |
| `cmd/heraldo-server`, `cmd/heraldo` | Server and CLI |
| `internal/` | Server packages: `config` (env), `server` (handlers), `version` |
| `apple/` | XcodeGen project: multiplatform app `Heraldo`, `HeraldoNotificationService` (iOS), `HeraldoWidgets`, package `HeraldoKit` |
| `deploy/` | Generic `compose.yml` and `heraldo.example.env` |
| `docs/` | API summary, Apple notes |

## Build and test

| Task | Command |
|---|---|
| Go tests | `go vet ./... && go test ./...` |
| Regenerate Go types after a schema change | `go generate ./...`, then commit `api/types.gen.go` |
| Run the server | `go run ./cmd/heraldo-server` (port 7842; `HERALDO_ADDR` changes it) |
| Apple build (iOS Simulator + macOS) and HeraldoKit tests | `apple/scripts/check.sh` |
| Regenerate the Xcode project | `apple/scripts/generate.sh` (the `.xcodeproj` is not committed) |

- A schema change adds or updates examples in `schema/examples/valid` and `invalid`.
- CI runs the Go checks only. Run `apple/scripts/check.sh` before pushing Apple changes.
- Names and bundle IDs live in `apple/Config/Names.xcconfig`. Signing lives in the
  untracked `apple/Config/Local.xcconfig`.

## Hygiene: the repo stays publishable

- Nothing homelab-specific goes in the repo: no real hostnames, IPs, tailnet names,
  usernames, home paths, or vault and item names. This covers code, docs, examples,
  commit messages and CI.
- Configuration comes from the environment or a config file. No baked-in default
  names a real machine.
- The homelab deployment (stack, hostnames, tokens, sender rules) lives in the
  private servers repo, not here.
