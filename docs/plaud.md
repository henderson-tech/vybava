# plaud — Plaud recordings without an MCP server

`@plaud-ai/mcp` spawned one Node process per agent session (1300 MCP
processes machine-wide the day it was retired). `plaud` is a compiled applet
that talks to the Plaud developer API directly; the manual-only skill
`skills/plaud/SKILL.md` (`/plaud`) is the agent-facing doctrine.

## Install

```sh
go build -o ~/.local/share/vybava/bin/vybava-dev ./cmd/vybava \
  && ~/.local/share/vybava/bin/vybava-dev install plaud-cli plaud   # dev build, how the other applets are linked here
vybava install plaud-cli plaud                                       # from a tagged release
```

`plaud-cli` links `~/.local/bin/plaud` to the multicall binary; `plaud`
copies the skill into `~/.claude/skills/plaud` (and Codex via the installer
adapters).

## Auth model

OAuth 2.0 authorization code + PKCE for Plaud's public client
(`client_9c501dad-…`, override `PLAUD_CLIENT_ID`). The registered redirect is
`http://localhost:8199/auth/callback` — Plaud validates it, so the port is
not configurable.

| Token | Where it lives |
|---|---|
| refresh token (long-lived) | onyx vault only: `onyx://Plaud/Plaud%20OAuth%20refresh%20token/token`, injected as `PLAUD_REFRESH_TOKEN` per call |
| access token (short-lived) | `~/.plaud/access-token.json`, 0600, with expiry and a one-way hash of client ID + refresh token as its key (a different vault credential never reads it); refreshed transparently |

- `plaud login --json` — consent flow; prints the raw token JSON to stdout
  and nothing else there (progress on stderr), so `mcp__onyx__run_command`
  `capture {json_path: "refresh_token"}` stores it without the value ever
  entering an agent context. Nothing is persisted by the CLI. A capture
  always mints a new vault item: delete the old one by ref only after the
  new one landed, until the ref resolves to the captured id.
- `plaud refresh --json` — exchanges `$PLAUD_REFRESH_TOKEN`, caches the
  access token, prints the token response (including a rotated
  `refresh_token` when Plaud issues one — same capture flow updates the
  vault).
- A data command that observes a rotation prints a loud stderr notice; the
  old refresh token keeps working until the vault is updated only if Plaud
  did not revoke it, so act on the notice. Under `onyx run_command` that
  stderr is redacted, so there the symptom is a later 401.
- 401 on refresh → `plaud login` again. 401 on a data call → the cache is
  cleared and the call retried once.

## Data commands

| Command | API |
|---|---|
| `plaud whoami` | `GET /open/third-party/users/current` |
| `plaud files [--query q] [--page n] [--page-size n]` | `GET /open/third-party/files/`; `--query` scans up to 5×100 names case-insensitively (what the MCP's `list_files` did) |
| `plaud file <id>` | `GET /open/third-party/files/{id}` |
| `plaud note <id>` | the file's `note_list` |
| `plaud transcript <id> [--block …]` | the file's `source_list` block (`transaction` default, `transaction_polish`, `outline`) — inline `data_content` or fetched from `data_link` |

`--json` prints the API payload verbatim (`files`, `file`, `note`) or a stable
`{file_id, block, available_blocks, total, segments|text}` for `transcript`.
`--out <file>` sends that result to a file instead — or `{"error": …}` when
the command fails (plus `output`, whatever it had written first). The file is
owner-only (0600) and its directory is created on demand. It is required under
`onyx run_command`, which redacts the whole output of a call it injects the
token into (the helper is shared with posta: `internal/cli/outflag.go`).
The MCP's transcript pagination (`next_cursor`) was local slicing of one
document; the CLI returns the whole block in one call.

## Source

`internal/plaud/` — `plaud.go` (config, PKCE, exchange, refresh),
`login.go` (callback server), `cache.go` (access-token cache + session),
`client.go` (REST calls). CLI wiring only in `internal/cli/plaud.go`. Tests
are httptest-based, one per behaviour.
