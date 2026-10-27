# heraldo

A self-hosted notification hub: one Go server stores every event and delivers it
to a native iOS and macOS app. Later, a status overview on the same event stream.
The plan and its stages live in bm (project `heraldo`).

## Layout

| Path | Contents |
|---|---|
| `schema/` | `envelope.v1.schema.json` and `openapi.yaml`. The one source of truth for the API. |
| `api/` | Go types generated from `schema/` (`types.gen.go`; never edit it by hand). `internal/bundle` inlines the JSON Schema's `$defs` for oapi-codegen |
| `cmd/heraldo-server`, `cmd/heraldo` | Server and CLI |
| `internal/` | Server packages: `config` (env), `server` (handlers), `store` (SQLite, migrations in `store/migrations`), `validate` (envelope checks), `auth` (tokens), `ids` (ULIDs), `teeimport` (apprise-tee backfill), `rules` (ingest rules, `HERALDO_RULES`), `replay` (rules against a catalogue, in memory), `push` (APNs sender: provider token, payload, push table, queue), `live` (Live Activities for kind progress: push-to-start, throttled updates, the 8 hour chain, ghost handling), `version`; `client`, the API client the CLI uses |
| `apple/` | XcodeGen project: multiplatform app `Heraldo`, `HeraldoNotificationService` (iOS), `HeraldoWidgets` (widgets and the Live Activity), package `HeraldoKit`; `LiveActivity/` holds code built into both the app and the widget extension |
| `deploy/` | Generic `compose.yml`, `heraldo.example.env` and `rules.example.yaml` |
| `design/icon/` | App and web icon and the wordmark. `heraldo.svg` is the source of truth; `build.py` derives the masters, layers, web set, logos, wordmarks, README banners and og-images, `apple/App/AppIcon.icon` and the logo assets in `apple/App/Shared/Assets.xcassets` |
| `docs/` | API summary, Apple notes |

## Build and test

| Task | Command |
|---|---|
| Go tests | `go vet ./... && go test ./...` |
| Regenerate Go types after a schema change | `go generate ./...`, then commit `api/types.gen.go` |
| Run the server | `go run ./cmd/heraldo-server` (port 7842, database `heraldo.db`; see the README for `HERALDO_*`) |
| Run the CLI against a server | `HERALDO_URL=... HERALDO_TOKEN=... go run ./cmd/heraldo ls` (see the README, "CLI") |
| Hash a token for the token list | `go run ./cmd/heraldo-server hash-token < token.txt` |
| Back up a running server's database | `heraldo-server backup <new-path>` |
| Backfill an apprise-tee catalogue | `heraldo-server import-tee <dir>` |
| Check a rules file against a catalogue | `heraldo-server replay-tee -rules <file> <dir>` |
| Build the image | `docker build -t heraldo .` (CI pushes `ghcr.io/noahkiss/heraldo` on main and `v*` tags) |
| Release | Push a `v*` tag. CI builds the image, then dispatches the tap's `bump.yml` (formula `heraldo`) with repo secret `HOMEBREW_TAP_TOKEN` and waits for the verified bump |
| Release the Mac app | After the `v*` tag: `apple/scripts/build.sh`, then `NOTARY_PROFILE=<profile> apple/scripts/notarize.sh` (makes `apple/build/Heraldo-<version>-macos.zip`), `gh release upload <tag> <zip>`, then `gh workflow run bump.yml -R noahkiss/homebrew-tap -f formula=heraldo-app -f tag=<tag>`. `MARKETING_VERSION` in `Names.xcconfig` matches the tag |
| Save the Mac app's URL and tokens from the shell | `HERALDO_URL=... HERALDO_READ_TOKEN_CMD=... HERALDO_WRITE_TOKEN_CMD=... apple/scripts/provision.sh` (README, "Provision from the shell") |
| Apple build (iOS Simulator + macOS) and HeraldoKit tests | `apple/scripts/check.sh` (the tests that need a server skip) |
| HeraldoKit tests against a local server | `apple/scripts/test-server.sh` (builds `heraldo-server`, temp database, throwaway tokens) |
| Regenerate the Xcode project | `apple/scripts/generate.sh` (the `.xcodeproj` is not committed) |
| Rebuild the icon after editing `design/icon/heraldo.svg` | `uv run design/icon/build.py --apple` (see `design/icon/README.md`) |

- A schema change adds or updates examples in `schema/examples/valid` and `invalid`
  (sender inputs) and `stored` (events as the read API returns them).
- CI runs the Go checks, then builds the image. Run `apple/scripts/check.sh` before
  pushing Apple changes.
- Names and bundle IDs live in `apple/Config/Names.xcconfig`. Signing lives in the
  untracked `apple/Config/Local.xcconfig`.
- Never edit an applied database migration. Add a new numbered migration instead.

## Hygiene: the repo stays publishable

- Nothing homelab-specific goes in the repo: no real hostnames, IPs, tailnet names,
  usernames, home paths, or vault and item names. This covers code, docs, examples,
  commit messages and CI.
- Configuration comes from the environment or a config file. No baked-in default
  names a real machine.
- The homelab deployment (stack, hostnames, tokens, sender rules) lives in the
  private servers repo, not here.
- Test fixtures are written by hand. Never copy real notification data into the repo.
