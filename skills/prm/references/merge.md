# merge — gates, the merge itself, teardown

prm's merge terminus engine. Fully autonomous — the pre-check gates ARE the safety.
Drive the gates green, merge (merge commit), tear down the worktree + branch, pull the
main clone (never switch it).

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
requiredBotReviewers, afterMergeCmd, resolvedAfterMergeCmd, stopServers, raw}`.
`botApproval = {ok, required[], pending[]}`; `gates.botApprovalOk` mirrors it as the
`botReview` gate. `mergePolicy` is `review` (default) or `self` — see the keys below;
a non-null `mergePolicyInvalid` is a typo in the config: say so, run as `review`.
`stopServers` is `worktree` (default), `repo` or `none` — step 4c; a non-null
`stopServersInvalid` is likewise a typo: say so, run as `worktree`.

Hard guards — STOP immediately:
- `onDefaultBranch` → "On the default branch — nothing to merge here." Never tell the
  user to switch this checkout to a feature branch.
- `raw.state !== "OPEN"` → already merged/closed.

## Drive to green (bounded loop, max 6 iterations)

While `gates.allPass === false`, act per failed gate, then re-run merge-precheck:

| failed gate | action |
|---|---|
| `clean` (dirty worktree) | commit the whole tree per the `push-all` skill commit doctrine, then `git push` |
| `ci` / `ci-absent` | the CI fix loop below |
| `review` + `CHANGES_REQUESTED` | a round (resolve comments + push). Still not `APPROVED` → **STOP: "blocked on human approval"** |
| `review` + `REVIEW_REQUIRED` | `mergePolicy === "self"` → the review gate is not a gate: `--admin` merge now (no round, no watcher). Else the solo-owner carve-out applies (below) → `--admin` merge, not a STOP. Otherwise **STOP: "blocked on human approval"** — never self-approve (`--admin` does NOT fake an approval) |
| `botReview` (`botApproval.pending` names which) | bot has open threads → a round (resolve + push); the bot re-reviews on the push. Still pending → **STOP: "blocked on bot review (`<bot>` pending)"** — never self-approve, dismiss, or `--admin` past a required bot |
| `conflict` | `gh pr update-branch <pr>`, re-check. Still conflicting → **STOP: "conflicts need manual resolution"** |
| `draft` | `gh pr ready <pr>`, re-check |

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

## Merge

`gh pr merge <pr> --<mergeMethod> --delete-branch` — `mergeMethod` is `merge-precheck`'s
`MERGE_METHOD` reading (default `merge`; `squash` on squash-only repos). Append `--admin`
only when the user passed it or the carve-out applies. Remote branch deleted. A non-null
`mergeMethodInvalid` is a typo in the config: say so, run as `merge`.

## Solo-owner carve-out — unsatisfiable review gate

Some orgs (known: `FixIt-Technologies`) enforce PRs-for-everyone plus an
owner-approval review requirement the user alone bypasses as org owner. On the user's
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

## Feature closure (merged, before Teardown)

Resolve the epic: the PR.s task ref (`vt-<id>` in branch or title → task → parent epic) or the branch handoff.s `feature:`. None → skip silently. Tick the gate matching the PR (`fields.gates[].done = true`, `evidence` = PR URL); every gate done → `ledger_state: closed`, otherwise `next_action` = first open gate and one `create_comment` listing what remains. Unanswered `decisions` items are the only escalation — name them in the hand-back. Contract: `~/.claude/docs/specs/2026-09-05-portfolio-ledger-decisions.md`.

## QA plan (vitrinka, feature lifecycle D6 — merged, after Feature closure)

When the branch carried `vt-<id>` (or the user named a task) and the repo has a
vitrinka binding (`.vitrinka/project.json` or `vitrinka project setup` was run):

1. Resolve the epic: `vitrinka task get <id> --json` — a `task`/`story` climbs
   `parentId` until it reaches the `epic`; no epic → skip and say
   "no epic — QA plan not filed".
2. When the epic already has an OPEN `qa` child
   (`vitrinka task resolve-qa --task <epic> --json` exits 0), the plan exists:
   add this PR's touched journeys to it (tasks skill → "Journeys"); never file
   a second qa task.
3. Otherwise follow the tasks skill's **"QA plan"** recipe: ONE
   `propose_tasks {project, source: {kind: "qa-plan", ref: "<owner/repo#n>"},
   drafts: [...]}` call — the `qa` draft carries `key: "plan"`, `type: "qa"`,
   `parentId: <epicId>`, `fields: {scope, roles}`; every journey draft carries
   `parentKey: "plan"`, `type: "journey"`, `fields: {key, role, route,
   steps: [{name}], expected, test}`, one per user-visible path from the
   decision log (`docs/specs/*-decisions.md` on the branch), the merged diff
   and the e2e specs the PR added or changed. `parentId` is always a task id
   (live or pending), never a position. Accepting a journey accepts its
   pending qa draft first; declining the qa draft declines its journeys. Eve
   refines the drafts when a backend is configured (fail-open); a human
   accepts them from the intake queue.
4. Print the qa task's short URL as `🧪 QA plan: <shortUrl> — ⏸️ waiting on
   intake accept` in the hand-back. Do not compose the qa board yourself —
   `qa_board` runs after the plan is accepted (tasks skill → "Journeys").

## Teardown

Two non-negotiables: (a) every git op runs from the main clone via `git -C <mainClone>`;
(b) MOVE the session shell out of the worktree FIRST.

0. **Stop this PR's watcher FIRST**: `TaskStop` the recorded Monitor task id NOW —
   its self-exit on merged lags a full poll cycle and only covers merges that happen
   outside the session. Id not recorded → find it in the task list by its
   `gitkit pr-events <pr>` command line; "already exited" is fine, a still-running watcher
   after merge is not.
1. **Leave the worktree before removing it**: if `isWorktree`, `cd <mainClone>` as its
   own command BEFORE any removal. `git -C <mainClone>` alone does NOT move the shell.
2. **Self-occupant triage** — this session sits in the worktree it is tearing down
   (the cwd-holders are our own `claude`, its MCP servers and shells, nothing else):
   - **Entered via `EnterWorktree`** → after the dev-server kill + scoped Docker
     teardown, call `ExitWorktree(action: "remove")` INSTEAD of `cd` + `worktree
     remove` (step 5 still deletes the branch, after the exit). The clean gate already
     passed, so `discard_changes` must not be needed; if the tool refuses and lists
     changes, STOP and report — never
     pass `discard_changes: true` on your own initiative.
   - **Launched inside the worktree** (no `EnterWorktree` this session) → ONE one-shot
     Bash call:
     ```bash
     cd <mainClone> && git worktree remove <worktree> && git branch -d <branch>
     ```
     (`git worktree unlock <worktree> &&` first if locked.) The harness re-pins the
     shell to the primary working directory when it finds the anchored dir gone. The
     `cd … &&` prefix must be ON the removal command itself — a `cd` in an earlier
     call does not persist. If the harness demonstrably does NOT re-pin (next command
     dies in the deleted dir): `EnterWorktree(name: "teardown-hop")` → BARE
     `git worktree remove <worktree>` + `git branch -d <branch>` from the hop (not
     `git -C <mainClone>` — an isolation guard refuses main-clone redirects while
     entered; main-clone ops wait until after the exit) → `ExitWorktree(action:
     "remove")`, which parks the session in the hop; a later session sweeps the
     leftover hop under `.claude/worktrees/`. Both unavailable → KEEP: do everything
     else and END the summary with the deferred one-liner
     `git worktree remove <worktree> && git branch -d <branch>`.
   - Holders belonging to **another** session → report `pid + command`, skip removal,
     never kill them.
3. **`resolvedAfterMergeCmd` non-null** → run IT (after the `cd <mainClone>`) and skip
   the generic teardown — the hook owns the richer cleanup (worktree, Docker volumes,
   persona sims, branch, remote); tokens are already substituted.
4. **Else, generic auto-cleanup** — map this worktree's resources FIRST, while
   `<worktree>` still exists:
   - dev servers: `lsof -a -d cwd +D <worktree> -t 2>/dev/null`, then MANDATORY
     intersection with dev-server command lines (`ps -o command= -p <pid>` matching
     `next dev|expo|metro|nx|nest|vite|webpack|tsx watch|ng serve|bun run .*dev`, or
     an orphaned `node` dev process). The lsof list ALONE is a trap — it routinely
     includes other live Claude sessions. **NEVER kill:** `claude`, `tmux`, any
     `*mcp*`, `zsh|bash|sh`, editors, `$SELF`/its subtree — regardless of cwd.
   - Docker: compose projects whose `working_dir` label is inside `<worktree>`:
     `docker ps -a --filter label=com.docker.compose.project --format '{{.Label "com.docker.compose.project"}}\t{{.Label "com.docker.compose.project.working_dir"}}'`.
   Then: `kill -TERM` the FILTERED PIDs only, `sleep 3`, `kill -KILL` stragglers
   (re-filtered) — scoped to `<worktree>`, never machine-wide. Non-dev holders remain
   → report `pid + command`, skip `worktree remove`.
   Then `git -C <mainClone> worktree remove <worktree>` (clean by gate; no `--force` —
   acceptable ONLY after an interrupted previous attempt, because the clean gate had
   passed). Removal is SLOW on bootstrapped worktrees (`node_modules`) — generous
   timeout; 30–60s is NOT failure. Then scoped Docker teardown per mapped project `P`:
   - `P` starts with `wt-` → `docker compose -p "$P" down -v --remove-orphans` —
     re-verify the `wt-` prefix on `P` AND on each volume's
     `com.docker.compose.project` label immediately before removal; REFUSE any volume
     whose label is not `wt-*`.
   - otherwise → `docker compose -p "$P" down` (NO `-v` — volumes preserved).
   Never a machine-wide `wt-*` sweep — that stays an interactive, human-run cleanup.
4b. **Devbox workspace** (after either path, while `<worktree>` is known): if the
   removed worktree carried a `devbox.yaml` (or `devbox.worktree.yaml`) — check
   BEFORE removal, or ask the box: `git -C <mainClone> ls-tree HEAD devbox.yaml` —
   run `devbox reap --worktree <worktree> --json` from the main clone. It frees the
   branch's port window and stops its stack; it applies exactly `devbox gc`'s verdict,
   so it can never reap more than gc would. `ok:false` with `WS_NOT_DEAD` → report the
   diagnostic's `fix` line, never retry, never `devbox down` on your own; `ok:true`
   with `WS_NO_RUNTIME_META` (never instantiated) is a clean no-op. `devbox` missing
   from PATH → skip silently (a non-devbox Mac). `WS_REAP_FAILED` saying "held or
   not parked/stopped" on a workspace whose apps are all inactive is a **hold
   lease** (4 h, renewed by every `devbox up`/`run`; `park`/`down` never clear it):
   `devbox unhold <workspace>` then reap again — seen 2026-09-14 on
   `fixit-work-vt-863`. A workspace with apps still ACTIVE is a different case:
   report it, never `unhold` your way past a live session.
4c. **Main-clone dev servers** — `AFTER_MERGE_STOP_SERVERS` (after EITHER path, hook
   or generic, because a repo whose work lands from the main clone never had a
   worktree to scope step 4 to). `worktree` (default) → nothing extra; step 4 already
   covered it. `repo` → repeat step 4's dev-server mapping with `<mainClone>` in place
   of `<worktree>`: `lsof -a -d cwd +D <mainClone> -t`, the SAME mandatory command-line
   intersection and the SAME never-kill list, then `kill -TERM` → `sleep 3` →
   `kill -KILL` stragglers. Docker is NOT touched here and the clone is never removed.
   `none` → skip entirely, even in step 4. A process whose cwd is the main clone but
   belongs to ANOTHER session's port window is still a kill (the cwd IS the repo) —
   what protects other sessions is the never-kill list, not ownership guessing; report
   every pid + command you killed. Unset key = `worktree`.
5. **Delete the local branch, then prune** — always, after EVERY path: `ExitWorktree`
   and an `AFTER_MERGE_CMD` hook may or may not have deleted it, and a merged branch
   never survives the session. `git -C <mainClone> branch -d <branch>`; "not found"
   means done. `error: … not fully merged` is the NORMAL outcome of a squash/rebase
   merge or a main clone not yet pulled, never a reason to leave the branch: the merge
   is confirmed and `git -C <mainClone> rev-parse <branch>` equals the PR's `headSha`
   → the tip is provably in the PR → `git -C <mainClone> branch -D <branch>`. A tip
   that is NOT the merged `headSha` (commits never pushed into the PR) → KEEP it and
   report. Then `git -C <mainClone> fetch --prune`, and verify: `git -C <mainClone>
   branch --list <branch>` prints nothing.
6. **Pull the main clone — never switch it.** It is ALWAYS on the default branch — the
   `DEFAULT_BRANCH` overlay when set, else GitHub's (the
   standing arrangement, not something to verify-then-correct): `status --porcelain`
   empty → `git -C <mainClone> pull --ff-only`; dirty → SKIP and warn. Not on the
   default branch → report and skip the pull, never correct it. Never `git switch`/
   `checkout` the main clone — it is the user's seat and the guard hard-blocks it.
7. **Summary** (`output.md`): full clickable URLs (merged PR, base branch). The shell
   may sit in the removed worktree — END with an explicit `cd <mainClone>` line.

## `.claude/.claude.git.config` keys

Same `KEY=value` file `/sync` reads, in `<repo>/.claude/`:

```
# How a green PR reaches the default branch. review (default): the loop as written —
# rounds, watcher, human/bot approval. self: a solo-owner repo where the PR is the
# TRAIL, not a review — ensure-pr labels it `eve-ignore` (eve never reviews it), and
# once clean + CI + mergeable pass the merge is `--admin` immediately: no round, no
# Monitor, no hand-back "waiting on review". Still never past red CI, a conflict, a
# pending required bot, or a PR authored by someone else. Worktree rules unchanged.
MERGE_POLICY=self

# Which `gh pr merge` flag lands a green PR: merge (default, merge commit) | squash (one
# commit titled after the PR — set this on squash-only repos) | rebase.
MERGE_METHOD=squash

# Runs INSTEAD of the generic worktree-remove + branch -d after a successful merge.
# Tokens substituted by gitkit merge-precheck: {slug} {branch} {worktree} {pr}
AFTER_MERGE_CMD=/wk:cleanup {slug} --remove --yes --delete-remote

# Which dev servers a successful merge stops, ON TOP of whichever teardown path ran.
# worktree (default): only the merged worktree's, as step 4 already does. repo: also
# the ones whose cwd is the MAIN CLONE — for repos that work from main (content ships
# with no worktree) or leave a preview server there after hand-testing. none: stop
# nothing. Same command-line filter and never-kill list either way; never Docker,
# never the clone itself.
AFTER_MERGE_STOP_SERVERS=repo

# Runs when prm ENTERS the review loop, and again at the start of each round.
# Same tokens. Must be idempotent and cheap. Stops processes we own (dev servers,
# bundlers, emulators) — never databases/containers, which stay warm.
BEFORE_REVIEW_CMD=/wk:pause {slug}

# The branch PRs land on and the seat the main clone sits on. Set it in the gitignored
# .claude/.claude.git.config.local while one machine adopts an integration branch ahead of
# the team (e.g. Reservine's devlp, 2026-09); absent → GitHub's default branch.
# Read by gitkit merge-precheck, resolve-fetch (prm) and sync-context (/sync).
DEFAULT_BRANCH=devlp

# Only for machine-user bots (ordinary account/PAT, __typename == "User") that can't
# be auto-detected. GitHub-App bots (__typename == "Bot") are auto-detected and
# required whenever on the PR — most repos need no config. [bot] suffix optional.
# Absent → defaults to eve-bot-lovinka[bot]. A listed bot only gates when actually
# on the PR (vacuously satisfied otherwise).
REQUIRED_BOT_REVIEWERS=eve-bot-lovinka[bot], my-ci-machine-user
```

`BEFORE_REVIEW_CMD` is resolved by `vybava gitkit before-review` (git-only — no `gh`
call, no PR required, so the create path can quiesce before the PR exists) and also
emitted as `resolvedBeforeReviewCmd` by `gitkit merge-precheck`. Per-round contract:
`round.md` §0.5. A required bot is approved only when its LATEST review is `APPROVED`.

## Hard rules

- Never merge from the default branch; never `--admin` unless the user passed it or
  the carve-out applies; never self-approve, dismiss, or `--admin` past a pending
  required bot.
- Never `--force` a worktree removal; never switch or pull a dirty main clone.
- **Stacked PRs: retarget the child BEFORE deleting a base branch.** Deleting the base
  auto-closes every PR stacked on it. Recovery: push the old SHA back as a branch,
  reopen the child, retarget it, then delete.
- Dev-server kills and `down -v` stay surgically scoped as specified above.
- Gates needing a human (approval, unresolvable conflict, repeated CI failure) STOP
  with a link-bearing report — autonomous ≠ overriding protections.
