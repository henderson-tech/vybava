# fleet - every Claude Code session on this Mac, and the work that died

`fleet` is the one reader behind the `/fleet` pane and the lazarus mod
(epic vt-4759, `docs/specs/2026-10-02-claude-mods-brief.md`). It answers
"which of my 40 sessions needs me, and which ones died with work in flight"
from what Claude Code itself records, and never writes anything Claude Code
owns. Code: `internal/fleet`; CLI wiring: `internal/cli/fleet.go`.

```sh
fleet                          # waiting on you first, then dead, busy, idle, shell
fleet --json                   # the snapshot envelope the /fleet pane reads
fleet --no-codex               # skip the Codex rows (no ps/lsof/rollout read)
fleet revive                   # sessions that died with work in flight + resume lines
echo '{"kind":"workflow","id":"wf_1","runId":"wf_1","scriptPath":"/…/x.js","status":"started"}' \
  | fleet ledger record --session <session-id>
fleet ledger show --session <session-id>
fleet ledger close --session <session-id>    # dismiss: every open job → stopped
fleet schema --ts              # the .d.ts of these contracts, for the mods
```

Every verb prints the runx envelope under `--json` (`v`, `ok`, `verb`,
`data`, `diagnostics`, `next`) and exits 0 ok, 1 infra, 2 diagnostics.

## What it reads

- **The session registry**, `~/.claude/sessions/<pid>.json`, one file per
  running Claude Code process: `sessionId`, `pid`, `pidDomain`, `procStart`,
  `status` (busy | idle | waiting | shell), `waitingFor` ("input needed"),
  `statusUpdatedAt` (epoch ms), `cwd`, `name`, `version`, `kind`,
  `entrypoint`. Read-only. Only `*.json` is opened — the per-session `.key`
  files beside them are never read.
- **Live Codex CLIs**, read-only, through `internal/codexusage` (rollouts of
  the last 24 h joined to `ps`/`lsof`). A Codex failure is a warning; the
  snapshot stands without Codex rows.
- **Ledgers** it owns, `~/.local/state/vybava/fleet/ledger/<sessionId>.json`.

The registry format is undocumented. Each file must carry `sessionId`,
`pid`, `status`, `cwd`, `procStart` (strings/numbers as listed) and
`statusUpdatedAt`; one file without them is skipped with
`REGISTRY_FILE_SKIPPED` naming the file and field. When files exist and
**none** has that shape, the format changed under us and the read fails with
`REGISTRY_SHAPE_UNKNOWN` (exit 2) naming the most common missing field —
never an empty fleet that looks healthy.

Claude Code removes a session's registry file on an orderly exit (a
terminal closing under it included); a file whose process is gone is a hard
kill. That is why `revive` also reads ledgers.

## Liveness — dead only when proven

Mirrors `internal/plugingc`'s rule (`processView.classify`): `kill(pid, 0)`
succeeding proves nothing because macOS recycles PIDs. A session is

| liveness   | when |
|------------|------|
| `gone`     | no process holds its PID |
| `foreign`  | the PID is held by something that cannot be Claude Code (not `claude*`, `node`, `bun`, `deno`, `npx`, `bunx`) |
| `recycled` | the PID is held by a process that started after the session's own `procStart` (+1 min grace) |
| `unknown`  | `ps` failed, or the record's `pidDomain` is another OS — never dead |
| `alive`    | everything else |

**The zone trap:** `procStart` has no zone. Claude Code 2.1.28x writes it in
UTC (`Fri Oct  2 06:53:54 2026`) while `ps` reports `lstart` in local time,
so at UTC+2 every live session's own process "started" two hours after its
record. The stamp is read as UTC and as local time and the LATER instant is
the bound: the zone can only make a verdict alive, never dead
(`TestLivenessIgnoresTheProcStartZone`).

State is the registry status, except a proven-dead process makes a busy or
waiting session `dead` (work was lost) and an idle or shell one `ended`.
Sessions sort waiting → dead → busy → unknown → idle → shell → ended, the
longest-waiting first within a state; projects sort by waiting, dead, busy.
The project is the repository root's name (`internal/transcripts.GitRoot`, a
linked worktree folds into its main checkout); `worktree` is the linked
worktree's path under `.worktrees/`.

## The ledger (lazarus)

One file per session, written only through `fleet ledger record`, under a
per-session `flock` (two tool calls can finish together) and replaced
atomically (temp file + rename). The event on stdin is exactly one JSON
object; unknown fields are refused:

| field         | |
|---------------|---|
| `kind`        | `workflow` · `shell` · `monitor` · `agent` · `limit-wait` · `park` |
| `id`          | the job's own id (task id, run id, agent id); jobs upsert by kind + id |
| `status`      | `started` (open) · `completed` · `failed` · `killed` · `stopped` |
| `at`          | RFC 3339; default now |
| `runId`, `scriptPath` | what `Workflow({scriptPath, resumeFromRunId})` needs |
| `description` | one line, cut to 120 characters |
| `command`     | input only: hashed to a 12-hex `digest`, **never stored** |
| `until`       | for `limit-wait`: when the limit resets |
| `cwd`         | input only: stored on the ledger, for the resume line |

Each record also stamps the ledger's `owner` (`pid`, `procStart`,
`pidDomain`) from the session's registry record, so a ledger that outlives
its registry file can still be proven dead by the same rule. A ledger keeps
at most 200 jobs; the oldest finished ones go first, open jobs never.

`fleet revive` lists (a) registry sessions proven dead while busy or
waiting, or with open jobs, and (b) ledgers with open jobs whose session has
no registry record and whose stamped owner is proven dead. A ledger without
an owner stamp, or whose owner's liveness is unknown, is never offered
(`LEDGER_UNPROVEN`, info). Each row carries `resume`:
`cd '<cwd>' && claude --resume <id>`. `fleet ledger close` marks a session's
open jobs `stopped` so revive stops offering them.

## The published summary

`vybava watch serve` publishes `~/.local/state/vybava/fleet/summary.json`
every 15 s (`FleetSummary`: counts plus the sessions waiting on you, oldest
first), atomically. Every session's fleet mod reads that one file for its
status line instead of running `fleet --json` on a timer — one producer, ~45
readers, no process spawned per session. Without the daemon the file goes
stale; a reader shows its `generatedAt` age rather than trusting old counts.

## Fleet.app's view: snapshot.json

`watch serve` also publishes `~/.local/state/vybava/fleet/snapshot.json`
(`Published`, `version` 1) for Fleet.app (claude-switcheroo `apps/fleet`):
Claude rows every 15 s and within a second of every cmux agent event that
can change who waits (`Notification`, `Stop`, `UserPromptSubmit`,
`SessionStart`, `SessionEnd`, `PermissionRequest`; a lost-events gap
republishes too), Codex rows from a once-a-minute ps/lsof read. One reader
machine-wide; the app only watches the file. `fleet publish [--codex]`
writes it once without the daemon.

- Only sessions a cmux surface hosts are listed (joined by live pid through
  `agent.resolve_delivery_target`); the rest are counted in `hidden`. Claude
  Code background sessions (registry `kind: bg`) run in a process no surface
  hosts — they are hidden, reachable through the interactive session showing
  them.
- Each row carries `actions`, decided here and rendered as-is by the app:
  `focus` + `screen` for every Claude row, `reply` too while it is `waiting`
  or `idle`; Codex rows `focus` only (no waiting signal).
- `cmux` is the `internal/cmux` status (`ok`, `unreachable`, `denied`,
  `restricted`, `outdated`, with `fix`). When cmux cannot be reached nothing
  is listed and the app shows that status — never an empty fleet.
- `surface` is for display; every action resolves the surface again.

## Acting on a session (Fleet.app's verbs)

All take `--session <id>` (a Claude session), resolve its surface by live
pid at call time — never from snapshot.json — and print a runx envelope.

| Verb | Does |
|---|---|
| `fleet screen [--lines n]` | the visible screen (or n lines of scrollback) with every secretscan shape redacted, plus the `dialog` open on it |
| `fleet dialog` | the open permission or AskUserQuestion dialog: `kind`, `title`, `question`, `detail`, `tabs`, `options` (`key`, `label`, `kind` answer/other/chat), `answerable`, `fingerprint` |
| `fleet reply` | text on stdin: pasted once and submitted. `--option <key> --expect <fingerprint>`: that one digit typed |
| `fleet focus` | cmux brought to the surface's window, workspace and surface, then the app activated; `--pid` addresses a Codex row |

`reply` refuses (`REPLY_*`, `DIALOG_*`, `NO_DIALOG`, `OPTION_INVALID`) rather
than guess:

- **Text while any dialog is open** — `DIALOG_OPEN`. Typed text can never
  approve a tool call; a dialog takes an option.
- **Text off the conversation's prompt** — `REPLY_REFUSED` unless the screen's
  bottom is the input box (a `❯` line between two rules). The agents view
  draws the same box, but text there starts a NEW background session.
- **An option for a dialog no longer shown** — `DIALOG_CHANGED` unless the
  fresh screen's fingerprint equals `--expect`. `Type something.` is never an
  answer; a multi-select list or an option past 9 is `answerable: false`.
- **A Codex target** — `REPLY_REFUSED`.
- A paste cmux accepted is never re-sent: `submitted: false` is
  `NOT_SUBMITTED`, the text is already at the prompt.

Dialogs are parsed from the screen (`dialog.go`): the footer `Esc to cancel`
must be among the last two non-blank lines, so an answered dialog in
scrollback never matches. Fixtures: `internal/fleet/testdata/screens/`.
Claude Code prints a conversation's title inside the input box's top rule;
a rule is "mostly dashes, starts and ends with one".

## JSON contract and the TypeScript copies

`fleet schema --ts` renders the contracts from the Go types by reflection:
`Envelope<T>` (with `v` pinned to the runx envelope version),
`FleetSnapshot`, `FleetSession`, `FleetCounts`, `FleetProject`, `CodexRow`,
`FleetRevive`, `ReviveSession`, `Ledger`, `LedgerOwner`, `LedgerJob`,
`LedgerEvent`, `FleetSummary`, `FleetSummarySession`, and the unions `FleetState`, `Liveness`, `ReviveSource`,
`JobKind`, `JobStatus`. Keys are camelCase; optional keys are `?`; times are
RFC 3339 strings.

A mod keeps a verbatim copy at `mods/<id>/types/fleet.gen.d.ts`:

```sh
go run ./cmd/vybava fleet schema --ts > mods/<id>/types/fleet.gen.d.ts
```

`TestModCopiesMatchTheGenerator` (internal/fleet) fails when any copy
differs from the generator, so a Go-side change cannot drift silently from
the mods; it skips while no mod carries a copy. Bump `LedgerVersion` on a
breaking change of the ledger file — a ledger of another version is refused,
never silently re-read.

## The Swift contract

`fleet schema --swift` renders `Published`, the action results and their
enums as `Codable, Sendable` Swift (`FleetSnapshot`, `FleetSession`,
`FleetCodexSession`, `FleetScreen`, `FleetDialog`, `FleetReply`,
`FleetFocus`, `FleetEnvelope<T>`, `CmuxStatus`, …) with `FleetJSON.decoder()`
for Go's RFC 3339 times. Fleet.app keeps it verbatim at
`apps/fleet/Sources/FleetSnapshot.swift`, and `fleet schema --example` — a
deterministic sample of every contract — at its test fixtures; the app's
`test.sh` fails when either differs from the installed `vybava`. Bump
`PublishedVersion` on a breaking change: the app refuses another version.

## Diagnostics

`REGISTRY_MISSING` · `REGISTRY_FILE_SKIPPED` · `LIVENESS_UNAVAILABLE` ·
`CODEX_UNAVAILABLE` · `CODEX_PARTIAL` · `LEDGER_UNREADABLE` (warnings on
reads) · `LEDGER_UNPROVEN` (info) · `REGISTRY_SHAPE_UNKNOWN` ·
`LEDGER_EVENT_INVALID` · `LEDGER_BUSY` · `SESSION_INVALID` ·
`SCHEMA_FORMAT` (errors, exit 2). Actions: `CMUX_UNAVAILABLE` ·
`SESSION_NOT_FOUND` · `SESSION_GONE` · `NOT_IN_CMUX` · `REPLY_INVALID` ·
`REPLY_REFUSED` · `DIALOG_OPEN` · `NO_DIALOG` · `DIALOG_CHANGED` ·
`OPTION_INVALID` · `NOT_SUBMITTED` · `SNAPSHOT_UNWRITABLE`. Defined in
`internal/fleet/diag.go`.
