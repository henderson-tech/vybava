# 0004 — What the context guards actually do in the field

Status: accepted

## Context

The context-budget guards were designed against synthetic fixtures, shipped
(#59, #60) and installed. Everything we believed about them came from the
bench. Installing them then exposed, within five minutes, that a trailing `)`
made every rule read the wrong command — and that the same gap had left a hard
destructive ban bypassable via `$(…)` since #52. That was found by using the
thing, not by testing it.

This record is the deliberate version of that accident: the guards measured
against our own transcripts. Corpus: 5,284 Claude Code transcripts, 9.4 GB,
spanning 2026-07-14 to 2026-09-14.

**The install boundary is `2026-09-10T19:50Z`**, not the date the brief
assumed. The graduating commit is `571dc70`; the first-ever context denial in
any transcript lands 12 s after it. `d49b618` (#60) and `2497c68` (#59) refine
rules that were already live for three days. Leakage check: 0 context firings
in 1,602 before-sessions, 310 in 171 of 282 after-sessions.

## What was measured

**875 real firings** across the corpus — 473 before the boundary, ~400 after
(the post count is 395 or 402 depending on whether the audit's own greps are
excluded). 562 sessions, 79 project slugs.

Post-boundary, by rule: `whole-file-dump` 170 · `unbounded-output` 115 ·
`inline-script-write` 61 · `secrets:env-dump` 12 · `browser:onyx-first` 11 ·
`simulator:host-input` 7 · `destructive:git-switch` 5 · `no-read` 4 ·
`commit-secrets` 3 · `git-stash` 2 · `transcript-dump` 2 · one each for
`proc-environ`, `inspect-config-env`, `heredoc-overwrite`.
`context:budget-read` has **never fired**.

**78% of firings are followed by the sanctioned alternative.** The escape hatch
was used 7 times in the entire post-boundary corpus, and 3 of those 7 were one
false positive (below). A guard people route around would show the opposite
shape; these do not.

## The verdict on whether it helped

Behaviour changed, causally and hard. Tool cost per response rose for four
weeks (444→478→493→632→698), dropped 33% **within the boundary day**
(698→470), and stayed down. Same direction in every project-matched cut; only
6 of 282 after-sessions are vybava, so the audit is not driving its own result.

| metric (before-near → after) | before med/p90 | after med/p90 | verdict |
|---|---:|---:|---|
| peak context, size-matched | 404,608 / 575,959 | 403,188 / 577,542 | **unchanged** |
| tool tokens / response | 644 / 1,400 | 458 / 1,105 | improved |
| largest single tool result | 5,448 / 8,790 | 4,342 / 7,554 | improved |
| heredocs / 100 cmds | 4.6 / 10.9 | 0.9 / 3.3 | improved |
| `cat >`, `tee >` / 100 cmds | 1.6 / 4.4 | 0.0 / 2.0 | improved |
| bare `cat <file>` / 100 cmds | 2.1 / 7.7 | 0.8 / 2.5 | improved |

Tail rates: sessions with a single result over 10k fell **5.5% → 0.4%**;
sessions with zero Edits despite 50+ Bash calls fell **35.0% → 5.3%**.

**And peak context did not move.** Size-matched, −0.4%. Sessions simply got
longer (responses +39%, shell commands +53%) with no attributable cause.

> The guards won the behaviour they targeted. It did not convert into the
> outcome they were built for. Anything proposed next on the promise of
> lowering peak context has to explain why it will not be absorbed the same
> way.

## The defect class the audit actually found

Every false positive but one traced to a single root: **the guard had no shared,
correct notion of what a command is.** Each rule family re-derived it.

- **Quoted text was scanned as executable.** `segmentSplit` was quote-blind, so
  any `;`/`|`/newline inside a literal split it. 18 of 25 rules could fire on
  text that can never run. `grep -nE "vault|env|path"` was read as a pipe into
  `env` — all 12 `env-dump` firings in the corpus are this. Worst case:
  `git commit -m 'fix: stop the crash; git stash was the cause'` could not be
  committed at all.
- **A `FOO=1` prefix disarmed 9 rules**, including hard bans. `FOO=1 env` was
  allowed. The prefix needed no value and stacked. This is the same shape as
  the `$(…)` bypass from #52: a ban defeated by typing five characters.
- **Only the first stage of a pipeline was inspected**, so a real terminal
  bound was invisible: `git show X | awk '{…}' | head -20` blocked.
- **`| sed -n 'A,Bp'` was not recognised as a cap** though the machinery to
  judge it already existed for sed-as-reader.
- **An inclusive range is off by one against how agents pick windows.**
  `sed -n 200,400p` is 201 lines against a 200-line budget: 84 firings —
  ~10% of the corpus — every one of them punishing an agent that had
  *complied*, and the source of 3 of the only 7 escape-hatch uses ever.

The fix is one shared normalisation layer (`splitShell`, `trimAssignments`,
`runnerPayloads` in `input.go`) that every rule family now goes through, not
five local patches.

**Quoting does not make text inert when it is a payload.** `ssh host 'env'` and
`docker exec c sh -c "env"` genuinely dump an environment. The first attempt at
"quoted text is data" broke the existing `ssh … docker exec … env` regression
test, which was protecting a real hazard. Runner payloads are recursed into;
everything else's quoted arguments are data.

## Where context actually goes now

Measured over the 33 largest post-boundary **lead** sessions. Agent runs were
unmeasurable when this was taken and are now half the fleet's tool-result
tokens (below), so treat every figure here as a floor for the system as a
whole.

| # | class | tokens | verdict |
|---:|---|---:|---|
| 1 | tool-call **inputs** (Bash 792k · Edit 303k · Write 241k) | 1,931,754 | inherent |
| 2 | Bash file reads `cat/sed/head/tail` | 916,276 | gateable, partly gated |
| 3 | **MCP results** | 813,898 | **not gated** |
| 4 | Bash `grep`/`rg` | 524,317 | gateable |
| 5 | images | 333,824 | inherent |
| 6 | instruction/reference re-reads via Bash | 286,667 | gateable |
| 7 | skill payload injection (`SKILL.md`) | 226,733 | gateable |
| 8–10 | task notifications · teammate messages · agent results | 409,084 | inherent |

**MCP is 26.1% of all tool-result tokens on 19.6% of calls.** vitrinka averages
1,002 tokens/call, 3× Bash's 330. The split that matters is by intent:
**write verbs — 316 calls, 311,845 tokens, 10.5% of their text already verbatim
in context; read verbs — 0.4%.** `hand_back` is the worst single verb at 2,423
tokens/call. The guards watch Bash and Read; they do not watch MCP at all.

## Settled — do not re-litigate

- **Tool-call inputs (31.7% of context) are inherent.** The command, the diff
  and the file body being written *are* the work. `ctx` does not account them,
  so no total will ever reconcile against `maxContext`; this is a known and
  accepted gap, not a bug to chase.
- **Images, task notifications, teammate messages and agent results are
  inherent.** They are what delegation and visual verification cost.
- **MCP *read* verbs are already lean** at 0.4% duplication. The problem is
  write verbs echoing the whole record back; that is an upstream API change,
  not a guard rule.
- **`/context` costs nothing** — 4 invocations, each a bodyless system record,
  0 model tokens. The brief assumed it was large. It is not.
- **`gh pr view --json` is not worth a rule** — all `gh pr` forms together are
  28,144 tokens across 174 calls.
- **`context:budget-read` has never fired.** Keep or retire it on design
  grounds; there is no field evidence either way.
- **Gate g1 is retired.** It replayed `f9ee8c4e` through the tiers, but that
  session started 38 minutes *after* the boundary — it was testing the guards
  against their own output. Replaced by **g1a** (synthetic fixture crossing
  50%/70% of a declared `maxContext`) and **g1b** (field regression: no
  post-boundary session above 25k in a single result).

## The half nobody was measuring

`ctx` could not read subagent or workflow transcripts at all — **1,738 of 4,370
files in the audit window (39.8%, 20% of bytes); 57.5% of files on the most
recent slice, because agent transcripts are a rising share.** `ResolveTranscript`
skipped any `subagents`/`workflows` directory unconditionally. That skip exists
for a real reason — otherwise `latest` resolves to whichever agent wrote last —
but it applied to explicit selectors too, so no selector could name an agent
run. The data was always on disk and agent rows parse identically; only
resolution was broken. Fixed by scoping the skip to `selector == "latest"`, and
`ContextReport.Kind` (`session|subagent|workflow-agent`) now exists so an
aggregator cannot average a lead session in with the agents it spawned.

It reported this honestly — `ok:false`, `TRANSCRIPT_UNAVAILABLE`, exit 2. The
plausible zero came from a collector reading `data:{}` through `jq … // 0` while
ignoring stderr and exit status. **The tool's contract was right and the harness
around it was wrong**, which is the more common shape of this mistake and the
reason `--json` consumers must check `ok` before reading `data`.

Measuring those files changes the audit's conclusion:

| tool-result tokens / day | before | after | |
|---|---:|---:|---|
| lead sessions | 3.99M | 4.00M | flat |
| agent runs | 1.76M | **4.01M** | **+128%** |
| total | 5.75M | **8.01M** | **+39%** |

Delegated share of all tool-result tokens: **31% → 50%.**

Agent runs *improved more* per run than lead sessions — tool tokens/response
−33%, largest single result −47%, and **peak context −16% median / −19% p90,
the only population whose peak actually fell.** Read migrated into them
(Read tokens/response p90 25 → 171).

> We optimised the smaller, flatter half of the system. Half of all
> tool-result tokens now live in transcripts that were unmeasurable until this
> fix, and that half is the one that is growing.

Honesty check: agent runs went from ~47 to ~135/day, but the pre-boundary
series was already volatile and climbing (Sep 09 hit 131, pre-boundary). Treat
delegation growth as correlation worth watching; the per-run improvements are
the part with a clean boundary.

## Accepted limits

**Two separate windowed reads are two reads.** `sed -n 200,400p f;
sed -n 400,600p f` delivers 402 lines and passes, because every budget in this
guard is per-segment — two 200-line windows always passed the same way. The
inclusive-endpoint allowance shifts one read's own ceiling by one line; it does
not make chained reads sum. Accepted: the alternative is cross-segment
accounting, which no rule here does and which would deny ordinary sequential
work.

**The +1 allowance applies to the 200-line default budget only**, never to the
100-line `budget-read` tier at 70% context. The arithmetic artifact is identical
at both, but `context:budget-read` fired 0 times in 395 audited firings, so there
is no field case at 100, and that tier is an emergency valve rather than the
working budget. Extending it would be a deliberate weakening of the only rule
that responds to context pressure, and would require editing the single test
that asserts it. Accepted asymmetry: evidence where the evidence is.

**`ctx` accounts no tool-call inputs.** No total will ever reconcile against
`maxContext`. Known and accepted, not a bug to chase.
