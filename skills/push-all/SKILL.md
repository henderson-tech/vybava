---
name: push-all
description: "Use when the user asks to commit and/or push — 'push all', 'commit and push', 'commit everything', 'commit this'. Owns all commit doctrine: full-tree sweep by default, surgical single-change-set on request, commit-only when no push is wanted. Never fires without an explicit ask — the global never-commit-unasked policy stands."
---

# push-all — sweep the tree into Conventional Commits, then push

The user opted in by asking; don't re-ask before committing. Default scope is the
ENTIRE working tree — staged, unstaged, AND untracked, deliberately including other
sessions' work. Two narrower asks route inside the same doctrine:

- **"commit" without "push"** → commit only, skip §3.
- **"just this change-set" / "my changes"** → surgical: if anything is staged, that
  IS the selection (commit exactly it, split by topic, add nothing). Else pick the
  single most coherent bundle tied to the current branch/task; leave every other
  path uncommitted and LIST what was left.

## 1. Classify & exclude

`git status --short` + `git diff` + `git diff --staged`. Clean tree → say so; still
push if the branch has unpushed commits.

`vybava gitkit classify-paths --repo <ABS repo path>` →
`{ commit[], secrets[], artifacts[] }`. Pass `--repo` as a LITERAL absolute path —
never `$(git rev-parse --show-toplevel)`, which re-inherits the drifted cwd it
defends against; without it the classifier reads whatever repo the persistent shell
is parked in.

- **Never stage `secrets[]`** — env files (`.env`, `.env.*`; `.example`/`.sample`/
  `.template` ARE committed), `*.key/.pem/.p12/.pfx`, `id_rsa*`, `credentials*`,
  `secrets.*`, `.npmrc`/`.netrc`/`.pypirc`, `*.token`, service-account JSON.
- **Never stage `artifacts[]`** — `node_modules/`, test/coverage reports,
  `__pycache__/`, `.next/`, `.turbo/`, `.DS_Store`, `*.log`.
- Skipping is per-path, never a reason to stop the run. The classifier matches
  secret-shaped FILES only — `tokenizer.ts` is real source and gets committed; the
  pre-commit hook (never bypassed) is the backstop for secrets pasted inside code.

## 2. Bundle & commit

Group `commit[]` by topic: same feature directory/module → one bundle; tests travel
with the code they test; lockfile + its manifest together; pure docs → `docs:`;
config/tooling (eslint, tsconfig, CI yaml) → `chore(config):`; generated/vendored
output → own `chore:` bundle, flagged in the report. A change fitting no obvious
bundle → report it and let the user decide rather than dump it into `chore:`.

Messages: `type(scope): subject` — `feat`/`fix`/`chore`/`docs`/`refactor`/`test`/
`perf`/`style`; scope = leading dir or feature name; imperative, ≤72 chars, no
trailing period; body only when the WHY isn't obvious. Match the change's actual
shape (`fix:` for a bug fix, `refactor:` for a pure restructure).

**Task trailer (vitrinka).** In a repo onboarded with `vitrinka project
setup`, a `prepare-commit-msg` hook appends `Vitrinka-Task: vt-<id>` to
every commit on a branch (or `.worktrees/` path) carrying `vt-<id>`; the
subject prefix (`[vt-401] …` / `vt-401 …` / `vt-401: …`) appears only
when the project turned it on (`vitrinka project commits --subject on`;
style is workspace-wide: `vitrinka workspace commit-prefix <style>`).
Never write the trailer or the prefix by hand — the hook adds them, and it
never rewrites a trailer a human already wrote. When a commit must name a
DIFFERENT task than the branch, write that trailer yourself
(`Vitrinka-Task: vt-<other>`) and the hook leaves it alone. Merge, squash,
`fixup!` and `squash!` messages are never touched. The GitHub App's `push`
event turns the trailer into a `commit` ref on the task; nothing else is
needed to link a commit.

Commit serially: `git add <explicit-paths>` per bundle, then commit via HEREDOC:

```bash
git commit -m "$(cat <<'EOF'
feat(scope): subject

Body only when the why isn't obvious.
EOF
)"
```

Never `git add -A`/`.` · never `--amend` · never `--no-verify` · pre-commit hook
fails → fix the cause, NEW commit.

## 3. Push

- **Default-branch guard:** refuse to push from `main`/`master` UNLESS the repo is
  commit-to-main-by-design: the deploy-from-main infra repos (`devulinka-infra`,
  `produlinka-infra`, `webulinka-infra`), config/dotfiles repos with no PR workflow
  (`~/.claude` and the like), or any repo whose history is provably main-only —
  check, don't assume: `git log --oneline --first-parent -20` shows no merge commits
  AND `gh pr list --state all --limit 1` is empty. Anywhere else: stop after the
  commits, report them, say a branch is needed.
- `git push`; no upstream → `git push -u origin <branch>`. Rejected (remote ahead) →
  STOP, report ahead/behind, hand off to `/sync`. Never force, never
  `--force-with-lease`, never rebase to make it fit.
- Never open a PR — that's `/prm`.

## 4. Report

Commits grouped by bundle with the paths each swept (bundling must be visible, never
silent), then a "Skipped (not committed)" block — 🔒 secrets and 🗑 artifacts, one
path per line — so the user can `.gitignore` or hand-commit them, then the push
result with branch and remote. `commit[]` empty → "nothing but skipped paths —
committed nothing." Nothing is silently dropped: every uncommitted path appears.
