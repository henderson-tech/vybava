# merge — gates and the merge itself

prm's merge terminus engine. Fully autonomous — the pre-check gates ARE the safety.
Drive the gates green and merge (by `mergeMethod`); a successful merge continues in
`teardown.md` (feature closure, QA plan, worktree + branch cleanup, main-clone pull —
never a switch). The config keys named here are documented in `config.md`.

## Pre-check

`vybava gitkit merge-precheck <PR?> --repo <ABS path of the CHECKOUT you are merging FROM>`
(LITERAL absolute path — never `$(git rev-parse --show-toplevel)`). **When the feature
lives in a worktree, that is the WORKTREE path (`…/.worktrees/<name>`), NOT the main
clone** — anchoring at the main clone flips `isWorktree:false`/`onDefaultBranch:true`
(a wrong-reason STOP) and resolves `slug`/`resolvedAfterMergeCmd` against the main
clone, aiming the teardown hook at the primary checkout. Cross-check: envelope says
`isWorktree:false` (or `slug` equals the main clone's basename) while you know you're
in a worktree → the anchor is wrong; re-run, never act on that envelope.

→ JSON: `{owner, repo, pr, url, title, branch, defaultBranch, onDefaultBranch,
worktree, mainClone, isWorktree, slug, checks, gates, botApproval,
requiredBotReviewers, mergePolicy, mergeMethod, mergeMethodSource, mergeMethodReason,
mergeMethodsAllowed, afterMergeCmd, resolvedAfterMergeCmd, stopServers, devbox, raw}`
(`mergeMethod*` are null/empty on a PR that is not OPEN — see Merge).
`botApproval = {ok, required[], pending[]}`; `gates.botApprovalOk` mirrors it as the
`botReview` gate. `mergePolicy` is `review` (default) or `self` — `config.md`;
a non-null `mergePolicyInvalid` is a typo in the config: say so, run as `review`.
`stopServers` is `worktree` (default), `repo` or `none` — `teardown.md` step 4c; a
non-null `stopServersInvalid` is likewise a typo: say so, run as `worktree`.
`devbox` is `down` (default) or `reap` — `teardown.md` step 4b; a non-null
`devboxInvalid` is a typo: say so, run as `reap`.

Hard guards — STOP immediately:
- `onDefaultBranch` → "On the default branch — nothing to merge here." Never tell the
  user to switch this checkout to a feature branch.
- `raw.state !== "OPEN"` → already merged/closed.
- an unmet `before:` item in the body's `Blockers & risks` (`pr-body.md`) — a PR that
  must land first, a deploy prerequisite → STOP naming it. `merge-precheck` cannot read
  the body: verify each item (`gh pr view <N> --json state` for a linked PR) before
  every merge, `--auto` included.

## Drive to green (bounded loop, max 6 iterations)

While `gates.allPass === false`, act per failed gate, then re-run merge-precheck:

| failed gate | action |
|---|---|
| `clean` (dirty worktree) | commit the whole tree per the `push-all` skill commit doctrine, then `git push` |
| `ci` / `ci-absent` | the CI fix loop below. Never fires for a PR carrying `skip-ci` — `gates.ciWaived: true` makes `ciOk` hold over cancelled/red runs, by design (`docs/skip-ci.md`); such a PR lands only with `--admin`, so without it (and no carve-out) **STOP: "labelled skip-ci — needs --admin"** |
| `review` + `CHANGES_REQUESTED` | a round (resolve comments + push). Still not `APPROVED` → **STOP: "blocked on human approval"** |
| `review` + `REVIEW_REQUIRED` | `mergePolicy === "self"` → the review gate is not a gate: `--admin` merge now (no round, no watcher). Else the solo-owner carve-out applies (below) → `--admin` merge, not a STOP. Otherwise **STOP: "blocked on human approval"** — never self-approve (`--admin` does NOT fake an approval) |
| `botReview` (`botApproval.pending` names which) | bot has open threads → a round (resolve + push); the bot re-reviews on the push. Still pending → **STOP: "blocked on bot review (`<bot>` pending)"** — never self-approve, dismiss, or `--admin` past a required bot |
| `conflict` | `gh pr update-branch <pr>`, re-check. Still conflicting → **STOP: "conflicts need manual resolution"** |
| `mergeable-unknown` | GitHub is still computing after a push — wait ~15 s and re-run the precheck (up to 4 times); still unknown → report it, never merge past it |
| `draft` | **STOP: "draft — waiting on `gh pr ready`"**. Drafts are explicit-only (`--draft`), so whoever asked for one readies it, e.g. a release lane once its device evidence is published. prm never readies a draft itself. |

A STOP prints the blocker + PR URL and stops. 6 iterations still red → STOP and report.

### CI fix loop

1. `vybava gitkit github-io find-run --sha <headSha>` → the run;
   none yet → wait for the watcher's next `ci` event.
2. `gitkit github-io watch-run --runId <id>` (blocks; `--exit-status`). Green → done.
3. Red → `gitkit github-io failed-logs --runId <id>`; diagnose the ROOT cause from the
   logs — never pattern-match the first error line, never push a speculative fix.
4. Fix (failing test first when the failure is a test), run local checks, push, re-watch.
5. Caps: 3 fix attempts on the same failing job → STOP with the run URL. Infra/flaky
   failure (timeout, runner error, network) → `gitkit github-io rerun-failed --runId <id>`
   once; still red → STOP.

## Extensions before the merge (`merge` stage)

Gates green → `vybava gitkit pr-extensions --stage merge --repo <ABS checkout path>`
→ follow each listed extension per `extensions.md`, immediately before the merge call.
None listed → skip silently. One that pushes moved the head: re-run the precheck and
Drive to green, then run the `merge` extensions again (they must now change
nothing). One that fails is a STOP — nothing merges.

## Merge

`gh pr merge <pr> --<mergeMethod> --delete-branch` — `mergeMethod` is what
`merge-precheck` read from the config and GitHub: the first `MERGE_METHOD_BY_HEAD` pair
matching the head (`mergeMethodSource: "head"`; built in: `promote/*:merge`, so a
promotion lands as a merge commit), else an explicit `MERGE_METHOD` the base
branch permits (`"config"`), else the first of merge → squash → rebase
that the repository's buttons AND the base's rulesets/protection permit (`"repository"`
— linear history refuses merge commits, so henderson-tech repos land as squash with no
key). Never substitute a method of your own; `mergeMethodReason` says why. Append
`--admin` only when the user passed it or the carve-out applies — a `gates.ciWaived`
PR has no green required check, so it needs one of the two. Remote branch deleted.
A non-null `mergeMethodInvalid` is a config typo or a method the base refuses: say so,
run as `mergeMethod`. A precheck that exits 1 with `cannot read …`, `no merge method is
permitted …` or `STOP — <head> must land as …` (a head override the base refuses) is a
STOP with that line — never fall back to another method.

**Promotion heads (`promote/*`) are immutable**: the branch is content-identical to
commits already reviewed and tested upstream, so no review rounds, no CI-fix pushes and
no update-branch on it. Any red gate is a STOP: "cut the promotion again".

**Production bases**: when the repo's `PROD_BRANCHES` names the base, the merge call
is blocked by claude-guards `prod-merge:merge`. That is the user's merge: STOP and hand
the green PR back. Only on the user's explicit go for that one PR, run the merge with
`CLAUDE_ALLOW_PROD_MERGE=1` in front.

**Merged** → `teardown.md`, at once.

## Solo-owner carve-out — unsatisfiable review gate

Some orgs (known: `henderson-tech`, formerly `FixIt-Technologies`) enforce
PRs-for-everyone plus an owner-approval review requirement the user alone bypasses as org owner. On the user's
OWN PRs that gate is structurally unsatisfiable — GitHub forbids self-approval, and
bot approvals never count toward a code-owner review — so it is NOT a
"blocked on human approval" STOP. When ALL of:

1. PR author == the authenticated `gh` user (`gh api user` vs `gh pr view --json author`), and
2. the ONLY failing gate is `review` (`REVIEW_REQUIRED`): CI green, no conflicts,
   required bots approved, audit PASS where one applies, and
3. a plain merge is refused by branch policy ("base branch policy prohibits the merge"),

merge with `--admin` — the normal completion, not an escalation: the server enforces
the ruleset bypass identity; nothing is faked. Never for PRs authored by anyone else
(human OR bot), a pending required-bot review, a BLOCK/stale audit, or red CI.

## Hard rules

- Never merge from the default branch; never `--admin` unless the user passed it or
  the carve-out applies; never self-approve, dismiss, or `--admin` past a pending
  required bot.
- **Stacked PRs: retarget the child BEFORE deleting a base branch.** Deleting the base
  auto-closes every PR stacked on it. Recovery: push the old SHA back as a branch,
  reopen the child, retarget it, then delete.
- Gates needing a human (approval, unresolvable conflict, repeated CI failure) STOP
  with a link-bearing report — autonomous ≠ overriding protections.
