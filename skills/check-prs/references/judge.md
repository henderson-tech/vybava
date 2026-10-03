# Judgment briefs

Both briefs carry the PR's census row verbatim, the active PRs' census rows, and
these rules:

- Read-only: `gh pr view|diff|checks`, `git -C <worktree>` reads,
  `git log|diff origin/<base>...<head-sha>`, `git merge-tree --write-tree`. Never
  close, comment, label, push or edit; never modify a worktree; never `git stash`;
  never switch or restore the main checkout.
- No builds, test runs, dev servers or installs — static reading only.
- Task ids in the title or body: report their state from the repo's tracker
  (vitrinka MCP for a bound workspace).

## Vocabulary

| Field | Values |
|---|---|
| Verdict | `MERGE` ready or nearly, at most a small rebase or CI fix · `KEEP` valuable, needs real work — name it · `SALVAGE` only named commits or files are worth it: move them to a fresh PR off the base, then close · `CLOSE` superseded, obsolete or no remaining value |
| Validity | `current` · `partly superseded by <PR/sha>` · `superseded by <PR/sha>` · `obsolete: <why>` |
| Danger | `low` · `medium` · `high`, each with its reason: the risk of merging as-is — money, auth, permissions, migrations, deploy or production config, deletions, size, conflicts with active PRs, an unresolved security finding |
| Priority | `P0` fixes a bug live on a production ref · `P1` fixes a bug on the base, or unblocks a release · `P2` feature or improvement · `P3` chore, docs, tests only |

Production refs: census `prodRefs`. P0 requires the bug
shown in a production ref's file content, not a commit message.

## Analyst brief

Per PR, with evidence:

1. What it changes; for a big diff, `git diff --stat` then representative files
   per area.
2. What the base already has: later PRs or commits delivering the same — compare
   file contents on `origin/<base>`, never commit messages alone.
3. Unique value left, judged against today's base (paused areas, redesigned
   screens, replaced infra).
4. Merge cost: the conflicting files and their nature, CI, review threads, size.
5. Local state: unpushed or uncommitted work in its worktree.
6. Overlap with the active PRs.

CLOSE only for value you located elsewhere; value you could not place is SALVAGE
or KEEP.

## Skeptic brief

Refute the analyst's verdict with independent evidence:

- CLOSE / SALVAGE: hunt for missed unique value — check 2–3 concrete changes
  against the base's file contents; check every salvage item fixes a bug the BASE
  has, not one the PR's own branch introduced; check the worktree for unpushed or
  uncommitted work.
- MERGE / KEEP: hunt for obsolescence or harm — superseded later, a paused or
  removed area, a regression against what the base changed since, conflicts
  worse than claimed, CI red for a real reason, an ordering constraint.

Re-check every cheap factual claim (superseding PR or sha, conflict files, task
states). `upheld: false` with the corrected verdict when the evidence points
elsewhere.

## Output schema

Analyst, per PR: `pr`, `title`, `whatItDoes`, `supersededBy`, `uniqueValue[]`,
`overlapWithActive`, `mergeCost`, `localState`, `linkedTasks`, `verdict`,
`validity`, `danger`, `priority`, `confidence` (high · medium · low),
`nextStep`, `evidence[]`.

Skeptic, per PR: `pr`, `upheld`, `finalVerdict`, `validity`, `danger`,
`priority`, `confidence`, `refutation`, `missedFacts[]`, `correctedNextStep`.
