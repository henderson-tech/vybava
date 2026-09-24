# ensure-pr — make sure a PR exists for THIS checkout

prm's create-or-find path. Idempotent: guarantees an open PR exists for
the current branch and returns its URL + number. **Current-branch selector only** — an
explicit `<N>` / URL / `latest by` selector with no PR stays a hard error.

Inputs (optional): `--base <branch>` (default: `defaultBranch` from `gitkit resolve-fetch` — the
`DEFAULT_BRANCH` overlay in `.claude/.claude.git.config.local` when the machine has one, else
GitHub's default branch) ·
`--draft` (explicit only) · `--title <text>` / `--body <text>` (either alone is fine).

**Always author the body yourself** per `pr-body.md` (sibling file) —
`--fill` restates the commit log and is a fallback for a single self-explanatory commit
only. **Never type a multi-line body inline on the command line** (zsh mangles it):
write it to a scratchpad file and pass `--body "$(cat <file>)"` (double-quoted), or
`gh pr edit <N> --body-file <file>`.

Steps:

0. **Quiesce, then write the body.** Run
   `vybava gitkit before-review --repo <ABS repo path>`
   (LITERAL absolute path); a non-null `resolvedBeforeReviewCmd` runs BEFORE anything
   else here, null → skip silently — dev servers and bundlers must stop before you
   diff. Then write the body per `pr-body.md` — run its blockers lens and links sweep
   over `git diff <base>...HEAD`, and fill `Opened by session` from
   `printenv CLAUDE_CODE_SESSION_ID`, so migrations, env vars, flags, deployed-client
   breaks, boards and the opening session surface before the PR exists.
1. Resolve the current-branch PR via `vybava gitkit resolve-fetch`.
2. **PR already exists** → return its URL + number (never error or recreate). Check its
   body against `pr-body.md`: a commit-log dump, a missing `Blockers & risks` section
   or a missing links table → rewrite (`gh pr edit <N> --body-file <file>`) and say so.
   A body a human hand-wrote is theirs — append the missing section/table, keep their
   prose.
3. `gitkit resolve-fetch` returns `{ "noPr": true, … }`:
   - `onDefaultBranch === true` → **STOP**: never open a PR from the default branch;
     tell the user to create/switch to a feature branch.
   - otherwise: `git push -u origin HEAD`, then
     `vybava gitkit github-io create-pr --head <branch> --base <base>`
     with `--title <text>` and `--body "$(cat <file>)"` (caller's values win when
     supplied) and `--draft` when passed. **`MERGE_POLICY=self`** in
     `<mainClone>/.claude/.claude.git.config` → add `--label eve-ignore` (create the
     label first if missing: `gh label create eve-ignore --color ededed --description
     "skip eve's automatic PR review" --force`) so eve never reviews a PR nobody will
     wait on; a labelled PR whose author later wants a review just removes the label. `--fill` stays in argv as a backstop, but
     reaching it means step 0 was skipped — a bug, not an outcome. "No commits between
     <base> and <branch>" → **STOP** and say so. Print the URL per `output.md`; return
     URL + number.
4. **Attach the PR to its task (vitrinka, run publish 2026-09-09 D10).** When the
   branch or title carries `vt-<id>` and the repo has a vitrinka binding
   (`.vitrinka/project.json`): `add_task_ref {id: <id>, kind: "pr", ref:
   "<owner/repo>#<N>", meta: {title, url}}` on the task AND, when the task
   climbs to an `epic`, the same ref on the epic — the epic's rollup ("last run …
   · PR #N"), the final artifact's § Delivery and the run door all read `pr`
   refs. Re-attaching the same (kind, ref) only updates meta, so re-running is
   safe. No `vt-<id>` or no binding → skip silently, never ask.
