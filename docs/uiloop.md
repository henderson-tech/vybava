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
ui-loop publish     --follow [--from user@host:path] [--interval 30s] [--until-idle 10m] [--pass N] [--areas a,b] [--retries 3]
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
  lint: {
    grid: 4, touchTarget: 44, off: ['repeated-text'], ramp: [11, 12, 14, 16, 20, 24, 30],
    // Rule id → selectors: a hit on a matching element, or inside one, counts as info, not a defect.
    allow: { grid: ['ui-button', '.ui-badge-small', '[role=menuitem]'] },
  },
  vitrinka: { project: 'powerflow', boardPrefix: 'ui-polish' },
  publish: { from: 'devops:ws/pwf/pwf-ui' },  // the repo on the capture box, for publish --follow
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

`lint.allow` takes defect rules only (an informational rule is refused), each with at least one selector. It exists for a spec that allows an off-grid value in named places only: pwf-ui allows Tailwind half steps (6/10 px) inside primitive recipes, and without it the grid rule flagged 6,033 hits on 38 shots, nearly all primitives and chrome. An allowed hit is still counted, under `info` in the shot record, so the report shows how much the allowlist absorbs. An invalid selector fails the lint loudly on the first shot.

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
- `full: false`, `knownIssues`, `unreachable` and `destructive`;
- `once: true`: shot only at the first viewport × theme the run selects, for a recipe with a side effect that must not repeat per shot (a real failed login that counts against a lockout).

**`Step`** is one of `goto`, `click`, `clickText`, `clickRole` (topmost overlay first), `check`, `uncheck`, `selectOption` (`{ selector, value }`, a native `<select>` option by value or label), `wait` (ms, capped at 2000; prefer `waitFor`), `fill`, `waitFor`, `press`, `hover`, `dblclick`, `longPress`, `drag`, `evaluate` or `upload`. `clickText` takes the exact label (a substring is the last resort) or a `/regex/flags` string; `check` reports an invalid regex and a negative `wait`.

A recipe function mixes steps with code through `runSteps(page, steps, ctx)`, exported from `vendor/capture.ts` (not `manifest.ts`, which stays free of runtime imports so `check` and `map` never load Playwright). A failing step throws `StepFailed`, which the shot records as `recipe-failed` with the step and its index.

**`defineProject`** takes:

- `params(run)`, resolved once per pass into `params.json`. A screen that names a missing param is recorded `unreachable`.
- `prepare(run)`: idempotent and never destructive. It is skipped on `--resume`.
- `login(page, as, app, run)`. It either returns a storage-state path or signs in on the page; the harness then saves the state.
- `signedOut(page, as, app)`. True when a signed-in screen landed on the sign-in page (an expired session): the shot is `recipe-failed`, never `ok`. Every run, `--resume` included, signs in fresh, so a resume re-takes it.
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

- **Pass numbers** start at 1. `--pass 0` (or below) is refused with `SELECTION_INVALID` on every verb, never read as "not given". The next pass is the latest plus one. A pass that holds no shots yet (a `--print` whose command never ran) is reused instead. `--resume` defaults to the latest pass and retakes everything that is not `ok` or `unreachable`.
- **Shot layout:** `shots/<id>/<viewport>.<theme>.{png,full.png,json}` in the pass directory. The full companion is only taken when the page scrolls, capped at 6000 CSS px.
- **Other pass files:** `params.json`, `auth.json` and `.auth/`, plus `report.json`, `report.md` and `done.json` (written by the global teardown, `done.json` last: `{ v, pass, run, finishedAt, shots }`, where `run` is the `createdAt` of the run.json it finished). The report sums defects per shot, so one sidebar defect counts once per screen, and beside it counts each rule + element path + detail once across the pass (`lintUnique`, `defectsUnique`), and lists the top repeated offenders (`offenders`: rule, path, detail, screens, shots).
- **Statuses:** a shot is `ok`, `recipe-failed` (still shot), `theme-mismatch`, `build-error` (a red dev-server overlay that never turned green within `--build-wait`), `unreachable` or `error` (a harness failure, which also fails the test).
- **Lint rules** (`LINT_RULES` in `lint.ts`), in two groups:
  - Defects: `h-scroll`, `text-clipped`, `text-spill`, `grid`, `type-ramp` (with `lint.ramp`), `touch-target` (mobile viewports), `safe-area`, `contrast`, `glass-on-content`, `nested-surface` and `glass-blur`.
  - Listed for judgement: `h-scroller`, `truncated` and `repeated-text`.

The shot record (`capture.ts` `ShotRecord`, v1) is the contract that `split`, `publish` and `scoreboard` read (`internal/uiloop/record.go`). Its `lint.distinct` (defect rule → the distinct `{ path, detail }` the shot hit, up to 1000 per rule) feeds the unique counts. It is additive: a record without it (a harness before v0.23) makes the unique columns unknown, never zero.

### On a Devbox

The capture must run next to the app, never on the Mac. The container needs the repo and its node_modules, but not vybava.

1. **Print the command.** On the Mac, in the repo: `vybava ui-loop check && vybava ui-loop run --print --json`. This writes run.json into the synced tree. The `--print` output also shows the `devbox run -- '<cmd>'` line.
2. **Run it in the workspace.** Use `vybava ui-loop run --wrap "devbox run --max-wait 45m -- {cmd}"`, or run the printed line yourself. Set the app's `env` var inside the command when the container reaches the app on another address.
3. **Publish beside the capture.** Devbox sync is one-way (Mac → box), and the box has no vitrinka CLI or token, so on the Mac, next to the running capture, run `vybava ui-loop publish --follow`. Every `--interval` (30 s) it rsyncs the pass back from `--from` (default `publish.from` + `/<out>/pass-<n>/`, e.g. `devops:ws/<workspace>/<app-dir>/.ui-loop/pass-3/`, with `<workspace>` from `devbox url --json`). It never fetches `.auth/` (storage states), `playwright/` or `run.json`. It then adopts and pushes every shot that became final, one push per area set that gained files. It stops once the run's `done.json` has arrived and no new shot has for `--until-idle` (10 min). It is idempotent through the ledgers and the index, so a re-run after a stop or a crash picks up where it left off. Without `done.json` (a capture killed before its teardown) it gives up after 3 × `--until-idle` with no new shot (`RUN_UNFINISHED`); finish with `run --resume` and follow again.
4. **Score on the Mac:** `ui-loop scoreboard`. `split` and a plain `publish` still work on a pass fetched by hand (`rsync -a devops:ws/<workspace>/<app>/<out>/pass-<n>/ <out>/pass-<n>/`).

A shot is **final** when its captures are on disk and the running capture will not rewrite it: it was taken by this run (`capturedAt` at or after run.json's `createdAt`, so keep the box's clock in sync), it has a status `--resume` keeps (`ok`, `unreachable`), or the run is done. A `--resume` re-run writes a new run.json, so the previous run's `done.json` no longer counts.

Follow is progressive through vitrinka's own detached push. The set root keeps its `.vitrinka` descriptor, so every `board capture` fires a detached `board push` that uploads only the files the deployment lacks, and the shot joins its area's board within seconds. That push reports no exit status or URL, so after adopting, each tick also pushes every area set that gained files. This backstop push commits the set's metadata and records its URL and status in the index. The two pushes coexist by vitrinka's design. Both read the server's have-list and only add. A detached push that is still running re-pushes for every capture that marked the root, so the last push to land always holds every shot. An overlap costs at most a re-upload of the files in flight at that moment. A failed push is retried on the next tick.

#### The sync trap: ignore the capture's output

When `<out>` sits inside a checkout that Devbox syncs one-way, the next sync deletes everything the box wrote there mid-run. It removes `shots/`, `.auth/`, `params.json` and `auth.json`, and the next test fails with "no signed-in session". The app that syncs the repo must list these in its `sync_ignores`, so that only `run.json` (written on the Mac) syncs:

```yaml
sync_ignores: [/.ui-loop/*/shots, /.ui-loop/*/.auth, /.ui-loop/*/auth.json, /.ui-loop/*/params.json,
  /.ui-loop/*/report.json, /.ui-loop/*/report.md, /.ui-loop/*/done.json, /.ui-loop/*/review, /.ui-loop/*/fix,
  /.ui-loop/*/*.tmp-*, /.ui-loop/*/playwright]
```

Replace `.ui-loop` with your `out`. Every app that syncs the repo needs them. That includes a sibling workspace's `sync: sibling:<repo>` app, such as pwf-docker-compose's `pwf-ui`, next to the repo's own `devbox.yaml`. `ui-loop check` warns (`DEVBOX_SYNC`) when a `devbox.yaml` in the repo or a sibling directory has an app that syncs the repo without them. A `devbox.worktree.yaml` beside the recipe is folded in: its `sync` wins and its ignores add.

#### Login secrets on the box

A `login` that types a password reads it from the environment. Getting it there takes three steps:

1. **Push the env.** Push it to the workspace from the vault, never from a file: use onyx `run_command` with `env_refs` that map each `NAME` to its onyx ref, running `devbox env push <ws> <app> --from-env NAME…`. The app must be a real unit (one with a `cmd`). A `source_only` app carries no `env_file`, so the push has nowhere to land.
2. **Source it in the capture command.** The capture command sources the file with auto-export: `set -a; . ~/ws/<ws>/env/<file>; set +a; <capture command>`.
3. **Put node and pnpm on the PATH.** The box's mise has no global node or pnpm. Run the command under `mise x node@<v> pnpm@<v> -- sh -c '…'`, or export `PATH` from `mise x node@<v> pnpm@<v> -- printenv PATH` first.

## Split and publish

`split` plans **one vitrinka set per area**, in config area order. A set key is a board, so an area has ONE board across every pass: its key is `<boardPrefix>-<area>`, and each pass adds its shots and its `Pass N · <viewport> · <theme>` sections to it. Every viewport × theme of the area goes into that one set.

- **Images and notes:** only `ok`, `theme-mismatch` and `build-error` shots become images. Every other status (`recipe-failed`, `unreachable`, `error`) is listed per area under `notes` in the plan and in `publish/index.json` (`id`, `viewport`, `theme`, `status`, `step`, `error`), for the review-loop publisher to render as a text card. A `recipe-failed` shot is a picture of wherever the recipe died, usually the same sign-in page.
- **Keys** are `<boardPrefix>-p<n>-<area>`, capped at 64 characters with a digest. **Titles** are `<boardPrefix> · <area> · pass <n>`.
- **Order:** files are grouped by viewport, then theme, in the order `apps.<app>.viewports` × `themes` lists them. Inside each group they follow screen (manifest) order, with a shot's full capture right after its viewport capture. A plain `publish` adopts in this order, so the set's manifest follows it too. Under `--follow` shots are adopted as they become final, so the manifest is in arrival order, and the sections below carry the layout.
- **Sections:** each set in `plan.json` and each row in `publish/index.json` carries `sections: [{title: "<viewport> · <theme>", viewport, theme, labels: [...]}]`, in set order. The review-loop publisher lays out one board section per entry from its labels.
- **Size:** vitrinka 5.13 syncs a screenshot set file by file (the per-file door), so a set has no byte cap and holds up to 20,000 files, its manifest included (`ingest.MaxSetFiles`). An area above that is not planned: `split` reports `SET_TOO_LARGE`. Split the area in the config.
- **Labels** are `P<n>-<ID>-<VIEWPORT>-<THEME>[-FULL]`, capped at 40 characters.
- **Captures** carry `--device <viewport>`, `--viewport <W>x<H>@2` and `--state <theme>[ · as <user>][ · <app>]`, so every shot names its section on the board.
- **Captions** carry the status and the lint defects, worst first.
- **Determinism:** the plan is identical for an identical pass. It goes to `<pass>/publish/plan.json` (`v: 2`).

`publish` adopts each set under `<out>/sets/<key>`, one root per area shared by every pass (a set key is a board, so each pass adds its shots and its `Pass N · <viewport> · <theme>` sections to the same board), then pushes it:

1. `vitrinka board init --root --key --title --project`.
2. `board capture web --file … --label --title --route --url --note --src --state --device --viewport` adopts each file. The descriptor stays in place, so each capture fires vitrinka's detached per-file push and the shot shows up on the board while the rest adopt. A ledger at `publish/adopted/<key>` makes a re-run adopt only what is missing. It lives beside the set roots, never in one: `board push` refuses a root holding anything but images, `.boxes.json` sidecars and `manifest.json`.
3. `vitrinka board push --root --title --yes --no-input --no-render --json`, reading `data.url`. This commits the set and records its status.
4. A failed push is retried (`--retries`, default 3). A set that still fails is `failed` in the index, and `--sets <key>` retries it.

Outcomes go to `<pass>/publish/index.json`, with the notes beside the sets. A set already pushed with the same files is skipped unless `--force`.

**A pass published before one set per area** (chunked by area × viewport × theme into `…-<viewport>-<theme>-<n>` sets) is not re-adopted into those chunks. `publish` plans fresh area sets and adopts every file into them. Index rows whose key is no area's set move to `legacy` (`key`, `title`, `files`, `status`, `url`), and publish reports `LEGACY_SETS`. Their boards are never deleted here: that cleanup is the owner's call, and `legacy` is its list.

`publish.maxFiles` and `publish.maxBytes` are deprecated. An older config that still sets them decodes, and they are ignored with a `CONFIG_DEPRECATED` warning from `check`, `split` and `publish`. Delete them.

## Scoreboard and the review backlog

`scoreboard` folds the pass's records with its review backlog. The backlog comes from `--backlog`, else from `<pass>/review/backlog.json` when present. A backlog whose `pass` is not the scored pass is refused. The output is `scoreboard.json` and `scoreboard.md` in the pass directory. Each area counts:

- screens by their worst open finding: broken, needs-work, polish or clean;
- findings by status;
- lint defects per rule, raw (summed per shot) and unique (each rule + element path + detail once, `lintUnique` and `lintDefectsUnique`). Unique counts never sum across areas, because a chrome defect sits on every area's screens;
- console errors;
- shots that are not ok.

`offenders` lists the 20 defects seen on the most screens (2 or more), each with its rule, path, detail, screens and shots. The markdown shows the top 10. `uniqueKnown` is false when a record predates the distinct keys, and then the unique columns render `—`.

The delta against the previous pass comes from its `scoreboard.json` (or its records when it was never scored). Severity deltas appear only when both passes were reviewed, and unique deltas only when both carried distinct keys.

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
- **An e2e run never leaves seed data changed.** Back up first and reach states with request mocks. Destructive recipes run only with `--destructive`, in a separate Playwright project that starts after every read-only shot has finished, one at a time. A harness error in the read-only phase skips them. `listStates` children keep their base screen's `destructive` flag.
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
- `LINT_RULES` ↔ `LintRules` and `LintInfoRules`;
- the run.json and record shapes ↔ `run.go` and `record.go`, and `done.json` (`teardown.ts` `DoneFile`) ↔ `follow.go` `DoneFile`. Bump `RUN_VERSION`/`RECORD_VERSION` on a breaking change.

The harness must type-check under TS 5.3 strict, with `noUncheckedIndexedAccess`, `exactOptionalPropertyTypes` and `noPropertyAccessFromIndexSignature`, in both CJS and ESM packages. It must stay Node 20 compatible: no bun-only APIs, no `import.meta`, no `__dirname`.
