# MCP server

`heraldo mcp` serves heraldo to coding agents over the Model Context Protocol,
on stdio. Agents read what happened, act on incidents, checks, mutes and the
alert hold, and send the owner a message. It is part of the CLI: nothing extra
to install, and nothing changes on the server.

The server can serve the same tools over HTTP at `POST /v1/mcp`, for an agent
with no CLI ("Over HTTP", below). The tool layer lives in `internal/mcptools`
and knows nothing of the transport; `internal/mcphttp` mounts it on the
server.

## Start and settings

```sh
heraldo mcp [--read-only]
```

- stdout carries only MCP JSON-RPC. Logs go to stderr as JSON lines.
- Settings come from the environment first, then from the CLI's env file
  (`$XDG_CONFIG_HOME/heraldo/env`, or the file `HERALDO_ENV_FILE` names).
- The server calls heraldo on the first tool call, not at start. A slow or
  asleep server does not block the harness.

| Variable | Use |
|---|---|
| `HERALDO_URL` | Server root. Required. |
| `HERALDO_TOKEN` | A `read` token. Required. |
| `HERALDO_WRITE_TOKEN` | A `write` token. Without it, the write tools are not listed. |
| `HERALDO_ADMIN_TOKEN` | An `admin` token. Without it, `settings_write` is not listed. |
| `HERALDO_CURSOR` | The read cursor that `unread` reads. Default `inbox`, shared with the apps. The MCP server never moves it. |
| `HERALDO_SOURCE` | The `source` of events the tools post. Default: `agent:<harness>`, from the client's name (`agent:claude`, `agent:codex`, `agent:opencode`), else `heraldo-mcp`. |
| `HERALDO_PROJECT` | `send`'s default `project`. Default: none. |

- Without `HERALDO_URL` or `HERALDO_TOKEN`, the server still starts and lists
  only `overview`. `overview` fails with `not_configured` and names the missing
  variable and the env file.
- With `--read-only`, or without a write token, only the nine read tools are
  listed. `overview` says why in `server.write_off`. `--read-only` also drops
  `settings_write`.
- One source per harness lets the owner mute one agent: `mute` with
  `source: agent:codex`.

## Tools

| Tool | Does | Token |
|---|---|---|
| `overview` | Unread count, open incidents, checks that are down or late, running Live Activities, mutes in force. On request: statuses, projects, devices | read (mutes: write) |
| `events_search` | List or search events: filters, full text, unread, cursor paging | read |
| `event_read` | One event in full: body (paged), details, delivery, why it did or did not push (`why`, `decisions`), attachment list | read |
| `checkin_read` | One check: schedule, state, due times, the end of the last log | read |
| `activity_read` | One Live Activity: content, device instances, timeline | read |
| `attachment_read` | One attached file: text inline (paged), an image as image content | read |
| `hold_read` | The alert hold: mode, dead-man, held entries. A look, not a triage poll, unless `poll` is true | read |
| `incident_act` | Ack or resolve an open incident | write |
| `checkin_update` | Pause, resume, or replace a check's schedule | write |
| `mute` | Mute a source, key or project for a while, or end a mute | write |
| `send` | Post a plain message to the owner, with optional base64 attachments | write |
| `settings_read` | Server settings: value, type, default, source and lock; a secret shows only `set` | read |
| `settings_write` | Change one server setting, or put it back to its default | admin |
| `hold_act` | Release held entries as one diagnosis, or suppress them | write |
| `views_read` | The saved views: filter and bundle | read |
| `views_write` | Save a view (replacing one with the same name), or delete one | write |

Each tool's description says what it is best for, what it returns, what it is
not for, and which tool follows it. Two short notes on the newer ones:

- `hold_read`. Best for: "what is held", and triage. Not for: held events in
  history; `events_search hold=held` lists those.
- `hold_act`. Best for: closing a group of held alerts with one call after
  `hold_read`. Not for: incidents (`incident_act`) or mutes (`mute`).
- `views_read`. Best for: the view name to pass to `events_search view=`.
  Not for: the events in a view; `events_search view=<name>` lists those.
- `views_write`. Best for: the owner asks for a view, or to fold a noisy
  source's pushes into one notification (`bundle`). Not for: muting (`mute`).
- `events_search` takes `view` (a saved view's name or id) and `level`. With
  `view`, the view's own routine filter applies unless `routine` is given.

`overview` takes `sections`. The default is `unread`, `incidents`,
`checkins`, `activities` and `mutes`. Ask for `statuses`, `projects` (the
names to filter by, with counts) or `devices` (the owner's phones and Macs,
and whether each gets pushes) by name.

Left out on purpose:

- Moving the read cursor. Reads never clear the owner's unread badge.
- Ping keys. A key is a credential and would land in transcripts.
- Opening incidents, check-in events, Live Activities, re-alerts. `send` posts
  kind `message` only.
- Retiring a device. A wrong call stops pushes to the owner's phone. The CLI
  has `heraldo devices retire`.
- Icons and the event stream.
- Secrets, such as `llm.token`. `settings_write` refuses them, since they
  would land in the transcript. The owner sets them with
  `heraldo settings set llm.token -` and stdin.

## Results and limits

Each result has one compact text block for the model and `structuredContent`
that matches the tool's `outputSchema`.

| Limit | Value |
|---|---|
| `events_search` page | 20 by default, 100 at most |
| `overview` lists | 20 per section; totals are always given |
| `event_read` body | 16 KiB per call; pass `body_page.next_offset` as `body_offset` |
| `event_read` details | 4 KiB of JSON, else `{"_omitted_bytes": n}` |
| `event_read` delivery | counts, plus the first 10 outcomes |
| `event_read` decisions | every push stage the event passed through; absent before the server decides it |
| Check `last_log` | the last 8 KiB |
| `hold_read` entries | 50, oldest first, each body 2 KiB; `total` is always given |
| `activity_read` timeline | the newest 50 by default, 200 at most |
| `attachment_read` text | 32 KiB per call; pass `page.next_offset` as `offset`. A PDF or other type returns its type and size only |
| `send` attachments | 4 at most, each 5 MiB decoded |
| Any result | 64 KiB of JSON; `events_search` drops rows to fit and sets `truncated` |

Paging: `events_search` returns `next_cursor`. Pass it alone as `cursor`. It
carries the first call's filters and order. With `order: oldest` or `unread`,
an empty page still returns a `next_cursor` to poll with later.

## Errors

A failed call returns `isError: true`, a text of the form `<what failed>. <what
to do>.`, and `structuredContent.error` with `code`, `status`, `message` and
`hint`. No error or log line carries a token.

| code | When |
|---|---|
| `not_configured` | `HERALDO_URL` or `HERALDO_TOKEN` is not set |
| `unauthorized` | The server rejected the read or the write token (401) |
| `forbidden` | The token lacks the scope (403) |
| `not_found` | No such event, check, activity or mute (404) |
| `invalid` | Bad arguments, or the server's 400 text |
| `too_large` | The server refused the size (413) |
| `no_open_incident` | `incident_act` on a key with no open incident |
| `nothing_open` | `hold_act` with no id that names an open entry (409) |
| `unreachable` | No answer from `HERALDO_URL` |
| `server_too_old` | The server cannot list newest first; pass `order: oldest` |
| `unsupported_filter` | The server has no `project` field yet, or no project mutes |
| `locked_by_env` | `settings_write` on a key a server flag or env var sets |
| `no_secret_key` | The server has no `HERALDO_SECRET_KEY_FILE` for a secret write |
| `secret` | `settings_write` on a secret key |

## Routine events

`events_search` leaves routine events out by default: check-in pings that did
not fail, progress and status (docs/api.md, "Routine events"). It keeps them
when `kind` is `checkin`, `progress` or `status`, or when `key` is set, so
`key: <slug>` gives a check's pings. `routine` overrides both: `exclude`,
`include` or `only`. The cursor keeps the choice. A server before the filter
ignores it; the client then drops the rows itself, and a page can come back
shorter than `limit`.

## Projects

`events_search`, `overview` and `send` take `project`. The MCP server sends it
only when set. The read tools take a list and send one `project` parameter per
name; `-` means events with no project. A server without the field ignores the
filter on reads and refuses it on `send`; both give `unsupported_filter`
rather than wrong results. `overview` does not filter the unread count,
activities or mutes by project. `mute` takes one `project` and mutes every
event filed under it; a server before project mutes refuses it, and the
tool answers `unsupported_filter`.

## Register it

The env file holds the URL and tokens, so these configs carry no secrets.
Replace `https://heraldo.example.net` with your server, or leave `env` out
when the env file sets it.

Claude Code:

```sh
claude mcp add --scope user heraldo -e HERALDO_URL=https://heraldo.example.net -- heraldo mcp
```

Codex (`~/.codex/config.toml`):

```toml
[mcp_servers.heraldo]
command = "heraldo"
args = ["mcp"]
env = { HERALDO_URL = "https://heraldo.example.net" }
```

OpenCode (`opencode.json`):

```json
{
  "mcp": {
    "heraldo": {
      "type": "local",
      "command": ["heraldo", "mcp"],
      "environment": { "HERALDO_URL": "https://heraldo.example.net" },
      "enabled": true
    }
  }
}
```

- A read-only agent: add `--read-only` after `mcp`.
- To keep tokens out of files, point `HERALDO_ENV_FILE` at a mode 600 file, or
  start the command under a secret manager's `run` wrapper.
- Permissions: the read tools are safe to allow without a prompt. Keep a prompt
  on `send`, `incident_act`, `checkin_update`, `mute`, `hold_act`, `views_write` and
  `settings_write`.

## Over HTTP

With `HERALDO_MCP_HTTP=on`, the server serves the same tools at
`POST /v1/mcp`. Off, the path answers 404. Use it for an agent on a machine
without the CLI, or a hosted agent that can only reach a URL.

- Transport: MCP streamable HTTP, stateless. Each POST carries one JSON-RPC
  message and gets a JSON answer. The server keeps no MCP session, so GET and
  DELETE get 405.
- Auth: `Authorization: Bearer <token>` on every request, a token from the
  server's token list. No token, or an unknown one, gets 401. A token with
  the `notify-key` or `trusted-ingest` capability gets 403: it is a sender's
  credential, and a notify-key may sit in sender configs on many hosts. Give
  each agent its own `read` or `write` token.
- The token's scope picks the tools:

  | Scope | Tools |
  |---|---|
  | `read` | the nine read tools |
  | `write` | the read and write tools, fifteen |
  | `admin` | all sixteen, with `settings_write` |

- A `write` token may also read through MCP, because a client sends one token
  and the write tools read before they write. Outside MCP the same token still
  cannot read (docs/api.md, "Auth").
- The tools call the API in the server's own process, so each call is
  checked as the REST call is. Nothing leaves the process.
- Posted events have source `agent:<token name>`. Give each agent its own
  token, and `mute` with `source: agent:<name>` silences one agent.
- `unread` reads the `inbox` cursor. `send` has no default project or host.
- The request log has one line per POST, with the token's name.
- Put the server behind TLS. The token is a bearer credential.

Register it. Keep the token in an environment variable, not in the config
file. Replace `https://heraldo.example.net` with your server.

Claude Code expands `${VAR}` in `url` and `headers` when it loads the config:

```sh
claude mcp add-json --scope user heraldo \
  '{"type":"http","url":"https://heraldo.example.net/v1/mcp","headers":{"Authorization":"Bearer ${HERALDO_MCP_TOKEN}"}}'
```

The single quotes keep the shell from expanding `${HERALDO_MCP_TOKEN}`, so
the stored config holds the variable's name. `claude mcp add --transport http
heraldo <url> --header "Authorization: Bearer <token>"` also works, but it
stores the token in the config.

Codex (`~/.codex/config.toml`):

```toml
[mcp_servers.heraldo]
url = "https://heraldo.example.net/v1/mcp"
bearer_token_env_var = "HERALDO_MCP_TOKEN"
```

Set `HERALDO_MCP_TOKEN` in the agent's environment, for example from a secret
manager's `run` wrapper. For a read-only agent, give it a `read` token.
