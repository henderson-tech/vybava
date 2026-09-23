# tokentime — where your AI tokens went

Screen Time for tokens: every Claude Code session and Codex thread on this
machine, rolled up by project, model, day and hour, with the API-equivalent
dollar value of what the subscriptions delivered.

```sh
vybava install tokentime
tokentime index                    # catch up (the first pass reads all history once)
tokentime rollup --json            # 90 days, 336 hours, projects, models, lifetime
tokentime rollup --json --days 14 --hours 48 --no-index
tokentime status --json            # cursors, buckets, pending bytes, db size
tokentime prices --json            # the price table and its override file
```

`--state-dir` (default `~/.local/share/vybava/tokentime`), `--claude-root`
(default `~/.claude/projects`) and `--codex-dir` (default `~/.codex`) apply to
every verb. Every verb prints one runx envelope under `--json`.

## What it reads

- **Claude Code** — `~/.claude/projects/<slug>/<session>.jsonl`, subagents
  (`<session>/subagents/agent-*.jsonl`) and workflow agents
  (`<session>/subagents/workflows/<run>/agent-*.jsonl`). Assistant records carry
  `cwd`, `message.model` and `message.usage`. Workflow `journal.jsonl`,
  `memory/usage.jsonl` and `*.meta.json` are not transcripts.
- **Codex** — `~/.codex/sessions` and `archived_sessions` rollouts:
  `session_meta` (owner, cwd), `turn_context` (model), `token_usage_record`
  receipts (CLI 0.153+) and, for older files, `token_count` events.

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
  the ONE live root sharing its basename (`~/Documents/Work/FixIt` into
  `~/Work/Projects/Org/FixIt`). With no live namesake, or with two or more, the
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
  Aggregates, identities and cursors commit in one transaction.

## The rollup

The JSON is a contract with claude-switcheroo's Arcade
(`src/arcade/contract.ts`, `TokentimeRollup`). Days and hours are local
(`timezone` names the IANA zone) and dense — an empty day is still listed.
`projects` covers the requested days (tokens, usd, sessions, activeDays),
with lifetime `firstDay`/`lastDay`; `models` and `lifetime` cover everything.
A day's `sessions` counts the sessions active that day; `longestSessionMinutes`
is the longest session that ENDED that day, measured over its whole first→last
span — an overnight ten-hour session counts as ten hours, once, on its last day.

`usd` is the API-equivalent value at list prices, never a bill: standard tier,
short-context rates. A model without a price row reports `UNPRICED_MODEL` and
is left out of every usd figure rather than guessed. Override or extend the
table in `<state-dir>/prices.json` — a map of model id to
`{input, output, cacheWrite5m, cacheWrite1h, cacheRead}` in USD per million
tokens. A row merges over the built-in one field by field, so overriding
one rate keeps the others; a row for a model the table does not know that
leaves a rate out is reported as `PRICE_INCOMPLETE` (those components price at
$0). Model names are compared without `[1m]`, a date suffix or `-latest`;
OpenAI's Daybreak aliases bill as the model behind them.
