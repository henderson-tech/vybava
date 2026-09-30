# config — prm's `.claude/.claude.git.config` keys

Same `KEY=value` file `/sync` reads, in `<repo>/.claude/`:

```
# How a green PR reaches the default branch. review (default): the loop as written —
# rounds, watcher, human/bot approval. self: a solo-owner repo where the PR is the
# TRAIL, not a review — ensure-pr labels it `eve-ignore` (eve never reviews it), and
# once clean + CI + mergeable pass the merge is `--admin` immediately: no round, no
# Monitor, no hand-back "waiting on review". Still never past red CI, a conflict, a
# pending required bot, or a PR authored by someone else. Worktree rules unchanged.
MERGE_POLICY=self

# Which `gh pr merge` flag lands a green PR: merge (merge commit) | squash (one commit
# titled after the PR) | rebase. Unset → derived from what the base branch permits (merge
# buttons + linear-history / allowed-methods rules): merge where truly allowed, else
# squash, else rebase — squash-only and linear-history repos need no key. A value the
# base refuses falls back and is echoed as `mergeMethodInvalid`.
MERGE_METHOD=squash

# Head-glob overrides of MERGE_METHOD, first match wins (path.Match globs). Unset →
# promote/*:merge (promotions keep their history); empty → no override. A matched
# method the base refuses is a STOP, never a fallback.
MERGE_METHOD_BY_HEAD=promote/*:merge

# Production branches: claude-guards prod-merge blocks an agent's gh pr merge, gh api
# merge/ref write and git push landing on any of them (escape CLAUDE_ALLOW_PROD_MERGE=1,
# only on the user's go for that merge). Read from the MAIN clone. Unset → none.
PROD_BRANCHES=canary release master

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

# What a successful merge does to the branch's devbox workspace (step 4b). down
# (default): the merge takes the stack down — stop its apps, release its hold, then
# reap. reap: retire it only when gc would — a workspace with apps still running is
# reported and kept (for a repo that hand-tests main-bound stacks after merge). A
# typo → reap.
AFTER_MERGE_DEVBOX=reap

# Runs when prm ENTERS the review loop, and again at the start of each round.
# Same tokens. Must be idempotent and cheap. Stops processes we own (dev servers,
# bundlers, emulators) — never databases/containers, which stay warm.
BEFORE_REVIEW_CMD=/wk:pause {slug}

# Markdown files prm reads and FOLLOWS at a stage of its flow — for a project step that
# needs the model (drafting a release note from the diff), which a shell hook cannot do.
# One glob relative to the repo root. Key and files are read at origin/<default branch>:
# only merged instructions run, never a working tree or a PR branch's copy. Frontmatter:
# name, stage (ensure-pr | round | merge, or a list), description. Listed per stage by
# gitkit pr-extensions; contract in extensions.md.
PR_EXTENSIONS=.claude/prm/*.md

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
