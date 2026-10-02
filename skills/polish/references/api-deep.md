# API deep pass - the `full` pass for the `api` target

Runs after tiers 5, 6 and 7 of `matrix.md` are green. Every row is a cell
in the pass ledger (`polish-kit run add-cell --kind matrix --lane api …`).

## 1. Every mutation, twice at once

For each endpoint the feature writes through:

| Row | Provoke | Correct |
|---|---|---|
| Same request in parallel | the identical body with the same idempotency key, `xargs -P2` | exactly one effect, both answers agree |
| Two actors, one record | a write from the primary user and one from a second session on the same row | no lost update; a conflict is surfaced with a body, never a 500 |
| Retry after a cut | `blip db cut` mid-request, then the client's retry | one effect; the partial transaction rolled back |
| Job or cron overlap | trigger the feature's job twice concurrently | a lock or an idempotent second run; the log names which |

## 2. A log read after every fault

After each tier 5 and 6 cell, read the server log for the request id the
cell produced (the repo's log tool: FixIt has `fixit-api-log-debugger` for a
local API and `fixit-deployed-logs` for a deployment). The cell passes only
when the log names the cause in one line with its context, never a bare
stack trace or a swallowed error.

## 3. The journey suite grows with the fixes

Each fixed cell adds one case to the nearest real-HTTP journey suite (FixIt:
the `*.super.spec.ts` beside the controller), from the user's perspective,
including the adversarial probe that found it. A mocked-repository unit test
never counts as the sole coverage of a path that reaches the database.

## 4. Data shape after the pass

- Every table the feature writes carries its owning tenant or user column
  and the app filters on it, or row-level security is on.
- No column holds two facts; a second wire name for one fact is marked
  deprecated and names the canonical.
- A migration that ships with the fix is expand-only; the contract step is
  filed for the release after.
