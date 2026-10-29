# MCP server

`heraldo mcp` serves heraldo to coding agents over the Model Context Protocol,
on stdio. Agents read what happened, act on incidents, checks and mutes, and
send the owner a message. It is part of the CLI: nothing extra to install, and
nothing changes on the server.

The tool layer lives in `internal/mcptools` and knows nothing of the
transport, so an HTTP transport can mount the same tools later.

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
- With `--read-only`, or without a write token, only the six read tools are
  listed. `overview` says why in `server.write_off`. `--read-only` also drops
  `settings_write`.
- One source per harness lets the owner mute one agent: `mute` with
  `source: agent:codex`.

## Tools

| Tool | Does | Token |
|---|---|---|
| `overview` | Unread count, open incidents, checks that are down or late, running Live Activities, mutes in force | read (mutes: write) |
| `events_search` | List or search events: filters, full text, unread, cursor paging | read |
| `event_read` | One event in full: body (paged), details, delivery, and why it did or did not push (`why`, `decisions`) | read |
| `checkin_read` | One check: schedule, state, due times, the end of the last log | read |
| `activity_read` | One Live Activity: content, device instances, timeline | read |
| `incident_act` | Ack or resolve an open incident | write |
| `checkin_update` | Pause, resume, or replace a check's schedule | write |
| `mute` | Mute a source, key or project for a while, or end a mute | write |
| `send` | Post a plain message to the owner | write |
| `settings_read` | Server settings: value, type, default, source and lock; a secret shows only `set` | read |
| `settings_write` | Change one server setting, or put it back to its default | admin |

Left out on purpose:

- Moving the read cursor. Reads never clear the owner's unread badge.
- Ping keys. A key is a credential and would land in transcripts.
- Opening incidents, check-in events, Live Activities, re-alerts. `send` posts
  kind `message` only.
- Icons, devices and the event stream.
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
| `activity_read` timeline | the newest 50 by default, 200 at most |
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
  on `send`, `incident_act`, `checkin_update`, `mute` and `settings_write`.
