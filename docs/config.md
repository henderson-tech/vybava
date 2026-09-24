# vybava config — the shared per-repo configuration

Every applet that needs repository-specific settings reads them from one
file at the repo root, `vybava.config.ts`, through `internal/vconfig`. The
file is TypeScript so a repo gets completion, types and the freedom to
compute its config; bun evaluates it and the applets consume the resulting
JSON. A repo without bun ships `vybava.config.json` with the same shape.

```text
vybava config init            # scaffold vybava.config.ts + .vybava/config.ts
vybava config check --json    # evaluate, verify the helpers match this binary — CI gate
vybava config show --json     # the evaluated document
```

`.vybava/config.ts` is generated: it carries `defineConfig` and the typed
helpers each section understands (`englishAsKey`, `pathKeys`, …). It is
committed, never edited, and `vybava config check` fails on drift so an
upgraded binary and an old helper file cannot disagree silently;
`vybava config init --force` rewrites it.

```ts
import { defineConfig, englishAsKey, pathKeys } from './.vybava/config';

export default defineConfig({
  lok: {
    catalogs: {
      mobile: englishAsKey('apps/client/locales/{locale}.json', ['en', 'cs'], {
        required: ['en', 'cs'],
        afterWrite: 'bun run i18n:types',
        // call: one name or a list; `*.T` matches `.T('…')` on any receiver (Go: l.T("Sites"))
        scan: { roots: ['apps/client', 'services/api'], call: ['t', '*.T', '*.N'], extensions: ['.ts', '.tsx', '.go'] },
      }),
      webDictionaries: pathKeys('apps/web/dictionaries/{locale}.json', ['en', 'cs', 'sk', 'uk'], {
        required: ['en', 'cs'],
      }),
    },
  },
});
```

Evaluation is cached next to the git metadata (`<git-dir>/vybava-config-cache.json`)
keyed by the file's size and mtime, so hooks and repeated calls pay bun's
~0.5 s once per edit. Sections use `DisallowUnknownFields`: a typo in a key
is a diagnostic, never a silently ignored setting. Adding a section for a
new applet means a Go struct in that applet's package, its TypeScript twin
in `internal/vconfig/config-helpers.ts`, and nothing else.

## Guard settings and discovery

```ts
guards: {
  noRead: ['apps/api/openapi.json', 'packages/api-client/generated/**',
    '**/translation-keys.d.ts', 'bun.lock'],
  maxDumpLines: 200,
  // Omit for built-ins; [] disables only the unbounded-output gate.
  unboundedCommands: ['docker logs', 'gh run view', 'git log', 'git diff',
    'git show', 'bun test', 'go test', 'bunx jest'],
  // Extra trees whose scripts may open and delete a webdriverio session
  // (simulator:appium-session-churn). Trailing slash = subtree, else a glob;
  // matched at any depth. Built-in: appium/support/, appium/adhoc/lib/,
  // appium/specs/, appium/**/*.spec.ts, e2e/.
  appiumSessionDirs: ['tools/sim-probes/'],
  // Most workers a local playwright/vitest/jest run may ask for
  // (machine:test-worker-cap); at least 1, default 2. Heavier suites run on
  // the Devbox, or a deliberate run sets CLAUDE_GUARDS_ALLOW_TEST_WORKERS=1.
  testWorkerCap: 2,
  // RE2 patterns over one local command segment; a match is refused unless
  // it runs through `devbox run -- '<cmd>'` (machine:devbox-only). A hand
  // test on this Mac sets CLAUDE_GUARDS_ALLOW_LOCAL_STACK=1.
  devboxOnly: ['^bun run (dev|run):(api|web|admin)\\b', '^bun run test(:|$)'],
  // Most booted simulators (machine:sim-cap, default 2) and Metro/next/API
  // dev servers (machine:dev-server-cap, default 3) this Mac may hold before
  // another start is refused. Escape: CLAUDE_GUARDS_ALLOW_MACHINE_CAP=1.
  simCap: 2,
  devServerCap: 3,
}
```

Globs are repository-relative: `**` spans zero or more directories; ordinary
components support `*`, `?`, and character classes. Config is loaded for each
guard invocation; failed loads are reported and never cached by guard rules.

```text
vybava config discover           # review a TypeScript snippet
vybava config discover --json    # candidates, reasons, inferred catalogs
vybava config discover --write   # fill absent top-level guards/lok sections
vybava config check --json       # helper drift fails; discovery drift warns
```

Discovery examines tracked regular files, skipping symlinks. Signals include
size above 256 KiB, more than 2000 lines, generated paths/headers,
`linguist-generated`, and formatter exclusions. Candidates are evidence for
review; no-read suggestions favor explicit generated paths and attributes,
coalescing generated directories into globs. JSONC formatter exclusions that
cannot be parsed produce a warning.

Sibling JSON catalogs varying by a locale filename/directory become one
`{locale}` pattern. An English catalog with over 90% key/value equality suggests
`english-as-key`; otherwise the suggestion is `path`. Plural suffixes are
detected, and the largest catalog suggests a required locale **as a guess**.
Review these policies before writing. A lone catalog cannot establish a locale
family. Existing catalogs and human splits are preserved.

`--write` never changes an existing section. For TypeScript it preserves the
existing default expression in a binding and adds a default export with only
missing sections; unsupported export shapes are refused. For JSON it inserts
only missing properties. It requires an initialized configuration.

Discovery lives in `internal/configdiscover`, composing Lok's ordered parser
with `vconfig`; putting it in the loader itself would create an import cycle.

## Merge settings

The `merge` section drives `merge-assist` ([merge-assist.md](merge-assist.md)):
generated paths with the command that rebuilds them, and migration
directories whose unmerged files get renumbered past the base. Catalogs are
not repeated here; the lok driver takes them from `lok.catalogs`.

```ts
merge: {
  generated: [{ paths: ['apps/api/openapi.json'], regen: 'bun run api:generate' }],
  migrations: [{ dir: 'apps/api/src/database/migrations', style: 'typeorm', check: 'bun scripts/ci/check-migration-timestamps.ts' }],
},
```

## Release-readiness settings

The `readiness` section is the per-project adapter of the `release-readiness`
skill, read by the `readiness` applet ([readiness.md](readiness.md)). It covers:
- the repos, with what production runs and the integration branch;
- the lane checkout and dev-stack verbs, and the heavy-job wrapper;
- the test suites and the device matrix;
- the merge order, and the release gates of the final phase.

Command strings take `{token}` placeholders, and `readiness check` rejects a
token its field does not define.
