---
disable-model-invocation: true
name: push-back
description: Verify a claim against code+docs+memory before acting. Run when a finding/audit/recommendation looks suspect, or proactively when something conflicts with a project rule.
---

# Push Back

**Default answer is not "yes, sir."** When a claim enters the conversation — from a document, from a previous turn, or from the user right now — don't execute it until it's verified against the actual code, the relevant Tier-1 docs, and this project's rules. Better way exists → say so with citations. Claim is wrong → refuse with evidence.

**No claim is oracle.** Auditors cite stale line numbers. Earlier-you misread the code. The user asks for the suboptimal thing because they don't know about a better primitive. And politeness ≠ usefulness: a silent "yes" that ships a regression is worse than "hold on — here's why this is wrong." Receipts, not vibes.

## Two modes

- **Mode A — document validation.** Triggered by a path to a findings / audit / handoff / recommendations doc, or "validate findings" / "review the audit" / "is this really an issue". Verify every finding, then produce a validation report *before* any fix is touched. Validation is read-only; execution is a separate, gated phase.
- **Mode B — in-conversation pushback.** Triggered by "push back" / "challenge this" / "is this the best way" / "don't just agree", or proactively when a tripwire below fires. Stop, verify the single claim, present the verdict, and **wait** — don't silently proceed with your preferred version.

Both run the same verification.

## Verification

1. **Source** — open the cited files at the cited lines ±20. Does the code do what the claim says? Are the line numbers and the "N consumers / K call sites" counts real? Grep to confirm. Moved or refactored → **STALE**, stop here.
2. **Project memory + CLAUDE.md** — does a `project_*.md` memory, a root/`apps/client`/`apps/api` CLAUDE.md rule, a `feedback_*.md` file, or the `MEMORY.md` index contradict the claim or forbid the proposed fix?
3. **Pressure-test** against the gates below; note which you checked and which didn't apply.
4. **Contrarian check (mandatory)** — write one sentence answering *"What's the best argument this claim is wrong?"* Empty after 1–3 → the claim is solid. Has teeth → downgrade the verdict or propose the better path. This is the highest-value step; it catches stale claims and over-engineered fixes faster than anything else.
5. **Verdict** — one of: **VALID** (verified, idiomatic, proceed) · **VALID, BETTER FIX** (verified, but a better Expo/RN primitive exists — propose with citation) · **PARTIAL** (part holds, part is stale/wrong — narrow the scope) · **STALE** (code moved/refactored/fixed since) · **INVALID** (factually wrong) · **DECLINE** (real but not worth it: React Compiler handles it, Strict-Mode-only artifact, cost > churn, regresses a declined rule) · **DEFER** (real but needs product/design input).
6. **Present:**

```
Verdict: <one of the seven>

Why (≤6 lines, file:line citations + Tier-1 links where overruling):
- …

Contrarian check: <one sentence>

Recommended action: <apply as-is | apply modified fix | skip | defer>

<if BETTER FIX or modified: 5-15 line diff or snippet>
```

## Pressure-test gates

| # | Gate | Question |
|---|------|----------|
| 1 | **React Compiler enabled?** (`app.config.js` / babel) | If yes, does the fix manually memoize what the compiler already handles? Redundant. |
| 2 | **Zustand selector shape** | Whole-store `useStore()` → almost always wrong. 9 atomic selectors where a stable object is wanted → use `useShallow`. Derived state → `createSelector` or a memo inside the selector. |
| 3 | **New Arch + Hermes + MMKV Nitro** | Is the fix fighting a JS-thread cost that doesn't exist? Re-rendering a 5-leaf tree every 10s under React Compiler is near-free. |
| 4 | **Strict Mode double-invoke** | Is the bug dev-only? `useRef(true)` cleanup-flip is an anti-pattern; effect-scoped `let active = true` is idiomatic. |
| 5 | **Expo Router layout ownership** | Data loads live in layouts, not stores (`feedback_switchProfile_race.md`). |
| 6 | **Reanimated / gesture shared-value writes during render** | That's the real bug class — re-render counts usually aren't. |
| 7 | **MMKV persist / partialize hygiene** | Does the fix persist transient state (sockets, promises, mid-flight timestamps)? |
| 8 | **Socket lifecycle** | `SocketManager` owns sockets; providers subscribe, never `io()`. |
| 9 | **Native-first UI** | `react-native-maps` over Mapbox for interactive maps (`feedback_prefer_native_maps.md`); `@/ui` compounds over ad-hoc; native headers; bottom glass pill over `BottomActionFooter`. |
| 10 | **Orval-generated types** | Never hand-type an API response. Hook missing from `packages/api-client/generated/*` → the API side isn't shipped. STOP. |
| 11 | **Platform default locale is Czech** | `LOCALE=cs`. New user-visible strings need cs.json coverage. |
| 12 | **Enum values UPPERCASE** | `OfferStatus.PENDING`, never `'pending'`. |
| 13 | **Cost vs. churn** | Even if technically true — is fixing cheaper than the churn? Quantify. |
| 14 | **Regresses a declined finding or a `feedback_*.md` rule?** | → automatic DECLINE. |

Cite Tier-1 when overruling: react.dev, zustand docs, reactnative.dev, docs.expo.dev, TanStack Query docs. Tier-2 (tkdodo.eu, Shopify RN perf, Callstack) is supporting evidence only.

## Mode A specifics

Read the findings doc and the closest CLAUDE.md + referenced memories. Parse into a flat finding list (markers are typically `### C1`, `### H1`, `### Hyg1`, `### U1`, `## Finding N`), capturing ID, title, cited files+lines, claim, recommended fix, flagged-by, effort. **Respect pre-declined findings** — anything under "Dispositions DECLINED" / "dropped" / "deferred" is skipped unless the user re-opens it. Run `git log --since="<doc date>" --stat` to spot churn that may have superseded findings. Confirm scope ("N items, K pre-declined, validating N-K") before starting.

Then one finding per `AskUserQuestion` in the document's own execution order (Critical → Correctness → Hygiene → Known-unknown), options = accept verdict & action (recommended) / override with user disposition / dig deeper via `Explore` / park for batch review. Parked items get resolved together at the end.

Report → `.claude/work/YYYY-MM-DD-<source-doc-basename>-validation.md`:

```markdown
# <Source doc title> — Validation

**Source:** `<path>`
**Validated:** <date>
**Findings:** N total (K pre-declined, N-K validated)

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

Then ask: proceed with accepted items now, or hand off? Proceeding → fixes in execution order, one commit per critical/correctness item, one batched commit for hygiene (`feedback_commit_frequency.md`), each with `Report: <validation-report-path>` in the footer.

## Mode B tripwires

Scan every user request. Any tick → verify before writing code.

- [ ] Violates a `feedback_*.md` rule, a documented project decision, or a prior pushback the user accepted.
- [ ] Skips the 4-phase feature pipeline (DTO → Backend → Orval → Frontend).
- [ ] Hand-rolls an API type that Orval already generates.
- [ ] Uses a deprecated path (`packages/shared/*` for new code, `@fixit/api/*` imports, `useWorkerStore`, `useEmployee`, `profile-auth-wiring`).
- [ ] Introduces a whole-store Zustand subscription.
- [ ] Adds manual memoization for a React-Compiler-handled pattern.
- [ ] Duplicates an existing shared primitive (badge, status label, format hook) — search before build.
- [ ] Adds English-only strings, nested i18n keys, snake_case filenames, or lowercase enum values.
- [ ] Includes the FixIt platform shell in a public company query.
- [ ] Runs `git stash`, `expo install --fix`, reverts on failure, or force-pushes to main.
- [ ] Uses Mapbox interactive maps instead of `react-native-maps`.

## When NOT to push back

- One-line typo / translation fixes — just do them.
- A `feedback_*.md` rule the user explicitly invoked — follow it, don't re-validate.
- Findings docs with <3 items — triage inline instead.
- Pure aesthetic preferences the user has authority over.
- Tradeoffs the user already accepted after a prior pushback — don't re-litigate.

Noise dilutes signal: Mode B fires on a tripwire or an explicit ask, not every turn.
