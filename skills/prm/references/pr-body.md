# pr-body — the PR description contract

Every PR prm opens **or watches** carries a body written to this contract.

Reader: the repo owner months later with zero context, deciding *"is this still worth
merging, and what does merging it cost me?"*. Short and dense: every sentence carries a
fact — a behaviour, a decision, a name, a number — and a sentence that states nothing
new is cut. No narrative, no file inventory (the Files tab is one), nothing that needs
the diff open to parse. Whole body ≤ ~150 words outside the links table.

## Anti-patterns

| ✗ Don't | ✓ Do |
|---|---|
| A bullet per changed file, `--fill`, a pasted commit log | Why → approach → intent, a few dense sentences each |
| Connective prose ("In order to…", "This means that…") | Fact-only sentences |
| Describe the code (`adds enhanceSnippet()`) | Describe the behaviour the user gets |
| Internal shorthand as load-bearing text (`Implements D2/D3`) | Say the thing; the spec is a row in the links table |
| Links scattered through sections | One links table, identical rows in every PR |
| Silence on migrations / env vars / flags | One `Blockers & risks` bullet each, `None.` when empty |

## Required shape

```markdown
## Why this exists
<1–3 sentences: what is wrong today, what stays broken if this never merges.>

## Approach
<2–4 sentences: how it was solved, what was deliberately not done and why.>

## Intent
<1–2 sentences: the outcome merging should produce — what to judge this PR against.>

## Links
| | |
|---|---|
| Task | <url> |
| Epic | <url> |
| Spec / design | <url · url> |
| QA / testing | <url · url> |
| Review | <url> |
| Opened by session | `<CLAUDE_CODE_SESSION_ID>` |

## Blockers & risks
<one bullet per item, tagged `before:` (must happen first) or `after:` (the merge does
not do it itself), naming the exact command / key / file — or `None.`>

## Verification
<1–3 sentences: what was proven and how; what was NOT covered.>
```

Sections and table rows are **never omitted** — `None.` / `—` tells the reader the
question was asked. Rows: Task = the bound vitrinka task; Epic = its parent epic;
Spec / design = brainstorming, decision-log and design boards; QA / testing = testing
sets, journeys, recorded sessions; Review = the `pr-<N>-<repo>` board and Eve review
boards; Opened by session = `printenv CLAUDE_CODE_SESSION_ID` at create time, never
changed by later rounds. Every URL is the server-returned `url`, full, never shortened.
An optional `<details>` implementation-notes block may close the body after Verification, never sit above it.

## The blockers lens

Sweep the diff before writing `Blockers & risks` (same irreversibles lens as
`auto-audit.md` §4, asking *"what must a human DO about it?"*):

| Category | Look for | Tag |
|---|---|---|
| **Env vars / secrets** | New/renamed keys, changed defaults, new required credential | before |
| **DB migrations** | Migration files, DDL, index builds | after — plus backup reminder when destructive |
| **Client data** | Backfills, repair scripts, re-indexing, cache invalidation | after, with the exact command |
| **Feature flags** | A flag this PR reads or flips | before (create) / after (flip) |
| **Config & infra** | compose, nginx, Dockerfile, CI, cron | whichever applies; name the file |
| **Deployed clients** | API/shape change a mobile app or other service consumes | before — the consumer ships first |
| **Package publish** | Version bump needing `npm publish` / a tag | after |
| **Merge order** | A PR that must land first, a stacked branch | before, linked |
| **Manual verification** | Something only a human on prod can confirm | after |

**Deploy-on-merge repos** (merge to default = ship — vitrinka via Deployik, for one):
anything the code needs to boot — env vars above all — is a `before:` bullet, never
`after:`. Say so: `⚠️ before: merging deploys — VITRINKA_SMTP_CA must be set in production first.`

## The links sweep

Before writing: `list_boards` scoped to this repo/branch (plus `pr-<N>-<repo>` when the
PR exists) fills the three board rows; `get_task` on the `vt-<id>` from the branch or
title fills Task and Epic. Nothing found → `—` (never `create_board` for the body's
sake). A board or task appearing later in the PR's life lands in the table on the
upkeep rewrite.

**A links cell is a full URL or `—`, nothing else.** Never a description of where a
link could be found ("see epic refs", "board published from this session", a bare
`docs/specs/…` path). If a board publish or a task upload is still in flight when the
body is written, either wait for its URL before creating the PR, or write `—` and
rewrite the body the moment the URL lands (`gh pr edit <N> --body-file`) — the
publisher's report and the vitrinka task refs both carry it. Repo files (specs,
decision logs) link as `https://github.com/<owner>/<repo>/blob/<branch>/<path>`.
Observed 2026-09-15 on FixIt#1448: the board URL was in hand and the row still said
"hand-test board: see epic refs" — a reader months later cannot click that.

## Keeping it true (`prm`)

The body describes the PR **as it will merge**. Rewrite (`gh pr edit <N> --body-file <f>`)
when a round uncovers/retires a blocker, moves the scope, stales `Verification`, or
adds a link. Routine churn needs no edit. Human-edited prose stays theirs — only
`Blockers & risks` and the links table may be appended to. A stale blocker is worse
than none.

## Worked example

The vitrinka install-snippet PR (#252):

```markdown
## Why this exists
`/connect` ships install commands with `<your-token>` placeholders; every agent hookup
is copy → hunt the placeholder → mint a token in Settings → paste back. First minute
of the product, worst-feeling one.

## Approach
Placeholder values become editable spans inside the snippet (click, Tab cycles, Esc
restores); copy yields the resolved command. `/connect` pre-fills scope, base URL and
token; Settings → Tokens mints and fills in one step under the existing one-time-reveal
rule. JS-off renders the values as plain selectable text. No new endpoint.

## Intent
A new user connects an agent from `/connect` with one copy and zero tab switches.

## Links
| | |
|---|---|
| Task | https://…/t/vt-312 |
| Epic | — |
| Spec / design | https://…/b/456 |
| QA / testing | — |
| Review | https://…/b/pr-252-vitrinka |
| Opened by session | `9c1e…` |

## Blockers & risks
None.

## Verification
Hands-on on local dev: Tab cycling, Esc restore, resolved copy text; a minted `vks_…`
token landed in both the command and the tokens table. `go test ./internal/site
./internal/web` green. Not covered: no e2e drives the edit interaction —
`TestConnectHasEditableSnippetVars` asserts server-rendered markup only.
```
