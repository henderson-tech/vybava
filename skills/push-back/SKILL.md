---
disable-model-invocation: true
name: push-back
description: "Verify a claim against code, docs and project rules before acting; validate an audit or findings doc."
---

# Push Back

A claim — from a document, a reviewer, a previous turn or the user — is verified
against the code, the stack's Tier-1 docs and the project's rules before it is
executed. Better way exists → say so with citations. Claim is wrong → refuse with
evidence.

## Two modes

- **Mode A — document validation.** A path to a findings / audit / handoff /
  recommendations doc, or "validate findings" / "review the audit" / "is this really
  an issue". Verify every finding and produce the validation report before any fix
  is touched; execution is a separate, gated phase.
- **Mode B — in-conversation pushback.** "push back" / "challenge this" / "is this
  the best way" / "don't just agree", or a project tripwire fires — not every turn.
  Verify the single claim, present the verdict, and **wait**.

## Project gates

Read `<repo>/.claude/push-back.md` first when present: the project's pressure-test
gates, Mode B tripwires and Tier-1 doc set. Absent → the repo's CLAUDE.md rules and
`.claude/memory/` are the gates. Universal gates, always:

| Gate | Question |
|---|---|
| **Platform already handles it** | Does the fix hand-roll what the compiler, runtime or framework does for free? |
| **Dev-only artifact** | Does the bug exist outside dev mode (strict-mode double-invoke, hot reload, debug builds)? |
| **Existing primitive** | Does the stack or the repo already ship this — search before build. |
| **Cost vs. churn** | Even if true, is fixing cheaper than the churn? Quantify. |
| **Regresses a declined finding or an accepted rule** | → automatic DECLINE. |

## Verification

1. **Source** — open the cited files at the cited lines ±20. Does the code do what
   the claim says? Are the line numbers and the "N consumers / K call sites" counts
   real? Grep to confirm. Moved or refactored → **STALE**, stop here.
2. **Rules** — does CLAUDE.md, a `.claude/memory/` row or the project gates file
   contradict the claim or forbid the proposed fix?
3. **Gates** — the project's and the universal ones; note which applied.
4. **Contrarian check (mandatory)** — one sentence: *"What's the best argument this
   claim is wrong?"* Empty → solid. Has teeth → downgrade or propose the better path.
5. **Verdict** — **VALID** (verified, idiomatic) · **VALID, BETTER FIX** (verified,
   but the stack has a better primitive — cite it) · **PARTIAL** (part holds —
   narrow the scope) · **STALE** (code moved since) · **INVALID** (factually wrong)
   · **DECLINE** (real but not worth it: platform handles it, dev-only, cost >
   churn, regresses a declined rule) · **DEFER** (real but needs product/design
   input).
6. **Present:**

```
Verdict: <one of the seven>

Why (≤6 lines, file:line citations + Tier-1 links where overruling):
- …

Contrarian check: <one sentence>

Recommended action: <apply as-is | apply modified fix | skip | defer>

<if BETTER FIX or modified: 5-15 line diff or snippet>
```

Cite Tier-1 (the stack's official docs, named in the project gates file) when
overruling; Tier-2 blog posts are supporting evidence only.

## Mode A specifics

Read the findings doc, the closest CLAUDE.md and the referenced memories. Parse into
a flat list (markers typically `### C1`, `### H1`, `### Hyg1`, `### U1`, `## Finding
N`): ID, title, cited files+lines, claim, recommended fix, flagged-by, effort.
Skip anything under "DECLINED" / "dropped" / "deferred" unless the user reopens
it. `git log --since="<doc date>" --stat` spots churn that may have superseded
findings. Confirm scope ("N items, K pre-declined, validating N-K") before starting.

One finding per `AskUserQuestion` in the document's execution order (Critical →
Correctness → Hygiene → Known-unknown): accept verdict & action (recommended) /
override / dig deeper via `Explore` / park for batch review. Parked items resolve
together at the end.

Report → `.claude/work/YYYY-MM-DD-<source-doc-basename>-validation.md`:

```markdown
# <Source doc title> — Validation

**Source:** `<path>` · **Validated:** <date> · **Findings:** N total (K pre-declined)

## Verdict summary
| ID | Title | Verdict | Disposition | Effort |

## Per-finding detail
### <ID> — <title>
**Verdict / Disposition / Original claim / Verification (files read, what was checked)**
**Better fix (if applicable):** <code block>
**Effort / verification plan**

## Suggested execution order
## Open items (DEFER)
```

Then ask: proceed with accepted items now, or hand off? Proceeding → the
`receiving-code-review` skill works the accepted list, one commit per item,
`Report: <validation-report-path>` in each footer.

## When NOT to push back

- One-line typo / translation fixes.
- A project rule the user explicitly invoked.
- Findings docs with <3 items — triage inline.
- Pure aesthetic preferences the user has authority over.
- Trade-offs the user already accepted after a prior pushback.
