# tokentime — where your AI tokens went

Screen Time for tokens: every Claude Code session and Codex thread on this
machine, rolled up by project, model, day and hour, with the API-equivalent
dollar value of what the subscriptions delivered.

```sh
vybava install tokentime
tokentime index                    # catch up (the first pass reads all history once)
tokentime rollup --json            # 90 days, 336 hours, projects, models, lifetime
tokentime rollup --json --days 14 --hours 48 --no-index
cd <repo> && tokentime project --from 2026-09-01 --to 2026-09-24 --json   # this repository
tokentime project --project FixIt --from 2026-09-01 --to 2026-09-24 --json  # any project, by its rollup name
tokentime beats --from 2026-09-01 --to 2026-09-30 --json   # minutes you prompted / agents answered, per project
tokentime limits --since 1790550000000 --json   # Codex rate-limit readings, one per response
tokentime status --json            # cursors, buckets, pending bytes, db size
tokentime prices --json            # the price table and its override file
tokentime prices --json --model "claude-opus-5-5[1m]"   # + how the rollup prices that name
```

`--state-dir` (default `~/.local/share/vybava/tokentime`), `--claude-root`
(default `~/.claude/projects`), `--codex-dir` (default `~/.codex`) and
`--side-dir` (repeatable, default `~/Exports` and `~/Backups`; see Beats)
apply to every verb. Every verb prints one runx envelope under `--json`.

## What it reads

- **Claude Code** — `~/.claude/projects/<slug>/<session>.jsonl`, subagents
  (`<session>/subagents/agent-*.jsonl`) and workflow agents
  (`<session>/subagents/workflows/<run>/agent-*.jsonl`). Assistant records carry
  `cwd`, `message.model` and `message.usage`. Workflow `journal.jsonl`,
  `memory/usage.jsonl` and `*.meta.json` are not transcripts.
- **Codex** — `~/.codex/sessions` and `archived_sessions` rollouts:
  `session_meta` (owner, cwd), `turn_context` (model), `token_usage_record`
  receipts (CLI 0.153+) and, for older files, `token_count` events. A recent
  CLI's `token_count` also carries the account's rate-limit reading
  (`rate_limits`), recorded as a limit point.

Nothing is written outside the state directory, no network call is made, no
credential and no message content is read beyond what decoding a line needs.

## Rules that keep the numbers honest

- **Components are disjoint.** Anthropic's input already excludes cache reads
  and writes. Codex's `cached_input_tokens` (and cache writes) are SUBSETS of its
  input and are subtracted before they land in `input`.
- **Every response counts once.** Claude repeats one message per content block
  with identical usage; the message id is counted once and remembered forever,
  like every Codex response id, so a fork, a replaced or restored file, or a
  re-read after a lost cursor can never re-count it. Codex receipts are counted when their `thread_id` is the
  rollout's owner (the first `session_meta`); copied fork history keeps the
  ancestor's id and is skipped. Once a rollout writes receipts, its
  `token_count` events are bookkeeping. In older rollouts an unchanged
  `total_token_usage` is a rate-limit refresh, not a call.
- **Projects are git repositories.** Each record's `cwd` resolves to its
  repository root: a linked worktree's `.git` file leads through `gitdir` and
  `commondir` to the main checkout, wherever the worktree lives. Exact answers
  are cached forever, because a removed worktree can no longer be resolved. When
  the whole checkout has moved or been deleted, a path inside `.worktrees/` or
  `.claude/worktrees/` still folds into the directory that held it. A directory
  outside any repository is its own project.
- **A moved checkout is one project.** At rollup time — stored buckets keep the
  root they were recorded under — a root that no longer exists on disk folds into
  the ONE live root sharing its basename (a checkout moved from
  `<old parent>/FixIt` to `<new parent>/FixIt` stays one FixIt). With no live namesake, or with two or more, the
  dead root stays its own project: a basename never picks between candidates. Display names are unique across
  every root ever indexed: among roots sharing a basename, the one still on disk
  (then the busiest) keeps it (`FixIt`); the others gain parent directories
  (`Work/FixIt`). `root` is the stable key.
- **Buckets are permanent.** Hour × project × model buckets outlive their
  sources — Claude Code deletes transcripts after `cleanupPeriodDays` — so
  lifetime totals never shrink. Cursors of vanished files are dropped —
  but only after a walk that saw every directory: a file the walk failed to see
  (unreadable directory, missing root) keeps its cursor.
- **Reads are incremental and bounded.** A per-file cursor (offset, size,
  mtime, 256-byte prefix digest) means an unchanged file is never opened, a
  partially written last record waits for its writer, and a replaced file is
  re-read from byte 0 (identities stop double counting). Records over 16 MiB are
  stepped over without being held. `index --budget` and `rollup --index-budget`
  (default 64 MiB) bound a pass; the next pass continues where it stopped. Files already under a
  cursor go first, then new files newest-first: a cold start under a budget
  shows today before it fills history. `coverage.complete` turns false only for
  complete records a budget left unread: a last record still unterminated after
  10 minutes (a writer that died mid-line) is reported as `STALE_TAIL`, a file
  that fails to read as `FILE_ERROR` — both retried every pass, neither counted
  as pending.
  Aggregates, identities and cursors commit together, in bounded chunks —
  every 16 MiB read (also between sweeps of one long file), 256 files or
  second — so a pass killed at any moment keeps everything before its last
  chunk and the next pass continues there. SIGTERM or SIGINT stops a pass
  cleanly: it commits what it read and exits with `INDEX_INTERRUPTED`.
- **A held lock never blocks a rollup.** One pass at a time holds
  `index.lock`; `rollup` finding it held skips its own pass and serves the
  store as committed, with `INDEX_BUSY`. Only a pass holding the lock
  creates or migrates the store: `rollup` and `status` open it without a
  single write, so a concurrent writer cannot stall them and they never run
  DDL beside it. A store an older binary wrote is served as long as its
  schema still carries what they read; an older one is `STALE_SCHEMA`
  (exit 2, next `tokentime index`), and the first pass the lock lets through
  migrates it. The rollup reads in one transaction: a pass committing
  mid-read cannot make one answer disagree with itself.

## The rollup

The JSON is a contract with claude-switcheroo's Arcade
(`src/arcade/contract.ts`, `TokentimeRollup`). Days and hours are local
(`timezone` names the IANA zone) and dense — an empty day is still listed.
An hour is a stored bucket, a whole UTC hour, so in a zone off the hour
(+05:30, +05:45) it starts at the half or three-quarter hour.
`projects` covers the requested days (tokens, usd, sessions, activeDays),
with lifetime `firstDay`/`lastDay`; `models` and `lifetime` cover everything.
A day's `sessions` counts the sessions active that day. `longestSessionMinutes`
is the longest run of consecutive active local hours of any single session — an
hour counts when that session had at least one response in it, an idle hour
breaks the run — × 60, attributed to the local day the run ended. An overnight
ten-hour run counts as 600 once, on its last day; a session resumed the next
day is two runs, never the gap between them.

`usd` is the API-equivalent value at list prices, never a bill: standard tier,
short-context rates. A model without a price row reports `UNPRICED_MODEL` and
is left out of every usd figure rather than guessed. Override or extend the
table in `<state-dir>/prices.json` — a map of model id to
`{input, output, cacheWrite5m, cacheWrite1h, cacheRead}` in USD per million
tokens. A row merges over the built-in one field by field, so overriding
one rate keeps the others; a row for a model the table does not know that
leaves a rate out is reported as `PRICE_INCOMPLETE` (those components price at
$0). Model names are compared without `[1m]`, a date suffix or `-latest`;
OpenAI's Daybreak aliases bill as the model behind them. `prices --model <name>` (repeatable)
adds `resolved`: each name as given → `{model, input, output, cacheWrite5m,
cacheWrite1h, cacheRead}` with the canonical model it prices as, or `null` when
no row does — so a caller never re-implements that matching.

## One project across a range

```sh
cd <repo> && tokentime project --from YYYY-MM-DD --to YYYY-MM-DD [--bucket hour|day|month] --json
tokentime project --project FixIt --from YYYY-MM-DD --to YYYY-MM-DD --json
```

A project is a git repository root: every worktree of it, and every moved
checkout of it, folds in. `project` is the detail behind one rollup project (the
Arcade's expanded project row). It never runs an index pass and never takes
the lock: it serves the store as committed, so it answers at once even while a
pass is running — `rollup` or `index` is what brings the store up to date.

- **Read-only.** `tokentime.db` is opened SQLite `mode=ro`: the state
  directory and database are never created, no schema is written or
  migrated, the database file never changes. It is not opened `immutable`,
  because a pass may be committing, so SQLite may leave its `-wal`/`-shm`
  coordination files beside it, as for any WAL reader. A store never indexed
  exits 2 with `NO_STORE`; one an older binary wrote exits 2 with
  `STALE_SCHEMA` — one `tokentime index` migrates it. Every figure comes
  from one read transaction, one snapshot of the store.

- **Which project.** With no flag it is the repository the current directory
  is in, resolved by the indexer's own rule (a linked worktree anywhere on disk
  is its repository); a directory outside every repository exits 2 with
  `BAD_FLAG` before the store is opened. `--project <name>` takes a name as the
  rollup shows it (`FixIt`, `ADF/forge`, `unknown`) — exactly, else ignoring
  case when that picks one project; a name no project carries exits 2 with
  `UNKNOWN_PROJECT` naming the closest ones (every basename, plus its own
  basename's qualified names — read without summing a bucket), and one
  several carry lists them with their roots. `--root` is a root as the rollup reports it (what the
  Arcade passes); `--root ""` is the rollup's `unknown` project (responses
  recorded without a cwd). A dead root folding into a project is part of it,
  and a folded dead root given resolves to its live project. A root never
  indexed exits 2 with `UNKNOWN_PROJECT`; `--root` with `--project` is
  `BAD_FLAG`.
- `from`/`to` are inclusive local days; an hour bucket belongs to the day it
  starts in, as in the rollup. A day whose midnight a DST jump skips
  (America/Santiago, America/Havana) starts at the jump. Both days lie in
  2000-01-01..2100-12-31, and a range is capped per bucket, because its
  series is allocated whole: at most 31 days by `hour`, 1100 days by `day`,
  1200 months by `month` (a month the range touches counts). A bad day,
  range or bucket, or a range past its cap, exits 2 with `BAD_FLAG` before
  the store is opened.
- `tokens`, `usd`, `responses` and `models` (most tokens first) cover the
  range under the rollup's rules — disjoint components, an unpriced model left
  out of every usd figure and reported as `UNPRICED_MODEL`. `sessions` counts
  sessions with a response in this project inside the range; `activeDays`
  counts days with tokens, like the rollup's. `longestRunMinutes` is the
  longest-session rule restricted to this project's hours of each session and
  to runs ending inside the range (counted whole, even when they began before
  it). `peakHour` is the local hour of day with the most tokens, `null` for an
  empty range. `firstDay`/`lastDay` are lifetime.
- `series` is dense and oldest first: one entry per bucket, `start` as a local
  RFC 3339 time with its offset, `models` as per-model tokens (`[]` when the
  bucket is empty). The bucket defaults to hour for a single day, month past
  62 days, day otherwise. Hours are the stored buckets, whole UTC hours, so at
  +05:30 a day's entries start 00:30, 01:30, … 23:30, each labelled with the
  start of the bucket it counts. They are absolute — a fall-back day has 25
  entries, its repeated 02:00 told apart by the offset; the first month entry
  starts at `from`, every later one on the 1st.

## Beats — the minutes you and your agents were at work

```sh
tokentime beats --from YYYY-MM-DD --to YYYY-MM-DD --json
```

Tokens say how much work went where; beats say *when*, at minute
resolution, in two kinds. The JSON is a contract with claude-switcheroo's
timesheet (`src/timesheet/contract.ts`, `TokentimeBeats`), which turns
them into hours: your attention and agent time, combined per client.

- **human** — a minute you typed a prompt in. Claude Code marks the prompts
  a person typed with `origin.kind: "human"`; a task notification, a
  subagent's brief (`isSidechain`) and a tool result never count. In Codex it
  is a completed `UserMessage` item (older rollouts: a `user_message` event)
  in a thread a person drives: the owner's `session_meta` has a string
  `source` other than `exec` and is not from `codex_exec`. A spawned thread
  (an object `source`: subagent, guardian) and `codex exec` are agents, and a
  header naming no source is unknown — never a person.
  Copied fork history, older than the thread, is never a new prompt.
- **ai** — a minute an agent answered in: the minute of each response the
  rollup charges, from any session — subagents, workflows, headless runs
  alike — recorded where it is charged, so a copy of a charged response adds
  no minute, just as it adds no tokens. The backlog below cannot tell a copy
  from its original (all of them were charged before beats existed): it
  counts a message's repeated blocks once and copied fork history not at all,
  and a copy keeping its record's time and cwd lands on the original's minute.

**Where an AI minute goes: the repository the agent wrote in.** Agents reach
sibling repositories by absolute path while every record keeps the cwd the
session started in, so each transcript (main, subagent, workflow agent,
rollout) keeps a *focus*: the record cwd's repository until a write lands in
another one. A write is an Edit/Write/MultiEdit/NotebookEdit of a file; a
Bash segment a `cd <dir>` (in a `( … )` too, and taken as applied:
`cd <dir>; make` runs there), `git -C <dir>` or `--cwd <dir>` put somewhere
— the cwd too, so `cd <cwd> && git commit` brings the focus home, while a
bare command in the cwd says nothing; where it runs is claude-guards'
`RunDirs`, relative directories against the record cwd — unless the segment
only inspects (`ls cat head tail less rg grep find fd wc jq stat file tree du
pwd which echo printf true awk`, `sed -n`, git `log show status diff blame
rev-parse ls-files grep fetch`, `branch --list`, `remote -v`, `config --get`);
the file any output redirect names (`git diff > /tmp/x` writes in no
repository); an MCP tool's `cwd` (onyx
`run_command`, its argv judged the same way); a Codex `exec_command`/`shell`
`workdir` (same rule on its command) and the files an `apply_patch` adds,
updates, deletes or moves to. Reads (Read, Glob, Grep, WebFetch) never move
it, and only tool inputs are parsed, never message text. A write outside every
repository (`/tmp`), into the agents' own state (`~/.claude` — plans,
memory, handoffs — and `~/.codex`) or into a side directory moves nothing.
Side directories hold work products no project owns — `--side-dir`,
repeatable, default `~/Exports` and `~/Backups`: a client deliverable written
there is the session's project's work, and a root of its own would drop it
out of every client's hours. A person's prompt, or a
record cwd in another repository, sets the focus back to the cwd's.

A response's minute goes to every repository its own tool calls wrote in, else
to the focus. A Claude message's tool calls arrive in later records under the
same id, so its minute waits — in the file's saved state, across passes —
until the next message or prompt starts, or the message is 10 minutes old.
Human minutes, tokens, the rollup and `project` keep the record cwd: a
repository only focus minutes reached is a `beats` project but never a token
project (`project` answers `UNKNOWN_PROJECT`), and it never changes how the
token projects fold or are named.

Beats are stored per minute × project × kind and kept forever, like
buckets, so they outlive transcript cleanup. A project is the rollup's:
worktrees fold into their repository, moved checkouts into their live
namesake. Each project lists `human` and `ai` as runs of consecutive
minutes, `[startUnixMinute, minutes]` (unix seconds / 60), oldest first;
projects are sorted by root, and a project with no beat in the range is
left out. `from`/`to` are inclusive local days, at most 92 of them;
`timezone` names the zone they were read in.

- **Read-only**, like `project`: no index pass, no lock, `mode=ro`; a
  store never indexed is `NO_STORE`, one without beats (schema 3 or older)
  `STALE_SCHEMA` until one `tokentime index` migrates it. A bad or missing
  day, or a range past 92 days, is `BAD_FLAG` before the store is opened.
- **Coverage.** `coverage.from` is the first local day whose beats are
  complete: the day after the oldest Claude transcript still on disk when
  beats began (sessions that ended before it were deleted unread), never
  before the first beat. Rollouts are not cleaned up, so they do not bound it.
  A file that vanishes, or shrinks below what it owes, before its backlog is
  paid takes its unread beats with it: coverage moves to the day after the last write the index saw
  of the lost content (never the rewrite, which for a live transcript is today) rather than
  claiming days it cannot vouch for. The debt records that write when it opens, so the bound
  holds however many passes later the loss is found. A store without beats has `null`. `pendingBytes` is what the index still
  owes beats: unread records plus the backlog below; `complete` is false
  while any is.
- **The backlog.** A store indexed before beats existed has read its files
  without them. Every file owes beats for the bytes read before
  (`files.beats`), and each pass pays it after its token reads, from the
  budget they left — newest files first, so recent weeks fill before older
  history. The backlog reads beats only: it neither charges a token nor
  touches the `seen` identities, so no total can move. Everything a pass
  reads from then on records its beats as it goes — except where a token
  read re-reads a file that still owes (replaced, or re-read after its
  cursor was lost): its responses are seen, so the debt grows to cover what
  that read covered, and a backlog that finds the file replaced reads the
  new content from byte 0 and ends where that content does. `index --json` reports what is left as
  `beatsPendingBytes`.
- **The focus re-read.** A store indexed before AI minutes followed the
  focus (schema 5 or older) filed them by cwd. Every file on disk owes a
  re-read of the bytes read before (`files.focus`), paid like the backlog
  above — after it, before the points backlog, newest files first, charging
  nothing — into staging tables (`focus_beats`, `focus_found`); while it
  runs, every pass stages the minutes it records too. Beats keep no
  provenance, so a staged day replaces a stored one only when it can be
  vouched for: every response charged on that UTC day (its permanent `seen`
  identity) was found again, on that day, by a read that staged its minute.
  Then no deleted transcript holds a minute of that day the re-read lacks, and
  the day's AI minutes are swapped for the staged ones in the commit that
  re-reads the last file (meta `focus_rule`). A day a deleted transcript
  answered in keeps every AI minute it had, by cwd; beats never shrink with a
  transcript. Until the swap the re-read counts in `beatsPendingBytes`, so
  beats coverage stays incomplete. While it runs, a response a token read
  finds already seen — a transcript replaced or re-read, a rollout archived
  before its re-read — is staged and found like its original (it still adds
  no minute to beats); a transcript that shrinks or vanishes — even during
  the pass re-reading it — leaves its responses unfound and their days as
  they were. A Claude message still waiting for its tool calls when the
  re-read settles counts as found (its minute lands once it is whole), so a
  pass from a live session swaps today too. One unbudgeted `tokentime index`
  pays it in a single pass, reading every transcript on disk once more —
  minutes over months of history, longer than claude-switcheroo's 180 s
  `index` timeout: run it once by hand after upgrading, before the next
  Timesheet run (a pass the timeout kills keeps its progress, and the next
  one finishes).
- **Migrations** add one column each, and only when it is missing: a pass
  killed between a migration and the version bump leaves the column behind,
  and the next pass finishes the migration rather than failing on it.

## Limits — Codex rate-limit readings, one per response

```sh
tokentime limits --since <unix-ms> --json
```

Every Codex `token_count` carrying `rate_limits` with at least one window is a
limit point: what the account's windows read right after a response, and the
call that response was. The JSON is a contract with claude-switcheroo's Arcade
accounts:

```json
{ "points": [ { "ts": 1790549822427, "thread": "01a0e2a5-…", "plan": "pro", "model": "gpt-6-astra",
                "input": 120499, "cached": 114560, "cacheWrite": 0, "output": 996, "reasoning": 127,
                "windows": [ { "minutes": 10080, "resetsAt": 1791058036, "pct": 49.0 } ] } ],
  "cursor": 1790549822427,
  "backfill": { "done": true, "pendingBytes": 0 } }
```

- **Fields.** `ts` is unix milliseconds, `thread` the rollout's owner (its
  first `session_meta`), `plan` the reading's `plan_type`, `model` the last
  `turn_context` model (`codex-unknown` before one). The counts are
  `last_token_usage` as Codex writes it: **`input` INCLUDES `cached` and
  `cacheWrite`**, and `reasoning` is part of `output` — unlike the rollup,
  which splits them apart. `windows` are `primary` then `secondary` (some plans
  report a 5-hour window beside the week): length in minutes, `resetsAt` in
  unix seconds, `pct` used.
- **Refreshes keep their reading.** A `token_count` with a null `info`, or one
  repeating the previous `total_token_usage`, is a rate-limit refresh, not a
  call: it is a point with every count 0. So are counters the charge rejects.
  Rollouts older than the readings record none.
- **Each reading once.** Copied fork history (older than its thread) belongs
  to the ancestor that recorded it. A point's identity is its thread, `ts`,
  `total_token_usage` and windows, so an archived copy of a rollout, a re-read
  after a lost cursor or the backlog meeting what a token read recorded is kept
  once. Points are permanent, like buckets.
- **The read.** Points strictly after `--since` (default 0: all of them),
  oldest first; `cursor` is the newest `ts` returned, or `--since` when there
  is none. Read-only like `beats`: no index pass, no lock, `mode=ro`; `NO_STORE`
  before any pass, `STALE_SCHEMA` for a store without points (schema 4 or
  older) until one `tokentime index` migrates it, `BAD_FLAG` for a negative
  `--since`. A cursor is not a watermark: a later pass can still add points
  older than it — the backlog below, and records a budget left unread (see
  `status` `pendingBytes`) — so resume with some overlap while either is owed.
- **The backlog.** A store indexed before points existed has read its
  rollouts without them. Each rollout owes the bytes read before
  (`files.points`, fixed at the cursor the first pass after the upgrade
  starts from; the token read records everything after it), and each pass
  pays it after the token reads and the beats backlog, from the budget they
  left — newest rollouts first. It reads points only: no token charged, no
  beat, no `seen` identity, and it never touches `files.beats` or beats
  coverage. A rollout that vanishes or shrinks under its debt takes the owed
  points with it; nothing moves. `index --json` and `status --json` report the
  rest as `pointsPendingBytes` (and the beats backlog as `beatsPendingBytes`),
  and name `tokentime index` next while either is owed (`index` adds a
  `BACKLOG_PENDING` info diagnostic); `backfill` is the last completed pass's figure,
  `done` once nothing is owed.
