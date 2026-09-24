# gitkit — the git family's deterministic layer

The `prm`, `push-all` and `sync` skills never embed git/GitHub logic in prose.
They call `vybava gitkit <script> [args]`: PR selector parsing, review-thread
triage, merge preconditions, worktree resolution, path classification and
DB-url safety.

```sh
vybava gitkit --json                 # list scripts
vybava gitkit doctor --json          # node runtime + materialized payload
vybava gitkit resolve-fetch 42 --repo "$PWD"
vybava gitkit pr-events 42 --every-seconds 60 --repo "$PWD"   # under Monitor
```

## Contract

- **The verb is the interface, never a path.** A skill names `gitkit <script>`;
  porting a script to Go keeps the verb and its output, so no skill changes.
- The scripts are zero-dependency, erasable TypeScript in
  `internal/gitkit/ts/bin/`, embedded in the binary and materialized once per
  content digest under `<user cache>/vybava/gitkit/<digest>/bin` (they import
  each other by relative path).
- Script verbs never parse flags — every argument belongs to the script — and
  `exec` node in place of the `vybava` process, so a Monitor's signals and the
  script's exit code pass through untouched.
- Node must strip types natively: 22.18+, 23.6+ or 24+. Missing or older node
  answers `GITKIT_NODE_MISSING` / `GITKIT_NODE_TOO_OLD` with the fix.

## Tests

`internal/gitkit/ts/tests/` is the scripts' spec — the behaviours not obvious
from the code (which findings get filtered, which threads count as self, when
the teardown hook must emit nothing). CI runs it on Node 24.

```sh
node --disable-warning=ExperimentalWarning --test internal/gitkit/ts/tests/*.test.ts
```

Run it after editing anything in `ts/bin/`: these scripts run unattended inside
`/prm --auto`, where a regression silently mis-triages review comments or hands
the teardown hook the wrong directory. Every guard exists because something went
wrong live — add the case first.

| Test | Covers |
|---|---|
| `resolve-fetch` | PR selector forms, bot filtering, self/resolved thread rules |
| `merge-precheck` | required bot reviewers (incl. eve advisory COMMENTED verdicts), login canonicalisation, worktree-teardown guard |
| `pr-events` · `github-io` | timeline shaping, flag parsing, API I/O edges |
| `sync-context` | local-vs-remote DB url detection |
| `classify-paths` · `tdd-classify` | commit bundling and TDD classification |
| `worktree` · `repo-root` · `list-prs` | path and listing helpers |

Fixtures use `acme/app`-style placeholders, never a real repo.

## Per-repo configuration

`<main clone>/.claude/.claude.git.config` (committed) overlaid by
`.claude.git.config.local` (gitignored) carries `DEFAULT_BRANCH`,
`MERGE_POLICY`, `REQUIRED_BOT_REVIEWERS`, `AFTER_MERGE_CMD`,
`BEFORE_REVIEW_CMD`, `GENERATED_PATHS` and the rest; the `prm` skill's
`references/merge.md` documents every key.
