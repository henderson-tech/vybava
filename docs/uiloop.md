# ui-loop — the UI polish loop's harness and passes

`ui-loop` is the deterministic layer of the UI polish loop that polished Reservine (vt-1339) and voke:

1. **Map** every screen and its states in a typed manifest.
2. **Capture and lint** each one per viewport and theme.
3. **Review** it against a written spec.
4. **Fix** primitives first, then areas.
5. **Verify** in the next pass, with a scoreboard.

The applet owns what must not depend on judgment:

- the vendored TypeScript/Playwright harness and its drift gate;
- the pass directory and its `run.json`;
- the split into vitrinka sets and the publish;
- the scoreboard.

The orchestration (reviewers per area, synthesis, fix lanes, boards) lives in the vitrinka `map` and `review-loop` workflows, which drive this CLI. There is no Výbava skill for it.

It is framework-agnostic: it drives only Playwright and the DOM. It has been run against Angular (pwf-ui: node 20, pnpm, TS 5.3, Playwright 1.58) and React (voke: bun, Playwright 1.62) shapes.

```text
ui-loop init        [--json]   scaffold <dir> (project.ts, screens/), the spec template and the out gitignore line; sync
ui-loop sync        [--force]  write the embedded harness into <dir>/vendor + STAMP.json (version, sha256 per file)
ui-loop check       [--no-ts]  vendor drift · project.ts · manifest validation · app-map freshness, each reported separately
ui-loop map                    render uiLoop.appMap from the manifest
ui-loop run         [--app a,b] [--only id,area,prefix*] [--viewports v,…] [--themes light,dark]
                    [--destructive] [--resume] [--pass N] [--workers 2] [--build-wait 300]
                    [--print] [--wrap "devbox run -- {cmd}"]
ui-loop split       [--pass N] [--areas a,b]
ui-loop publish     [--pass N] [--areas a,b] [--sets key,…] [--retries 3] [--force] [--dry-run]
ui-loop scoreboard  [--pass N] [--backlog FILE] [--previous N] [--no-delta]
```

Every verb emits the `{v, ok, verb, data, diagnostics, next}` envelope (`--json` for machines). The output of the commands a verb runs streams to stderr. Exit codes: 0 ok, 1 infra, 2 diagnostics. The codes are the closed enum in `internal/uiloop/diag.go`.

## Config: `uiLoop` in vybava.config.ts

Typed by `UiLoopConfig` in `.vybava/config.ts`. Unknown keys are rejected, and every problem is reported at once.

```ts
uiLoop: {
  dir: 'tests/ui-loop',            // project.ts, screens/, vendor/ (synced, committed)
  out: '.ui-loop',                 // gitignored run root: <out>/pass-<n>
  appMap: 'docs/app-map.md',       // rendered by `ui-loop map`, drift-checked by `ui-loop check`
  spec: 'docs/ui-spec.md',         // the written spec reviewers judge against (init scaffolds it)
  runner: 'pnpm exec playwright test',
  tsRunner: 'pnpm dlx tsx',        // default: `bun` when runner starts with bun, else `npx --yes tsx`
  areas: ['tasks', 'dashboards'],  // capture, publish and scoreboard order
  apps: {
    portal: { baseUrl: 'http://10.8.0.10:21782/portal/', env: 'PORTAL_URL', viewports: ['phone', 'tablet', 'desktop'], themes: ['light', 'dark'] },
    designer: { baseUrl: 'http://10.8.0.10:21781/designer/', viewports: ['desktop', 'laptop'], themes: ['light', 'dark'] },
  },
  viewports: { 'phone-se': { width: 375, height: 667, mobile: true, insets: { top: 20, right: 0, bottom: 0, left: 0 } } },
  lint: { grid: 4, touchTarget: 44, off: ['repeated-text'], ramp: [11, 12, 14, 16, 20, 24, 30] },
  vitrinka: { project: 'powerflow', boardPrefix: 'ui-polish' },
  publish: { maxFiles: 96, maxBytes: 4_000_000 },
}
```

Built-in viewports, all DPR 2. The insets are top/right/bottom/left in px; `mobile` means touch, a phone/tablet UA and the touch-target lint.

| Id | Size | Mobile | Insets |
|---|---|---|---|
| `phone` | 390×844 | yes | 59/0/34/0 |
| `phone-320` | 320×568 | yes | 20/0/0/0 |
| `phone-360` | 360×780 | yes | — |
| `phone-landscape` | 844×390 | yes | 0/47/21/47 |
| `tablet` | 820×1180 | yes | — |
| `laptop` | 1024×768 | no | — |
| `desktop` | 1440×810 | no | — |

## The manifest contract

A repo owns `<dir>/project.ts` (`defineProject`) and `<dir>/screens/*.ts` (`defineScreens`). The vendored `manifest.ts` and `project.ts` are the API.

**`Screen`** holds:

- `id` (kebab-case, unique), `area`, `kind`, `title`, `route` (with `{PARAM}` placeholders) and `sourceFiles` (they must exist);
- `app`, required when the config has more than one app;
- `state`, exactly on `kind: 'state'`;
- `parentId` and `variantOf`;
- `as`, the identity from `login`;
- `viewports` and `themes`, which override the app's set;
- `before`, a Recipe run before navigation (request mocks);
- `open`, as steps or a Recipe (drives into the overlay or state and ends waiting for it);
- `ready`, a selector or a Recipe;
- `context` (timezone, locale, localStorage);
- `full: false`, `knownIssues`, `unreachable` and `destructive`.

**`Step`** is one of `goto`, `click`, `clickText`, `clickRole` (topmost overlay first), `fill`, `waitFor`, `press`, `hover`, `dblclick`, `longPress`, `drag`, `evaluate` or `upload`.

**`defineProject`** takes:

- `params(run)`, resolved once per pass into `params.json`. A screen that names a missing param is recorded `unreachable`.
- `prepare(run)`: idempotent and never destructive. It is skipped on `--resume`.
- `login(page, as, app, run)`. It either returns a storage-state path or signs in on the page; the harness then saves the state.
- `theme(context, theme, app)`, `readTheme(page)`, `settle(page, screen)` and `chrome` (selectors of app chrome that paints its own background).

`states.ts` reaches empty, error and loading without touching seed data:

- `emptyList(url)`, `failingList(url, status?, body?)`, `heldList(url)`, `patchJson(url, fn)`;
- `echoWrites(url)`, so preferences never persist;
- `all(...)`;
- `listStates(base, url)`, which yields `<id>-empty`, `<id>-error` and `<id>-loading`.

`check` validates each screen's area, app, viewports, themes, kind and state against the config, and that its sourceFiles exist.

## A pass

`run` writes `<out>/pass-<n>/run.json`. It holds the resolved apps (an app's `env` var set on the Mac already overrides `baseUrl`), every viewport, the selection, the lint knobs, the workers and the build wait. Then it runs the command from the repo root:

```sh
UILOOP_ROOT="$PWD" UILOOP_RUN="$PWD/.ui-loop/pass-3/run.json" pnpm exec playwright test -c "$PWD/tests/ui-loop/vendor/playwright.config.ts"
```

The repo needs no Playwright config of its own for the loop.

- **Pass numbers:** the next pass is the latest plus one. A pass that holds no shots yet (a `--print` whose command never ran) is reused instead. `--resume` defaults to the latest pass and retakes everything that is not `ok` or `unreachable`.
- **Shot layout:** `shots/<id>/<viewport>.<theme>.{png,full.png,json}` in the pass directory. The full companion is only taken when the page scrolls, capped at 6000 CSS px.
- **Other pass files:** `params.json`, `auth.json` and `.auth/`, plus `report.json` and `report.md` (written by the global teardown).
- **Statuses:** a shot is `ok`, `recipe-failed` (still shot), `theme-mismatch`, `build-error` (a red dev-server overlay that never turned green within `--build-wait`), `unreachable` or `error` (a harness failure, which also fails the test).
- **Lint rules** (`LINT_RULES` in `lint.ts`), in two groups:
  - Defects: `h-scroll`, `text-clipped`, `text-spill`, `grid`, `type-ramp` (with `lint.ramp`), `touch-target` (mobile viewports), `safe-area`, `contrast`, `glass-on-content`, `nested-surface` and `glass-blur`.
  - Listed for judgement: `h-scroller`, `truncated` and `repeated-text`.

The shot record (`capture.ts` `ShotRecord`, v1) is the contract that `split`, `publish` and `scoreboard` read (`internal/uiloop/record.go`).

### On a Devbox

The capture must run next to the app, never on the Mac. The container needs the repo and its node_modules, but not vybava.

1. **Print the command.** On the Mac, in the repo: `vybava ui-loop check && vybava ui-loop run --print --json`. This writes run.json into the synced tree. The `--print` output also shows the `devbox run -- '<cmd>'` line.
2. **Run it in the workspace.** Use `vybava ui-loop run --wrap "devbox run --max-wait 45m -- {cmd}"`, or run the printed line yourself. Set the app's `env` var inside the command when the container reaches the app on another address.
3. **Bring the pass back.** Devbox sync is one-way (Mac → box), so the pass directory stays in the workspace. Fetch it from the box host the way voke's `run-pass.sh` does: `rsync -a devops:ws/<workspace>/<app>/<out>/pass-<n>/ <out>/pass-<n>/`, with `<workspace>` from `devbox url --json`.
4. **Split, publish and score on the Mac:** `ui-loop split`, then `publish`, then `scoreboard`.

## Split and publish

`split` plans one vitrinka set per area × viewport × theme, in config area order.

- **Limits:** each set is chunked at `publish.maxFiles` (default 96; vitrinka rejects a set directory of more than 100 files) and at `publish.maxBytes` of source PNG bytes (default 4 MB; pushes time out at 30 s on a shared uplink). The adopted WebP is smaller, so the byte bound is conservative.
- **Pairs:** a shot's viewport and full captures stay in one chunk.
- **Keys** are `<boardPrefix>-p<n>-<area>-<viewport>-<theme>-<chunk>`, capped at 62 characters with a digest. vitrinka keys cap at 64, and the halving `b` needs room.
- **Labels** are `P<n>-<ID>-<VIEWPORT>-<THEME>[-FULL]`, capped at 40 characters.
- **Captions** carry the status and the lint defects, worst first.
- **Determinism:** the plan is identical for identical input. It goes to `<pass>/publish/plan.json`.

`publish` adopts each set under `<pass>/publish/sets/<key>`:

1. `vitrinka board init --root --key --title --project`.
2. The `.vitrinka` descriptor is held aside while `board capture web --file … --label --title --route --url --note --src --state --viewport` adopts each file. Otherwise every capture would fire a push. An `.ui-loop-adopted` ledger makes a re-run adopt only what is missing.
3. `vitrinka board push --root --title --yes --no-input --no-render --json`, reading `data.url`.
4. A failed push is retried (`--retries`, default 3). After that the set is halved once: the tail moves to `<key>b` and both halves are pushed.

Outcomes go to `<pass>/publish/index.json`. A set already pushed with the same files is skipped unless `--force`.

## Scoreboard and the review backlog

`scoreboard` folds the pass's records with its review backlog. The backlog comes from `--backlog`, else from `<pass>/review/backlog.json` when present. The output is `scoreboard.json` and `scoreboard.md` in the pass directory. Each area counts:

- screens by their worst open finding: broken, needs-work, polish or clean;
- findings by status;
- lint defects per rule;
- console errors;
- shots that are not ok.

The delta against the previous pass comes from its `scoreboard.json` (or its records when it was never scored). Severity deltas appear only when both passes were reviewed.

The review-loop writes the backlog. Its shape is strict, with unknown keys rejected:

```json
{
  "v": 1,
  "pass": 3,
  "findings": [
    {
      "key": "tasks-empty-state-cta",
      "screen": "tasks-my-new-empty",
      "area": "tasks",
      "severity": "needs-work",
      "status": "open",
      "title": "Empty queue offers no next step",
      "detail": "optional prose",
      "files": ["apps/portal/src/app/tasks/task-list.component.html"],
      "acceptance": "The empty queue names what lands here and links to start a request",
      "viewports": ["phone"],
      "themes": ["dark"],
      "shots": ["tasks-my-new-empty@phone.dark"],
      "refs": ["https://app.vitrinka.ai/…"]
    }
  ]
}
```

The fields follow these rules:

- **`severity`** is `broken`, `needs-work` or `polish`.
- **`status`** is one of:
  - `open`: found this pass;
  - `met`, `partly` or `not-met`: the verdict on a previous finding's `acceptance`, which reuses that finding's `key`.
- **Required:** `title` and `acceptance`. Every finding that is not `met` also names the `files` a fix lane edits.

## Operational rules

These cost real incidents. The review-loop workflow carries them into every lane brief.

- **Shoot on the GPU path.** `vendor/playwright.config.ts` launches Chromium with `--enable-gpu --use-angle=swiftshader`. Without it, `backdrop-filter` renders as a sharp ghost (or not at all) and glass is silently missing from every shot. Never drop the flags. The `glass-blur` probe fails loudly when they stop working.
- **Keep the shared dev server green.** Lanes share one dev server. Fix the provider first, and revert your own red edit after 5 minutes. Restarting a server on a red tree leaves no listener. A capture that meets a red overlay waits up to `--build-wait` and never records it as `ok`.
- **Take the locks.** One deploy lock and one e2e lock per workspace.
- **Never restart the capture container.** A pass cut short (oomd, a usage limit) is finished with `run --resume`, never by restarting.
- **An e2e run never leaves seed data changed.** Back up first, reach states with request mocks, and shoot destructive recipes last, only with `--destructive`.
- **Recover from a usage-limit cutoff this way:**
  1. Map the dirty files to lanes from the transcripts.
  2. Commit an in-flight map.
  3. Relaunch the successors with a checkpoint rule.
- **Keep workflows small:** at most 4 agents per phase and at most 10 per run, unless the owner asks for scale. Never SendMessage an agent a running workflow owns.
- **`tailwind-merge` drops a class.** `cn`/`cx` drop a `position` or `display` class that sits beside a recipe setting one (the sticky-toolbar incident). Put layout on a wrapper, or extend the recipe.
- **A focus ring follows its control's radius.** Grouped rows need an inset ring.
- **The owner's hand-test finds what captures miss.** Leave the app running after each fix round and ask for a hand-test.

## Changing the harness

Edit `internal/uiloop/harness/`, never a vendored copy. These tables are mirrored in Go, and tests keep them equal:

- `BUILTIN_VIEWPORTS` ↔ `viewports.go`;
- `LINT_RULES` ↔ `LintRules`;
- the run.json and record shapes ↔ `run.go` and `record.go`. Bump `RUN_VERSION`/`RECORD_VERSION` on a breaking change.

The harness must type-check under TS 5.3 strict, with `noUncheckedIndexedAccess`, `exactOptionalPropertyTypes` and `noPropertyAccessFromIndexSignature`, in both CJS and ESM packages. It must stay Node 20 compatible: no bun-only APIs, no `import.meta`, no `__dirname`.
