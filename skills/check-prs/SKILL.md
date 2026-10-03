---
disable-model-invocation: true
name: check-prs
description: "Triage PRs: verdict, validity, danger, priority and a code/test/config/rest diff split."
argument-hint: "[deep] [targeting <base>] [from last week | since <date>] [by <login>] [merged | all] [<N>…]"
---

# check-prs — is each PR still worth keeping?

Read-only: never close, comment on, label, push to, rebase or merge a PR; never
modify a worktree; never `git stash`; never switch or restore the main checkout.

## 1. Census

Map argument words to `vybava gitkit pr-census --repo "$PWD"` flags:

| Words | Flag |
|---|---|
| `targeting <b>` / `into <b>` | `--base <b>` |
| `from last week` / `from last <N> days` / `since <date>` | `--since 7d` / `--since <N>d` / `--since <YYYY-MM-DD>` |
| `by <login>` / `mine` | `--author <login>` / `--author @me` |
| `merged` / `closed` / `all` | `--state merged\|closed\|all` (default `open`) |
| `<N>` / `#<N>` | PR numbers as positionals |
| `deep` | no flag — §2 |

Empty → every open PR. A word that maps to nothing → ask once.

Two census facts decide alone (contract: Výbava `docs/gitkit.md`):

- `merge.emptyMerge` or `merge.patchOnBase` → `CLOSE`, validity `superseded`, no agent.
- `activity.state: "active"` → an active PR: never judged or given an agent.

## 2. Judgment

Every other PR gets one analyst; up to three small PRs (under 300 code + config
lines) share one. Briefs, vocabulary and output schema: `references/judge.md`. A
skeptic then tries to refute only CLOSE and SALVAGE verdicts (`deep`: every
verdict); its corrected verdict replaces the analyst's.

Run the fan-out as one Workflow (invoking this skill opts in); without the Workflow
tool, parallel subagents. At most 4 agents per phase and 10 per run — beyond that,
more PRs per analyst, never a PR left out.

## 3. Report

Two tables: Verdicts names each PR by its bare full URL, Size and health by `#N`.

**Verdicts** — active PRs last, verdict cell `active — <holder>` (`activity.holders`:
kind, name, last event):

| PR | Title | Verdict | Validity | Danger | Priority |
|---|---|---|---|---|---|
| https://github.com/acme/app/pull/573 | fix(ssr): renderer binds loopback | MERGE | current | high: prod edge config | P0 |

**Size and health** — numbers straight from the census:

| PR | +/− | code | test | config | rest | behind / conflicts | CI | review | age |
|---|---|---|---|---|---|---|---|---|---|
| #573 | +59/−5 | +18/−5 | +41/−0 | — | — | 116 / 1 | ✓ | approved | 9d |

Then one block per judged PR: what it does (one sentence), its superseder if any,
unique value left, next step.

Close with ordering constraints between PRs and the offered actions — merge via
`prm`, a salvage PR, close with a pointer, park as draft. Ask which; do none unasked.
