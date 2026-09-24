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
| `mergeprecheck` | required bot reviewers (incl. eve advisory COMMENTED verdicts), login canonicalisation, enum config keys, gates |
| `prevents` · `githubio` | event computation and snapshot shaping, argv construction, flag parsing |
| `synccontext` | local-vs-remote DB url detection, globs, `--freeze`, the verb end to end |
| `classifypaths` · `tddclassify` | commit bundling and TDD classification |
| `worktree` · `reporoot` · `listprs` · `beforereview` | path, listing and hook helpers |
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
