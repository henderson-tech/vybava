# blip — chaos proxy for polish passes

One named proxy per `blip up`; the app under test points at it and keeps
pointing at it while faults are toggled from the CLI. Never touches the
machine's network. Install: `vybava install blip` (multicall link) or
`vybava blip …`. Skill: `skills/polish`.

## Verbs

```
blip up <name> --listen <addr> --to http://host:port | tcp://host:port
blip <name> set <fault> [arg] [--match <glob>] [--method M] [--after N] [--for <dur>] [--rate p]
blip <name> ok          # clear the fault, release timeout-held requests
blip <name> cut         # close every established connection once; fault unchanged
blip <name> status      # mode, listen, upstream, fault, counters
blip <name> log [--tail] [--last N]
blip <name> record on|off|clear
blip <name> authz --as <identity> [--expect 401,403,404] [--only <glob>] [--exclude <glob>] [--mutations]
blip ls | blip down <name> | blip down --all
```

`up` daemonizes (re-executes the binary detached with a hidden `serve`),
waits up to 3 s for the control socket and prints the URL plus an env hint
(`EXPO_PUBLIC_API_URL=…` for http, `DATABASE_URL host:port` for tcp). It is
idempotent for identical args (`ALREADY_UP`, ok) and refuses differing ones
(`ARGS_DIFFER`, fix = `down` then `up`). One fault at a time: `set` REPLACES
the active fault (and restarts `--after` counting); `ok` clears it.

## Faults

| fault | http (`--to http://`) | tcp (`--to tcp://`) |
|---|---|---|
| `delay <dur> [--jitter <dur>]` | sleep before forwarding | sleep before each chunk, both directions |
| `drop` | hijack + RST, no response | accept then RST |
| `error <status> [--body s]` | answer without forwarding; JSON body → `application/json` | refused: `HTTP_ONLY` |
| `timeout` | read, never answer; `ok`/next `set` answers 503 | accept, never forward; released on `ok` |
| `slow <rate>` | throttle the response body | throttle both directions |
| `flap <down>/<up>` | `drop` during down windows, pass during up | same |

Rates are bits per second: `20kbps`, `1.5mbps`, `800bps`. Scoping:
`--match` is a path glob where `*` also crosses `/` (http only), `--method`
(http only), `--after N` skips the first N matching requests/connections,
`--for <dur>` auto-clears on a timer — a held `timeout` is released even
with no further traffic (logged as `FAULT_EXPIRED`), `--rate p` applies
with probability p (default 1). A fault swap is an atomic pointer store, safe
under concurrent requests.

## Access-control replay (http only)

`record on` makes the daemon append every proxied request (method, FULL URL
— path and query, it is the request under test — headers, body ≤ 64 KiB,
response status and byte length; never the response body) to
`<name>.rec.jsonl` (0600, removed by `record clear` and `down`). Credential
headers (`Authorization`, `Proxy-Authorization`, `Cookie`, `X-Api-Key`,
`X-Auth-Token`) and credential query keys (`access_token`, `X-Amz-Signature`, `X-Amz-Security-Token`, `X-Amz-Credential`, `X-Goog-Signature`, `X-Goog-Credential`,
`sig`, `signature`, `token`, `api_key`, `apikey`, `key`, `auth`, `jwt`,
`session`; case-insensitive) are stripped at record time and never re-added by
a replay; only their NAMES are kept and `status` lists them. JSON (one level)
and form bodies get credential keys (`password`, `secret`, `token`,
`client_secret`, `otp`, `code`, …) replaced by `[redacted]`; such a record
lists them in `body_redacted` and is skipped by `authz`. Paths are stored
decoded (for `--only`) and raw (`raw_path`, what a replay sends). `status`
shows `recorded=N`.

`authz --as <identity>` replays each distinct recorded request straight at
the upstream (not through the fault layer) with the identity substituted and
lists every answer whose status is NOT in `--expect` (default 401,403,404):

- `--as none` strips `Authorization` and `Cookie` (unauthenticated check)
- `--as 'header:Authorization=Bearer <token>'` replaces that header (another user/role)
- `--as 'cookie:<name>=<value>'` replaces that cookie
- `--as env:<VAR>` sets `Authorization` from that variable (inject it via onyx `run_command`; blip never prints it)

Identity comes only from these flags; every replay strips the whole
credential set first and carries exactly the `--as` credential. No
diagnostic ever echoes an `--as` value. Mutating methods (POST/PUT/PATCH/
DELETE) are replayed only with `--mutations` and otherwise listed as
skipped. Verdicts: `same payload` (2xx and the same byte length as the
original — strongest), `different payload` (2xx, other length), `not
refused`. Exit 2 with `AUTHZ_LEAK_CANDIDATES` when any candidate exists,
else 0. `record clear` deletes the recording.

## State

`~/.local/state/blip/` (override `BLIP_STATE_DIR`): `<name>.json` (pid,
mode, addresses, fault, recording), `<name>.sock` (control, JSON over HTTP),
`<name>.log` (one line per request/connection: time, method+path — never the
query string — or conn id, fault, status, duration), `<name>.rec.jsonl`.
`log` reads the tail backwards in chunks; `log --tail` streams plain lines,
or under `--json` one `{"event":"log","line":"…"}` per line and the envelope
on Ctrl-C. A state file whose daemon no longer answers is `STALE_STATE`;
`blip down <name>` cleans it: a lingering pid is signalled only when its
command line is `… serve --name <name> …` (PIDs are recycled; on Windows it
is never signalled), then all four files are removed.

## Envelope and exit codes

Every verb takes `--json` and emits one `{v, ok, verb, data, diagnostics,
next}` (cli-craft, `internal/runx`). Human mode prints one short line per
fact, then diagnostics as `CODE: detail — fix` and the `next` commands.
Exit 0 ok, 1 infra, 2 diagnostics present. Diagnostic codes are the closed
enum in `internal/blip/blip.go`: `NOT_RUNNING`, `STALE_STATE`,
`ARGS_DIFFER`, `ALREADY_UP`, `UPSTREAM_INVALID`, `LISTEN_INVALID`,
`FAULT_INVALID`, `HTTP_ONLY`, `NAME_INVALID`, `STARTUP_TIMEOUT`,
`NOTHING_RECORDED`, `IDENTITY_INVALID`, `AUTHZ_LEAK_CANDIDATES`,
`FAULT_EXPIRED`.

`next` on success: `up` → `set delay 800ms`, `status`; `set` → `ok`,
`log --tail`; `ok` → `log --tail`, `set …`; `cut` → `log --tail`; `status`
→ `set …`/`ok`; `record on` → `record off`; `record off` → `authz --as
none`; `authz` → `record clear`; `down` → `ls`; `ls` → `<first> status`,
`down --all`.
