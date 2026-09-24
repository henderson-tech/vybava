---
name: sync
description: "Use when the user asks to sync, pull, or bring a checkout up to date ('sync this', 'pull latest', 'get up to date with main', 'update my branch'). Two modes by branch identity: default branch commits + pulls ff-only + pushes; feature branch aligns with origin, merges the default branch with triaged conflicts, then deps → backup+migrate → regen → verify → restart."
---

# sync — bring this checkout up to date, whichever branch it's on

One command, two modes, decided by **branch identity**. Per-project behavior comes
from `<repo>/.claude/.claude.git.config`, which sync **populates itself on first
run**. Local dev-sync only: never targets prod, never force-pushes, never deletes
branches.

## Usage

`/sync [--dry-run] [--continue]`

- `--dry-run` — resolve the plan, print mode + every action it *would* take, touch nothing.
- `--continue` — resume after hand-resolved gated conflicts (finishes the merge if
  still in progress, then runs the shared tail).

To disable the migrate/restart steps, leave `DB_MIGRATE_CMD` / `RESTART_CMD` unset.

## 0. Anchor and resolve

Capture the absolute repo root ONCE (`git rev-parse --show-toplevel`) and pass it
literally as `--repo <ABS>` to every helper and `git -C <ABS>` to every git call —
never let a helper infer the repo from ambient cwd; a wrong-repo `git status` is a
silent, valid, wrong answer this command would then commit or merge against.

```
vybava gitkit sync-context --repo <ABS>
```

→ `{mode, currentBranch, upstream, isWorktree, defaultBranch, mergeStrategy,
packageManager, installCmd, lockfiles, generatedPaths, generatedFromConfig, regenCmd,
verifyCmd, migrateCmd, backupCmd, migrationsPaths, restartCmd, hasComposeFile,
localDbUrlVar, dbHost, localDbOk, runAfterSync, configFound, configPath, missingKeys,
configComplete}`

**`configComplete: true`** → execute the plan; ask nothing, explore nothing.

**First run (`configComplete: false`)**: resolve `missingKeys` once — inspect
`package.json` scripts, the codegen config (`orval.config.*`, `openapi-*`,
`codegen.yml`) and where its output lands — then ONE `AskUserQuestion` batch to
confirm `REGEN_CMD` / `VERIFY_CMD` / `GENERATED_PATHS`. Then freeze:

```
vybava gitkit sync-context --repo <ABS> --freeze
```

`--freeze` appends only keys the file doesn't already set — never overwrites a
hand-written value, re-running is a no-op. A declined key: write the confirmed ones
by hand, leave the rest for next time. Never freeze a secret — only command strings
and paths.

## 1. Guards

- Detached HEAD → STOP: "check out a branch first."
- Mid-merge/rebase in progress and no `--continue` → STOP, point at `--continue`.
- `--continue` with no merge in progress and a clean tree → skip to the tail (§5).

sync never stops for a dirty tree — handling dirt is its job.

## 2. Commit what's here (both modes)

Sweep the entire working tree per the `push-all` skill §1–2 (classify
with `gitkit classify-paths`, coherent path-scoped Conventional Commits; secrets never
committed and named loudly; artifacts left in place and listed).

⚠️ This deliberately overrides the "commit only your own change-set" law (user's
explicit call). In a worktree the whole tree is your work. In the primary clone on a
feature branch it can sweep a parallel session's WIP — so **list every swept path in
the report, grouped by commit**; the bundling is always visible, never silent.

## 3. MAIN MODE

1. `git -C <ABS> fetch --prune origin`
2. Record `BEFORE`, then `git -C <ABS> pull --ff-only`. Not fast-forwardable → local
   default branch has diverged: **STOP**, report the fork point + ahead/behind; never
   merge or rebase the default branch unasked.
3. Commits ahead of upstream → `git -C <ABS> push`. Never `--force`; a protected-
   branch rejection is information, not a failure to work around.
4. → §5.

## 4. BRANCH MODE

1. **Align with your own branch:** `git -C <ABS> fetch --prune origin`. No upstream →
   skip. `origin/<branch>` ahead, fast-forwardable → `merge --ff-only @{u}`.
   Diverged → `merge @{u}`, conflicts through §4.1. Never rebase — those commits are
   published and possibly a colleague's.
2. **Commit** — §2.
3. **Push** (`-u` if no upstream) — your work reaches the remote *before* the merge
   can complicate it.
4. **Merge the default branch:** record `BEFORE=$(git -C <ABS> rev-parse HEAD)`, then
   `git -C <ABS> merge origin/<defaultBranch>` (rebase only when
   `mergeStrategy == "rebase"` is explicit in config).
5. **Push again** once merged and verified. → §5.

### 4.1 Conflict triage — every conflicted file into exactly one bucket

**REGENERATE** — matches `generatedPaths` (no-glob pattern = path prefix; `**`
crosses separators), or is a lockfile. Generated output is never hand-merged; the
merged sources are the truth. Lockfile → `checkout --theirs` and let `installCmd`
rewrite it in the tail. Otherwise → `checkout --theirs`, `git add` purely to unblock
the merge, record for regen; after the merge run `regenCmd` and `git add` the output.
`regenCmd == null` → do not guess: leave conflicted, name the missing codegen
command, offer to record `REGEN_CMD`.

**AUTO** — disjoint, mechanical, or purely additive hunks (both sides appended to
different regions, import/export blocks, added enum members/switch cases,
formatting). Resolve by intent, preserving both sides' meaning. Never blanket
`--ours`/`--theirs` on hand-written code.

**GATE** — both sides rewrote the same logical unit (same function body, condition,
constant) or the changes are semantically incompatible. Uncertainty is a gate, not
an auto.

### 4.2 The gate

Finish every REGENERATE and AUTO first. Then: one summary table of hard conflicts
(`path:line` | what collided | why it's hard), then `AskUserQuestion` batched 4 per
call, options per conflict:

- *Apply my resolution (Recommended)* — the conflict hunks + proposed replacement in
  `preview`.
- *Keep mine* (HEAD) / *Keep theirs* (incoming) — state what each loses.
- *I'll do it myself* → stop cleanly MID-MERGE: AUTO files staged, regenerated files
  staged, only gated files carrying markers. Report the exact state and the resume
  path (resolve → `git add` → `git commit` → `/sync --continue`). Never
  `git merge --abort` — it would discard approved work.

## 5. Shared tail (both modes) — `RANGE = BEFORE..HEAD`

1. **Deps** — `RANGE` touched a `lockfiles` entry and `installCmd` set → run it.
2. **Migrations** (only if `RANGE` touched `migrationsPaths`):
   - `migrateCmd == null` → NOTIFY "set `DB_MIGRATE_CMD`" and skip.
   - 🚨 `localDbOk !== true` → **STOP the migration step**. `false` = `dbHost` looks
     remote/prod — NEVER migrate it; `null` = unconfirmed — stop to be safe.
   - 🚨 `backupCmd == null` → **STOP the migration step**: "refusing to migrate
     without a backup command — set `DB_BACKUP_CMD`." Otherwise run `backupCmd`,
     confirm success, THEN `migrateCmd`.
3. **Regen** — `regenCmd` set and (a REGENERATE conflict occurred, or `RANGE` touched
   a generated source) → run it; commit the result if it changed anything.
4. **Verify** — run `verifyCmd` only when this sync did something risky (any AUTO
   resolution, regen, or dep reinstall); a clean fast-forward skips it. **Never
   continue past a failing verification** — STOP, show it, leave the tree as-is.
5. **Restart:** `restartCmd` set → run it; else compose services running → `docker
   compose restart`; bare dev servers (`next dev`, `vite`, Metro, …) → REPORT how to
   restart, never kill the user's foreground process.
6. `runAfterSync` set → run it.

## 6. Report

Mode + branch; paths swept per commit (§2's visibility rule); alignment result;
merged range with full clickable URLs (the `prm` skill's `references/output.md`);
conflicts by bucket (regenerated / auto / gated + each gate's answer); deps / migrate
/ regen / verify / restart each done-or-skipped **with the reason**; any frozen keys.

## Hard rules

- Never blanket `--ours`/`--theirs` on hand-written code; never continue past failing
  verification; never `merge --abort` on the user's behalf; never rebase a branch
  with an upstream.
- 🚨 DB: backup BEFORE migrate; NEVER migrate unless `localDbOk === true`.
- Never `git stash`, `git checkout .`, `git restore .`, or `git checkout <branch>` —
  sync changes what's *committed*, never what's *checked out*.
- Every git mutation names its repo: `git -C <ABS>`.

## `.claude/.claude.git.config` (per-project, `KEY=value`, `#` comments; gitignored `.claude/.claude.git.config.local` overrides key by key)

`--freeze` writes the auto-detected ones; a hand-written value always wins.

```
DEFAULT_BRANCH=main                       # else auto from origin/HEAD
MERGE_STRATEGY=merge                       # merge | rebase (never auto-set)
INSTALL_CMD=bun install                    # else auto from lockfile
GENERATED_PATHS=packages/api-client/src/generated,**/*.gen.ts
REGEN_CMD=bun api:generate                 # codegen; also resolves generated conflicts
VERIFY_CMD=bun typecheck                   # run after risky syncs
DB_BACKUP_CMD=bun db:backup                # REQUIRED before migrate
DB_MIGRATE_CMD=bun db:migrate              # else auto from package.json scripts
MIGRATIONS_PATHS=apps/api/migrations,prisma/migrations
RESTART_CMD=docker compose restart api     # else auto (compose) / report
LOCAL_DB_URL_VAR=DATABASE_URL              # checked for localhost before migrate
RUN_AFTER_SYNC=bun gen:types               # optional final step
```

`GENERATED_PATHS` unset → conservative built-ins (`**/generated/**`,
`**/__generated__/**`, `**/*.gen.*`, `**/*.generated.*`,
`**/openapi*.{json,yaml,yml}`, `**/schema.graphql`, `**/graphql.schema.json`,
`**/prisma/client/**`). The same file carries prm's keys (`MERGE_POLICY`, `MERGE_METHOD`, `AFTER_MERGE_CMD`,
`BEFORE_REVIEW_CMD`, `REQUIRED_BOT_REVIEWERS`) — see
the `prm` skill's `references/merge.md`.
