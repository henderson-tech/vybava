# gitkit — the git family's deterministic layer

The `prm`, `push-all` and `sync` skills never embed git/GitHub logic in prose.
They call `vybava gitkit <script> [args]`: PR selector parsing, review-thread
triage, merge preconditions, worktree resolution, path classification and
DB-url safety.

```sh
vybava gitkit --json                 # list verbs
vybava gitkit doctor --json          # same list; nothing else to check
vybava gitkit resolve-fetch 42 --repo "$PWD"
vybava gitkit pr-events 42 --every-seconds 60 --repo "$PWD"   # under Monitor
vybava gitkit pr-extensions --stage ensure-pr --repo "$PWD"   # the repo's prm extensions
```

## Contract

- **The verb is the interface, never a path.** A skill names `gitkit <script>`;
  a verb keeps its argv grammar, stdout, stderr notes and exit codes, so no
  skill changes when its implementation does.
- The verbs are Go in `internal/gitkit/`, one file per verb, registered in
  `native.go`. They began as Node TypeScript and were ported byte-for-byte
  (PR #91): JSON key order is struct field order, and `native.go` carries the
  Node-compatibility helpers the port depends on — never bypass them:
  - `execFile` fails as `execFileSync` did (`Command failed: <argv>\n<stderr>`,
    `spawnSync <cmd> ENOENT|ETIMEDOUT|ENOBUFS`), echoes child stderr only
    where the script used Node's default stdio, and caps output at
    `maxBuffer` (Node's 1 MiB unless the verb set more).
  - `writeJSON` is `JSON.stringify(v, null, 2)`; `jsString`/`jsSlice` carry
    free text (comment bodies) cut at UTF-16 units and escaped as JS does.
  - `jsNumber` / `positiveInt` / `jsParseInt` read argv as `Number()` /
    `Number.parseInt()` did, so `0x7` and ` 7 ` still select PR 7.
  - `repoRoot` resolves the `--repo` / `GIT_SKILL_REPO` anchor once per
    invocation; a bad anchor fails loudly, never falls back to cwd.
- Script verbs never parse flags — every argument belongs to the verb — and
  run in-process; the verb's return value is the exit code.
- ⚠️ The ported verbs keep their Node grammar: they look up the flags they know
  and IGNORE the rest, `--help` included, so a mistyped flag runs the default.
  That leniency is frozen by the byte-for-byte contract, not endorsed. A verb
  born in Go (`pr-extensions`) validates its argv instead: an unknown argument
  or a bad value is `GITKIT_BAD_ARGS` (exit 2, a runx envelope under `--json`,
  `✗ CODE: detail — fix` otherwise) and `--help` prints real help. New verbs
  follow that, never the ported pattern.

## Tests

Each verb has a `<verb>_test.go` ported from the TypeScript spec — the
behaviours not obvious from the code (which findings get filtered, which
threads count as self, when the teardown hook must emit nothing). These
verbs run unattended inside `/prm --auto`, where a regression silently
mis-triages review comments or hands the teardown hook the wrong directory.
Every guard exists because something went wrong live — add the case first.

```sh
go test ./internal/gitkit/...
```

| Test | Covers |
|---|---|
| `resolvefetch` | PR selector forms, bot filtering, self/resolved thread rules, UTF-16 body cuts |
| `mergeprecheck` | required bot reviewers (incl. eve advisory COMMENTED verdicts), login canonicalisation, worktree-teardown guard, merge-method derivation from buttons + base-branch rules, enum config keys, gates |
| `prevents` · `githubio` | event computation and snapshot shaping, argv construction, flag parsing |
| `synccontext` | local-vs-remote DB url detection, globs, `--freeze`, the verb end to end |
| `classifypaths` · `tddclassify` | commit bundling and TDD classification |
| `worktree` · `reporoot` · `listprs` · `beforereview` | path, listing and hook helpers |
| `prextensions` | extensions come only from `origin/<default>` (never a PR branch, an uncommitted edit, an untracked draft or an unpushed commit), `--stage` filtering, `.local` switch-off, every malformed file and symlink refused, strict argv (`GITKIT_BAD_ARGS`, real `--help`) |
| `native` | `execFile`'s Node failure modes, `Number()` parsing |

Mutating `github-io` subcommands are tested on argv construction only; never
run them against GitHub from a test. Fixtures use `acme/app`-style
placeholders and RFC 5737 IPs, never a real repo or host.

## Per-repo configuration

`<main clone>/.claude/.claude.git.config` (committed) overlaid by
`.claude.git.config.local` (gitignored) carries `DEFAULT_BRANCH`,
`MERGE_POLICY`, `REQUIRED_BOT_REVIEWERS`, `AFTER_MERGE_CMD`,
`BEFORE_REVIEW_CMD`, `GENERATED_PATHS` and the rest; the `prm` skill's
`references/merge.md` documents every key.

### `PR_EXTENSIONS` — a repo's own steps inside prm

`PR_EXTENSIONS=<glob>` names markdown files prm reads and follows at a stage of its
flow (`ensure-pr`, `round`, `merge`) — the hook for a project step that needs the
model, which the shell hooks cannot carry. `pr-extensions` lists and validates them;
a glob matching nothing or a malformed file exits 1, because a repo that ships an
extension expects it to run. The key and the files are read as git blobs at
`origin/<default branch>` and the instructions travel in the output: prm executes
an extension with full tool access, so a working-tree copy — a PR branch checked
out, an uncommitted edit, a symlink (refused) — would let any PR write the steps prm
then runs on it. Contract: `skills/prm/references/extensions.md`.

### The merge method is read, never assumed

`merge-precheck` answers `mergeMethod` for an open PR from what GitHub will
actually accept into its base branch — the repository's merge buttons AND the
base's effective rules (org + repo rulesets, classic protection), fetched in
the same GraphQL round as the bot gate:

- an explicit `MERGE_METHOD` the base permits wins → `mergeMethodSource: "config"`;
- otherwise the first permitted of merge → squash → rebase → `"repository"`.
  `required_linear_history` refuses merge commits and a `pull_request` rule's
  `allowed_merge_methods` narrows the set, so a henderson-tech repository
  lands as squash with no key even while its merge-commit button is still on;
- a `MERGE_METHOD` the base refuses falls back and is echoed as
  `mergeMethodInvalid`.

`mergeMethodReason` names the rule behind the choice; `mergeMethodsAllowed`
is the permitted set. Unreadable settings or rules, or a base that permits no
method at all, exit 1 with the cause and fix — the precheck never guesses.
The buttons-only reading chose `merge` on a linear-history repository and
GitHub refused the merge (semafor#3, 2026-09-24).
