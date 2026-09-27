# teardown — after the merge: closure, QA plan, cleanup

prm's post-merge engine, run right after `merge.md`'s merge call succeeds: Feature
closure → QA plan → Teardown, in that order. A PR merged anywhere else runs the
Teardown section alone (SKILL.md → Merge terminus, case A). The keys it reads
(`AFTER_MERGE_*`, `DEFAULT_BRANCH`) are in `config.md`.

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
   act per `devbox` from the precheck. `devbox` missing from PATH → skip silently (a
   non-devbox Mac). Either mode touches only this branch's workspace, never a sibling.
   - **`down` (default) — merge = the branch's stack goes.** Resolve the name BEFORE
     the worktree is removed (`(cd <worktree> && devbox status --json)` →
     `data.context.workspace`); after removal, `devbox down <workspace> --json` →
     `devbox unhold <workspace> --json` → `devbox reap --worktree <worktree> --json`
     from the main clone. `down` may retire the record outright: a later `unknown
     workspace` / `WS_NO_RUNTIME_META` means it is already gone, a clean finish. Any
     other failure is reported with its diagnostic, never retried.
   - **`reap` — gc's verdict only.** `devbox reap --worktree <worktree> --json` from
     the main clone frees the branch's port window and stops its stack, but never
     more than `devbox gc` would. `ok:false` with `WS_NOT_DEAD` → report the
     diagnostic's `fix` line, never retry, never `devbox down` on your own; `ok:true`
     with `WS_NO_RUNTIME_META` (never instantiated) is a clean no-op. `WS_REAP_FAILED`
     saying "held or not parked/stopped" on a workspace whose apps are all inactive
     is a **hold lease** (4 h, renewed by every `devbox up`/`run`; `park`/`down` never
     clear it): `devbox unhold <workspace>` then reap again — seen 2026-09-14 on
     `fixit-work-vt-863`. Apps still ACTIVE → report it, never `unhold` your way
     past a live session.
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

## Hard rules

- Never `--force` a worktree removal; never switch or pull a dirty main clone.
- Dev-server kills and `down -v` stay surgically scoped as specified above.
