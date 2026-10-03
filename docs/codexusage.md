# codexusage — where the Codex plan limit went

An OpenAI plan limit can drain in an afternoon without anything feeling
unusual, because the expensive thing is invisible: not what you asked for, but
how much transcript each answer carried. `codexusage` reads the evidence Codex
already writes to disk and names the thread that spent the quota.

```sh
codexusage                  # today, one row per Codex process
codexusage --since 7am      # since you sat down
codexusage --since 3h       # the last three hours
codexusage --since week     # the whole quota window
codexusage --idle           # also list live threads that spent nothing
codexusage --top 0          # every thread, not just the top 12
codexusage --json           # stable output for agents and scripts
```

## What it reads

Codex writes one JSONL rollout per thread under `~/.codex/sessions` (and
`~/.codex/archived_sessions`). After every model call it appends a
`token_count` event carrying two things:

- `info.last_token_usage` — that call's tokens.
- `rate_limits.primary.used_percent` — the account's live quota percentage.

Reading them together is what makes the report possible. Thread titles come
from `~/.codex/session_index.jsonl`; live PIDs and terminals from `ps` and
`lsof`.

A long-lived caller passes an `Env.Cache`: each rollout's parse is kept
behind its `internal/transcripts` cursor, so a repeat `Run` reads only the
bytes appended since (a replaced or truncated file is re-read whole, a read
that fails part-way is forgotten). Samples are kept from the first window's
start and filtered per `Run`; a window that starts earlier re-reads the file.

Nothing is written, no network call is made, and no credential is read.

## The finding that drives the tool

**The quota meters total tokens, cache reads included.** This is not
documented; it was measured. Bucketing a day's spend into ten-minute slices and
regressing each slice against the reported percentage delta gives:

| Correlated against | Spread across buckets |
| --- | --- |
| total tokens (input + output) | **2.1–3.4M per point** — tight |
| fresh input only | 13k–88k per point — 6.8× |
| number of calls | 12–27 per point — 2.2× |

Only the total tracks. So a 98% cache-hit rate — which looks like thrift, and
genuinely saves latency and API-priced spend — buys **nothing** against a plan
limit. What you pay for is the size of the transcript on every call.

That is why the report leads with context-per-call, and why a resumed thread is
flagged: a thread carrying 150k of context bills 150k every single call, no
matter how small the question. A day where 148M tokens moved 2.8M tokens of new
information is a normal day on a resumed thread.

## Reading the report

```text
prolite    96%  ███████████████████░  resets in 6d 16h (Wed 16 Sep 11:00)
          +96pp in 4h07m · 2.47M/pp · ~246.69M allowance · 23.3 pp/h · empty in ~10m

534.96M tokens   524.00M cached (98.2%)   9.71M fresh   1.25M out   3806 calls

PID        TOKENS  %LIM  CALLS  CTX/CALL  THREAD                               WHERE
62273     313.44M    79   2303      136k  ↩ To be honest i have lost a track … ~/Work/Projects/ADF/powerflow
```

- **Allowance is derived, never published.** Codex reports a percentage, so the
  quota's size is a projection from observed spend in the same window. It
  sharpens as the window fills and is meaningless below a point or two of burn.
- **`%LIM` is charged per quota window.** Switching accounts mid-day means two
  windows with different allowances; a thread's share is computed against the
  window it actually spent in, so rows stay comparable and sum to the headline.
- **One row is one process, not one file.** Compacting rolls a thread onto a
  fresh rollout; both are the same terminal tab, so they merge and keep the
  original thread's name. The PID and TTY are there so an expensive tab can be
  found and closed.
- **`↩` means resumed** — the window opened on a transcript already under way.
  These are almost always the answer.
- **Runway** extrapolates the measured burn rate across the remaining
  percentage. It assumes the next hour looks like the last one.

## Accounting rules

Two traps, both of which invert the conclusion if you get them wrong:

- **`cached_input_tokens` is a subset of `input_tokens`, not a sibling.**
  Summing them double-counts every cache hit, and it inflates precisely the
  well-cached threads — so the lane that looks expensive is the wrong one.
  Fresh spend is `input - cached`.
- **A repeat `token_count` event is not a call.** Codex re-emits the event when
  only the rate limit refreshed, echoing the previous call's usage. Those are
  detected by an unchanged `total_token_usage` and recorded unbilled: their
  tokens are dropped, their fresher percentage is kept.

## Degradation

`ps`/`lsof` failures, an unreadable rollout, or a missing session index each
produce a `note:` on stderr and a report that still stands — the spend numbers
come from the rollout files alone. Live attribution is enrichment on top, never
a precondition, because the report is most needed exactly when a machine is
already misbehaving.
