# tokentime — Decisions

Epic: https://app.vitrinka.ai/w/fixit/p/claude-switcheroo/t/3202 (vt-3202, work packages 1–2) · 2026-09-23

## Summary

claude-switcheroo's Arcade needs per-project token spend from day one, including plain `claude` runs and both CLIs.
No ledger stored a cwd; Claude transcripts and Codex rollouts do. `tokentime` indexes both into permanent buckets
and rolls them up. It lives in Výbava because Výbava already owned two partial parsers and it is useful beyond
switcheroo.

## Decisions

| # | Decision | Call | Why |
|---|----------|------|-----|
| 1 | Home | A Výbava applet, not Go inside switcheroo or TS | switcheroo forbids a build step; Výbava already parsed both sources; one static low-memory binary |
| 2 | Shared reading | `internal/transcripts`: the operator cursor, Claude/Codex decoding, the tree walk, git-root resolution; operator and codexusage re-import it | A third parser of the same files would drift; `claudeguards/ctx.go` left alone (different job, safety-critical) |
| 3 | Storage | SQLite (modernc) hour × project × model buckets, permanent | Transcripts are deleted after `cleanupPeriodDays`; lifetime totals must not shrink |
| 4 | Dedup | A 64-bit hash of each response identity in a `seen` table, committed with the buckets and cursors | Forks, archived rollouts and replaced files re-present old responses; a per-file memory cannot see them |
| 5 | Seen retention | Claude ids 60 days; Codex ids forever | A Claude copy can only come from a live transcript; a Codex rollout can be archived (moved) at any age |
| 6 | Codex legacy vs receipts | Receipts are exact once a rollout writes one; before that, `token_count` with a changed total charges `last_token_usage` | Matches the codexusage rule; a token_count persisted before its receipt is recognised by equal usage |
| 7 | Project | Git root via `.git` → `gitdir` → `commondir`, per record | Worktrees live outside `.worktrees/` too; one session crosses the repo and several worktrees |
| 8 | Prices | Built-in table (Anthropic via the claude-api skill, OpenAI's pricing page, 2026-09-23) + `prices.json` override; unknown = unpriced, reported | Never guess money; standard, short-context rates |
| 9 | Moved checkouts | At rollup time, a dead root (gone from disk) folds into the one live root with its basename; zero or 2+ live namesakes → it stays its own project | Checkouts moved from ~/Documents/Work to ~/Work/Projects/<org>; folding at read time keeps the stored buckets untouched, so a wrong fold is undone by the next rollup, never baked in |

## Assumptions

- Claude's repeated content-block records carry identical usage (verified on three real sessions); the first wins.
- Buckets are UTC hours; a zone with a non-whole-hour offset splits them across local hours.
- OpenAI's long-context surcharge is not applied: per-request context size is not recorded.
- `gpt-5.6-sol` is priced at its promotional rate, published through 2026-11-21.

## Architecture notes

`index` walks both roots, skips unchanged files by cursor, reads the rest — files under a cursor first, then new files newest-first, so a budgeted cold start shows
today before history — in 64 MiB sweeps, aggregates
in memory and commits aggregates + identities + cursors per 64 MiB. `rollup` runs a bounded pass first (skipped
with `INDEX_BUSY` when another pass holds the lock) and aggregates the buckets in the local zone.
