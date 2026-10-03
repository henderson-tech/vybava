# cmux — the control-socket client

`internal/cmux` is Výbava's one client of the [cmux](https://cmux.com) terminal's
control socket. `fleet` acts through it (focus, reply, screen, dialog) and
`watch serve` follows its event stream; nothing else in Výbava talks to cmux.

## Wire

One JSON object per line, v2: `{"id","method","params"}` →
`{"id","ok":true,"result"}` or `{"ok":false,"error":{"code","message","data"}}`.
The client opens one connection per request (cmux serves each on a pool slot)
and never caches a surface across calls.

| Method | Params | Answer |
|---|---|---|
| `system.capabilities` | — | `access_mode`, `version` (protocol), `methods[]` |
| `system.identify` | — | `app_bundle_path` (the app version is read from its `Info.plist`) |
| `agent.resolve_delivery_target` | `pid` | `surface_id`, `workspace_id`, `pid_resolution`; `not_found` when no surface hosts the pid |
| `surface.read_text` | `surface_id`, optional `lines` + `scrollback` | `text`, `window_id`, `workspace_id` |
| `terminal.paste` | `surface_id`, `text`, `submit_key` (`return`/`none`) | `submitted`, `submit_error`, `delivery` |
| `surface.send_text` | `surface_id`, `text` — typed as keystrokes; a dialog option is its digit (`surface.send_key` takes named keys only and refuses `4`) | target ids |
| `window.focus` / `workspace.select` / `surface.focus` | the matching id | — |
| `events.stream` | `after_seq`, `categories[]`, `include_heartbeats` | an `ack` frame, then `event`/`heartbeat` frames on the same connection |

## Rules

- **Discovery**: `$CMUX_SOCKET_PATH` (inside cmux terminals), else
  `~/.local/state/cmux/last-socket-path`, else `~/.local/state/cmux/cmux.sock`.
  Fleet.app and the watch LaunchAgent are not cmux children and see no env var.
- **Access mode**: only `automation` admits a process cmux did not start. Under
  any other mode cmux closes such a connection without a reply — `Check`
  reports `denied`; a caller that got in under another mode is `restricted`
  (it works for you, not for Fleet.app). Fix: cmux Settings → Socket Control →
  Automation.
- **Retries**: only a reply cmux marks `data.retryable` (`overloaded`, a
  withdrawn main-actor hop) is re-sent, up to 3 times honoring
  `retry_after_ms`. A `timeout` with `retryable:false` started and its result
  is unknown — never re-sent. A paste cmux accepted is never re-sent, even
  when `submitted` is false: the text is already at the prompt.
- **Version**: `MinVersion` is 0.64.25, and every method in `RequiredMethods`
  must be advertised. `surface.input_state` (upstream's draft/dialog probe) is
  NOT in 0.64.25, so dialog detection is fleet's own screen parse.
- **Events**: the first connect starts at the live edge; a reconnect resumes
  after the last delivered `seq`. `Follow` calls `gap` when the ack says
  events were dropped or cmux restarted (`boot_id` changed — sequences restart
  with it); the caller then refreshes its whole view. Three missed heartbeats
  (45 s) end a silent stream. Payloads of agent-hook events carry `_ppid`, the
  agent's pid.
- `read_text` returns faint placeholder text (Claude Code's suggested next
  prompt) as plain text, indistinguishable from a typed draft.

Tests drive a fake unix socket (`internal/cmux/cmux_test.go`); macOS caps
socket paths at 104 bytes, so the fake lives under `os.MkdirTemp("", …)`,
never `t.TempDir()`.
