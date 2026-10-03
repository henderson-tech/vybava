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
- the scoreboard;
- the stage state the review-loop reads back: `state`, the review `batches`, `merge-review`, the fix `lanes` and the fix `checkpoints`;
- the pass leases that let identical review runs share a pass: one capture, claimed batches, one synthesizer and one publisher.

The orchestration (reviewers per area, synthesis, fix lanes, boards) lives in the vitrinka `map` and `review-loop` workflows, which drive this CLI. There is no Výbava skill for it.

It is framework-agnostic: it drives only Playwright and the DOM. It has been run against Angular (pwf-ui: node 20, pnpm, TS 5.3, Playwright 1.58) and React (voke: bun, Playwright 1.62) shapes.

```text
ui-loop init        [--json]   scaffold <dir> (project.ts, screens/), the spec template and the out gitignore line; sync
ui-loop sync        [--force]  write the embedded harness into <dir>/vendor + STAMP.json (version, sha256 per file)
ui-loop check       [--no-ts]  vendor drift · project.ts · spec lint lines · manifest validation · app-map freshness, each reported separately
ui-loop doctor      [--for capture|review|fix|verify]  preflight a stage: check · contract · apps · pass (· workspace · signin, skipped)
ui-loop map                    render uiLoop.appMap from the manifest
ui-loop run         [--app a,b] [--only id,area,prefix*] [--viewports v,…] [--themes light,dark]
                    [--destructive] [--resume] [--pass N] [--workers 2] [--build-wait 300]
                    [--print] [--wrap "devbox run -- {cmd}"]
ui-loop split       [--pass N] [--areas a,b]
ui-loop publish     [--pass N] [--areas a,b] [--sets key,…] [--retries 3] [--force] [--dry-run] [--owner ID] [--ttl 1h]
ui-loop publish     --follow [--from user@host:path] [--interval 30s] [--until-idle 10m] [--pass N] [--areas a,b] [--retries 3] [--owner ID] [--ttl 1h]
ui-loop scoreboard  [--pass N] [--backlog FILE] [--previous N] [--no-delta]
ui-loop state       [--pass N] [--cap 6] [--primitives dir,dir]
ui-loop batches     [--pass N] [--size 14] [--areas a,b] [--claim N --owner ID [--ttl 3h]]
ui-loop batches     [--pass N] --stall ID | --split ID [--claim N --owner ID [--ttl 3h]] | --block ID --reason TEXT
ui-loop merge-review [--pass N] [--owner ID] [--ttl 1h]
ui-loop lanes       [--pass N] [--primitives dir,dir] [--max 4]
ui-loop checkpoints [--pass N]
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
  source: ['apps', 'libs', ':(exclude,glob)**/*.spec.ts'],  // git pathspecs of the app source (default below)
  primitives: ['libs/ui-lib', 'libs/tailwind-preset'],      // lanes' default; a change here makes a verify a full reshoot
  review: { carryTolerance: 0.001 },  // share of a shot's pixels that may differ for its screen to carry the last review
}
```

`source` is the application source as git pathspecs (`:(exclude)…`, `:(glob)…` magic included; git does the matching). Only a change there stales a pass (see `state`'s `drift`). Empty, it is the whole repo from its top (`:(top)`, so a config root below it still sees the library beside it) minus `out`, `.vitrinka`, `dir`, `spec` and `appMap` (relative to the config root, like every pathspec without `:(top)`), every `*.md` and every `.claude/`. `state` relays the effective list as `config.source`. `primitives` are directory prefixes (`libs/ui-lib` matches `libs/ui-lib/…`): the default of `lanes --primitives` and `state --primitives`. `review.carryTolerance` is the fraction of a shot's pixels that may differ from the previous pass's for its screen to carry that pass's review (see Carry-forward below): default `0.001`, `0` asks for identical pixels, and it must stay below 1.

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

The spec states what the lint measures. `init` writes two lines from `lint.grid` and `lint.touchTarget` into it (`scaffold/ui-spec.md.tmpl`: "Spacing, control heights and icon sizes sit on the 4px grid." and "Touch targets are at least 44×44px on coarse pointers."). `check` renders both again from the current config and warns `SPEC_LINT_DRIFT` for each one the spec no longer holds, naming the line and the knob's value; whitespace and rewrapping do not count. A spec written in its own words warns until it carries the two lines too. No `spec` configured, or no file there, means no warning.

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
- `volatile`: selectors (with `{PARAM}` placeholders) of content that changes between identical renders — a relative time, a live counter, a random avatar — masked in both shots (Playwright `mask`) so it never moves the pixels; the lint still reads it, `check` refuses an empty selector, and a `{PARAM}` the pass cannot fill makes the screen `unreachable` before the browser starts, as one in its route does;
- `once: true`: shot only at the first viewport × theme the run selects, for a recipe with a side effect that must not repeat per shot (a real failed login that counts against a lockout).

**`Step`** is one of `goto`, `click`, `clickText`, `clickRole` (topmost overlay first), `check`, `uncheck`, `selectOption` (`{ selector, value }`, a native `<select>` option by value or label), `wait` (ms, capped at 2000; prefer `waitFor`), `fill`, `waitFor`, `press`, `hover`, `dblclick`, `longPress`, `drag`, `evaluate` or `upload`. `clickText` takes the exact label (a substring is the last resort) or a `/regex/flags` string; `check` reports an invalid regex and a negative `wait`.

A recipe function mixes steps with code through `runSteps(page, steps, ctx)`, exported from `vendor/capture.ts` (not `manifest.ts`, which stays free of runtime imports so `check` and `map` never load Playwright). A failing step throws `StepFailed`, which the shot records as `recipe-failed` with the step and its index.

**`defineProject`** takes:

- `params(run)`, resolved once per pass into `params.json`. A screen that names a missing param is recorded `unreachable`.
- `prepare(run)`: idempotent and never destructive. It is skipped on `--resume`.
- `login(page, as, app, run)`. It either returns a storage-state path or signs in on the page; the harness then saves the state.
- `signedOut(page, as, app)`. True when a signed-in screen landed on the sign-in page (an expired session): the shot is `recipe-failed`, never `ok`. Every run, `--resume` included, signs in fresh, so a resume re-takes it.
- `theme(context, theme, app)`, `readTheme(page)`, `settle(page, screen)` and `chrome` (selectors of app chrome that paints its own background).
- `freezeClock: true` freezes `Date` at run.json's `clock` in every shot (Playwright `page.clock.setFixedTime`, before navigation; timers keep running). The loop's first `run` records that instant and every later run and pass carries it, so the app sees the same "now" in every pass. A run.json without a `clock` (an older vybava) fails the run's setup.

**Deterministic shots.** A screen whose shot is byte-identical to the previous pass's carries that pass's review for free (see Carry-forward); one whose pixels moved by a clock or a counter is reviewed again. `freezeClock` and `volatile` remove those two causes. Data a `prepare` re-dates relative to the real time still moves the pixels.

`states.ts` reaches empty, error and loading without touching seed data:

- `emptyList(url)`, `failingList(url, status?, body?)`, `heldList(url)`, `patchJson(url, fn)`;
- `echoWrites(url)`, so preferences never persist;
- `all(...)`;
- `listStates(base, url)`, which yields `<id>-empty`, `<id>-error` and `<id>-loading`.

`check` validates each screen's area, app, viewports, themes, kind and state against the config, and that its sourceFiles exist.

## A pass

`run` writes `<out>/pass-<n>/run.json`. It holds the resolved apps (an app's `env` var set on the Mac already overrides `baseUrl`), every viewport, the selection, the lint knobs, the workers, the build wait and the `clock` a `freezeClock` project freezes `Date` at (the newest earlier run.json's, else now). Then it runs the command from the repo root:

```sh
UILOOP_ROOT="$PWD" UILOOP_RUN="$PWD/.ui-loop/pass-3/run.json" pnpm exec playwright test -c "$PWD/tests/ui-loop/vendor/playwright.config.ts"
```

The repo needs no Playwright config of its own for the loop.

- **Pass numbers** start at 1. `--pass 0` (or below) is refused with `SELECTION_INVALID` on every verb, never read as "not given". The next pass is the latest plus one. A pass that holds no shots yet (a `--print` whose command never ran) is reused instead. `--resume` defaults to the pass `state` reads (the latest with shots, else the latest; never a newer, pending pass) and retakes everything that is not `ok` or `unreachable`.
- **One capture at a time.** `run` holds the pass's `capture` lease for its whole duration, `--wrap` included, renewing it while the capture runs, and releases it on exit, also on an error. `--print` releases it when it returns, so a printed command runs without it: `state` does not wait for that capture and another `run` is not refused beside it. Capture through `run` or `--wrap` to hold it. While any pass's capture lease holds, another `run` is refused with `CAPTURE_RUNNING`, so the pass it would reuse is one whose capture lease is absent or stale (see Pass leases).
- **Shot layout:** `shots/<id>/<viewport>.<theme>.{png,full.png,json}` in the pass directory. The full companion is only taken when the page scrolls, capped at 6000 CSS px.
- **Other pass files:** `params.json`, `locks/` (the pass leases, see Pass leases), plus `auth.json` and `.auth/` (the run's storage states; the global teardown deletes them, so a session never outlives its run), plus `report.json`, `report.md` and `done.json` (written by the global teardown, `done.json` last: `{ v, pass, run, finishedAt, shots }`, where `run` is the `createdAt` of the run.json it finished). The report sums defects per shot, so one sidebar defect counts once per screen, and beside it counts each rule + element path + detail once across the pass (`lintUnique`, `defectsUnique`), and lists the top repeated offenders (`offenders`: rule, path, detail, screens, shots).
- **Statuses:** a shot is `ok`, `recipe-failed` (still shot), `theme-mismatch`, `build-error` (a red dev-server overlay that never turned green within `--build-wait`), `unreachable` or `error` (a harness failure, which also fails the test).
- **Lint rules** (`LINT_RULES` in `lint.ts`), in two groups:
  - Defects: `h-scroll`, `text-clipped`, `text-spill`, `grid`, `type-ramp` (with `lint.ramp`), `touch-target` (mobile viewports), `safe-area`, `contrast`, `glass-on-content`, `nested-surface` and `glass-blur`.
  - Listed for judgement: `h-scroller`, `truncated` and `repeated-text`.

The shot record (`capture.ts` `ShotRecord`, v1) is the contract that `split`, `publish` and `scoreboard` read (`internal/uiloop/record.go`). Its `lint.distinct` (defect rule → the distinct `{ path, detail }` the shot hit, up to 1000 per rule) feeds the unique counts. It is additive: a record without it (a harness before v0.23) makes the unique columns unknown, never zero.

### On a Devbox

The capture must run next to the app, never on the Mac. The container needs the repo and its node_modules, but not vybava.

1. **Check.** On the Mac, in the repo: `vybava ui-loop check`. Step 2's `run` writes run.json into the synced tree. `vybava ui-loop run --print --json` writes it too and shows the `devbox run -- '<cmd>'` line without running anything.
2. **Run it in the workspace.** Use `vybava ui-loop run --wrap "devbox run --max-wait 45m -- {cmd}"`, which holds the capture lease until the capture ends. A printed line you run yourself runs without it (the lease ended with `--print`). Set the app's `env` var inside the command when the container reaches the app on another address.
3. **Publish beside the capture.** Devbox sync is one-way (Mac → box), and the box has no vitrinka CLI or token, so on the Mac, next to the running capture, run `vybava ui-loop publish --follow`. Every `--interval` (30 s) it rsyncs the pass back from `--from` (default `publish.from` + `/<out>/pass-<n>/`, e.g. `devops:ws/<workspace>/<app-dir>/.ui-loop/pass-3/`, with `<workspace>` from `devbox url --json`). It never fetches `.auth/` (storage states), `playwright/`, `run.json` or `locks/` (the leases are the Mac's; the box holds only the copy Devbox synced there). It then adopts and pushes every shot that became final, one push per area set that gained files. Without `--pass` it follows the pass a running `run`'s capture lease names, else the newest. It stops once the run's `done.json` has arrived and no new shot has for `--until-idle` (10 min). It is idempotent through the ledgers and the index, so a re-run after a stop or a crash picks up where it left off. Without `done.json` (a capture killed before its teardown) it gives up after 3 × `--until-idle` with no new shot (`RUN_UNFINISHED`); finish with `run --resume` and follow again.
4. **Score on the Mac:** `ui-loop scoreboard`. `split` and a plain `publish` still work on a pass fetched by hand (`rsync -a --exclude=/locks/ devops:ws/<workspace>/<app>/<out>/pass-<n>/ <out>/pass-<n>/`; never fetch `locks/`, whose box copy would bring back leases the Mac already released).

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
- **Keys** are `<boardPrefix>-<area>`, capped at 64 characters with a digest. **Titles** are `<boardPrefix> · <area>`.
- **Order:** files are grouped by viewport, then theme, in the order `apps.<app>.viewports` × `themes` lists them. Inside each group they follow screen (manifest) order, with a shot's full capture right after its viewport capture. A plain `publish` adopts in this order, so the set's manifest follows it too. Under `--follow` shots are adopted as they become final, so the manifest is in arrival order, and the sections below carry the layout.
- **Sections:** each set in `plan.json` and each row in `publish/index.json` carries `sections: [{title: "Pass N · <viewport> · <theme>", viewport, theme, labels: [...]}]`, in set order. The review-loop publisher lays out one board section per entry from its labels.
- **Size:** vitrinka 5.13 syncs a screenshot set file by file (the per-file door), so a set has no byte cap and holds up to 20,000 files, its manifest included (`ingest.MaxSetFiles`). The root is shared by every pass, so the files other passes adopted into it (their ledgers, one per path) count too. An area above the cap is not planned: `split` reports `SET_TOO_LARGE`. Split the area in the config.
- **Labels** are `P<n>-<ID>-<VIEWPORT>-<THEME>[-FULL]`, capped at 40 characters.
- **Captures** carry `--device <viewport>`, `--viewport <W>x<H>@2` and `--state <theme>[ · as <user>][ · <app>]`, so every shot names its section on the board.
- **Captions** carry the status and the lint defects, worst first.
- **Determinism:** the plan is identical for an identical pass. It goes to `<pass>/publish/plan.json` (`v: 2`).

`publish` adopts each set under `<out>/sets/<key>`, one root per area shared by every pass (a set key is a board, so each pass adds its shots and its `Pass N · <viewport> · <theme>` sections to the same board), then pushes it:

1. `vitrinka board init --root --key --title --project`.
2. `board capture web --file … --label --title --route --url --note --src --state --device --viewport` adopts each file. The descriptor stays in place, so each capture fires vitrinka's detached per-file push and the shot shows up on the board while the rest adopt. A synced ledger at `publish/adopted/<key>` (one `path<TAB>fingerprint` line per file) makes a re-run adopt only what is missing. The fingerprint includes image SHA256, capture time and metadata: changed pixels at the same path and size are adopted again. Older bare-path and timestamp-only ledger entries refresh once. It lives beside the set roots, never in one: `board push` refuses a root holding anything but images, `.boxes.json` sidecars and `manifest.json`. A full companion passes its own CSS size as `--viewport` (PNG size ÷ 2, `@2`): it is an element screenshot of the scroller, narrower than 2 × the page viewport, which the hi-dpi check would refuse. `--hidpi` stays on. A file `board capture` refuses is recorded under the set's `refused` (`path`, `error`) and reported as `PUBLISH_REFUSED`; the rest of the set is adopted and pushed, and the next publish retries only the refused files. Only a failure to run vitrinka at all aborts the set.
3. `vitrinka board push --root --title --yes --no-input --no-render --json`, reading `data.url`. This commits the set and records its status.
4. A failed push is retried (`--retries`, default 3). A set that still fails is `failed` in the index, and `--sets <key>` retries it.

Each acknowledged set is saved immediately to `<pass>/publish/index.json` through an atomic, synced replacement before another upload starts. Notes and legacy rows remain beside the sets. After a cutoff, rerun `publish`: acknowledged sets with matching content and metadata are skipped; unfinished or refused files retry. `--force` re-pushes an acknowledged set.

Only one publisher may write the shared area roots at a time. `publish` holds the pass's `publish` lease until it exits, renewed while it runs (`--follow` renews it every tick), named by `--owner` (default `ui-loop publish`) and bounded by `--ttl` (1h); a second publisher of the pass is refused with `LEASE_HELD`. The lease is per pass while the roots are shared by every pass, so two passes' publishers are still not excluded. These receipts prevent repeat delivery after a recorded acknowledgement. A cutoff between a capture or upload acknowledgement and its local ledger or receipt write can still repeat the request and append a capture; this is not an exactly-once protocol. Shared roots are never rebuilt on retry, so earlier passes stay present.

**A pass published before one set per area** (chunked by area × viewport × theme into `…-<viewport>-<theme>-<n>` sets) is not re-adopted into those chunks. `publish` plans fresh area sets and adopts every file into them. Index rows whose key is no area's set move to `legacy` (`key`, `title`, `files`, `status`, `url`), and publish reports `LEGACY_SETS`. Their boards are never deleted here: that cleanup is the owner's call, and `legacy` is its list.

`publish.maxFiles` and `publish.maxBytes` are deprecated. An older config that still sets them decodes, and they are ignored with a `CONFIG_DEPRECATED` warning from `check`, `split` and `publish`. Delete them.

## Scoreboard and the review backlog

`scoreboard` folds the pass's records with its review backlog. The backlog comes from `--backlog`, else from `<pass>/review/backlog.json` when present. A backlog whose `pass` is not the scored pass is refused. The output is `scoreboard.json` and `scoreboard.md` in the pass directory. Each area counts:

- screens by their worst open finding: broken, needs-work or polish. A screen with no open finding is `clean` only when it is in the backlog's `reviewed` list. Otherwise it is `unreviewed`: nobody judged it, so it is not clean;
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
  "reviewed": ["tasks-my-new-empty", "tasks-my-new"],
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
- **`carriedFrom`** (optional) is the earlier pass whose review of an unmoved screen the item was copied from by `merge-review`. It must name an earlier pass. The scoreboard leaves a carried item out of the status counts: it is that pass's verdict, not this one's.
- **`reviewed`** lists the screen ids a reviewer actually judged this pass, whether or not they found anything. It is the only thing that makes a screen clean: a screen with no open finding that is missing from `reviewed` is counted `unreviewed` per area and in the totals, and the markdown table shows the column. A backlog without `reviewed` (the format before v0.24.1) still scores every screen without an open finding as clean, with the unreviewed column shown as `—`, and `scoreboard` warns `REVIEWED_MISSING`. An empty list means nothing was judged.

## Doctor: preflight a stage

**`doctor [--for capture|review|fix|verify]`** runs every check a stage depends on and writes nothing:

```json
{ "ok": false, "vybava": "0.32.0", "contract": 2,
  "checks": [{ "id": "apps", "status": "fail", "detail": "portal http://10.8.0.10:21782 ($UI_LOOP_PORTAL_URL): answers 200 with the Vite error overlay", "fix": "fix the build error …" }] }
```

Each check is `ok`, `warn`, `fail` or `skip`, with a `detail` and a `fix`. `ok` is false iff a check fails; the envelope then carries the failing checks' diagnostics as errors and exits 2. Like `check`'s, `next` lists only the fixes of what fails, never a warning's. A check the `--for` stage does not need warns instead of failing. Without `--for`, every stage needs every check.

| Id | What it checks | Fails for |
|---|---|---|
| `check` | `check`'s diagnostics, under their own codes (`SPEC_LINT_DRIFT` included): an error fails the row, a warning warns it. `VENDOR_DRIFT` only warns: the capture stage syncs it first, and `run` still refuses a drifted vendor. | `capture`, `verify`, which run the harness (warns for `review`, `fix`) |
| `contract` | Always `ok`; the detail names `StateContract`, so a workflow can compare. | — |
| `apps` | Each app's base URL, resolved like the capture's (the app's `env` var when it is set here), answers 2xx/3xx within 5 s and is no dev-server error page: the Vite overlay or error page, `Cannot GET`, an Angular CLI/esbuild compile error (`✘ [ERROR]`) or `Failed to compile`. A redirect is an answer and is not followed (`APP_UNREACHABLE`). | `capture`, `verify` (warns for `review`, `fix`) |
| `pass` | The newest pass has shots. A shot-less one warns: the next `run` reuses it, never skips it, and `state` reports it `pending` (or reads it, when no pass has shots). Its fix: `run --resume --pass N` while the source still matches the revision in its `capture.json` (a resume from a moved HEAD is refused), else a plain `run`, which reuses it; or deleting it when it is a stray `--print` and an earlier pass is the one to carry on. No pass at all is `ok` for `capture` and without `--for`, since `run` starts pass-1 (`PASS_MISSING`). The pass leases of every pass warn too: a running capture (`CAPTURE_RUNNING`; wait, `state` routes `wait` until it ends), and a lease its holder never released (`LEASE_STALE`: a process lease after a crash, or a file that names no holder; `rm` it, or leave it to the next taker, which replaces it). A stale owner lease is how a claim or a synth ends, and is not reported. | `review`, `fix`, `verify`, when no pass holds shots |
| `workspace` | Always `skip`: the Devbox workspace and its hold are not in the config yet. | — |
| `signin` | Always `skip`: per-persona sign-in is not probed yet. | — |

## Stage verbs: what the review-loop reads back

A pass is too big to move through an agent's return value. On pwf-ui pass 1 (377 screens, 31 batches, a 340-item backlog of 500 KB) the workflow's agents dropped the screen list, abridged the raw review files and refused the backlog, so a backlog was synthesized from 7 of 31 batches and a fix stage planned 0 lanes. These four verbs own every list instead. An agent runs one and relays its envelope; an agent that needs an item's body reads it from the file by key (`jq '.findings[] | select(.key=="<key>")' <pass>/review/backlog.json`).

**`state [--pass N] [--cap 6] [--primitives dir,dir]`** reads a pass back as counts, never item bodies. It defaults to the latest pass that holds shots (the latest pass when none does), and a repo with no pass answers pass 0 and `capture`. It writes nothing.

```json
{
  "vybava": "0.39.0", "contract": 5,
  "pass": 1, "passDir": ".ui-loop/pass-1",
  "capture": { "running": false, "pass": 0, "since": "", "owner": "" }, "pending": 2,
  "config": { "dir", "out", "spec", "appMap", "areas": [], "apps": [], "project", "boardPrefix", "lint": { "grid": 4, "touchTarget": 44 },
              "source": [":(top)", ":(exclude,literal).ui-loop", "…"], "primitives": ["libs/ui-lib"] },
  "capturedHeadSha": "<sha>", "sourceUnchanged": false,
  "drift": { "app": ["apps/portal/src/app/tasks/tasks.component.ts"], "rig": ["tests/ui-loop/screens/tasks.ts"], "spec": ["PWF-B04"], "appTotal": 1 },
  "shots": 2231, "screens": 377, "areas": [{ "area": "portal-shell", "screens": 80 }],
  "published": true, "unpublished": [], "sets": [{ "area", "key", "status", "url" }],
  "review": { "batchesFile": true, "size": 14, "planned": 32, "done": ["<batch id>"], "left": ["portal-shell-2.2"], "blocked": [{ "batch": "portal-shell-2.1", "reason": "reviewer stalled twice" }],
              "reviewedAreas": [], "stalls": { "portal-shell-2": 2, "portal-shell-2.1": 2 }, "carried": 0, "carriedFrom": null },
  "hasBacklog": true,
  "backlog": { "file", "findings": 340, "open": 340, "byStatus": {}, "bySeverity": {}, "reviewed": 264, "carried": 0 },
  "previous": { "pass", "file", "open" },
  "checkpoints": { "total": 47, "byStatus": { "done": 43, "blocked": 4 } },
  "boards": [{ "area", "url", "slug", "section" }],
  "next": { "stage": "review", "resume": false, "reason": "areas not reviewed: portal-shell", "only": null, "parallel": 3 }
}
```

- `vybava` is this binary's version as `vybava --version` prints it (`dev` for a source build). `contract` is `StateContract` (`stage.go`): it is bumped whenever a field a workflow reads is added or changes format, digests included, and the review-loop refuses a lower one (`UILOOP_STATE_CONTRACT`). A higher one passes that check, so a release that adds a value a workflow routes on needs that workflow's upgrade too: contract 4 adds `next.stage` `wait`, which a review-loop built for contract 2 or 3 does not know, so its Prepare fails while a capture runs until vitrinka is upgraded. Contract 5 adds `review.stalls` and `review.blocked`, and a split batch's parts in `batches`, `planned`, `done` and `left` (see Stalls: split, then block); a review-loop built for contract 4 never splits or blocks, but it does not know `blocked` either: on a pass where a contract-5 run blocked a batch, it counts that batch left and waits on it for good, so every machine that shares a pass upgrades vitrinka with vybava. A `state` without it predates the contract: `brew upgrade --cask vybava`.
- `capture` is the capture a `run` holds a live lease for, the newest pass first, whichever pass it shoots: `{running, pass, since, owner}`, all zero values while none runs. `pending` is a pass newer than `pass` that holds no shots and has no live capture (a stray `--print`, or a run killed before its first shot), else null: the next `run` reuses it, and `state` reads the newest pass with shots meanwhile (the pass a bare `run --resume` resumes). An explicit `--pass` is read as asked, never pending.
- `config.lint` is the lint the pass was captured with (its `run.json`), else the config's before the first pass: `grid` and `touchTarget` with the defaults (4, 44) filled, so a reviewer brief quotes the values the shot records were measured with, even after `vybava.config.ts` changed.
- `published`: every area with shots has its area set in `publish/index.json`, `pushed` (or `skipped`: already pushed with the same files). The index's `legacy` rows never count.
- `review`: batches come from `review/batches.json`, else they are computed with size 14 (`batchesFile: false`). A batch is done when `review/raw/<id>.json` exists and, in a pass with provenance, when its raws complete it (see Durable workflow evidence: v1 by the whole basis, v2 screen by screen). `planned`, `done` and `left` count the batches a reviewer takes: a split batch's parts, never the split batch, which is done exactly when its parts are. A blocked batch is in neither `done` nor `left` but in `blocked` (`[{batch, reason}]`, never null); one its raws complete after all is `done`. An area is reviewed when none of its batches is left, so an area whose every screen was carried is reviewed, and so is one whose last batch was blocked. `stalls` counts each batch's reviewer stalls (`batches --stall`), only the batches that had one, `{}` when none did. `carried` counts the screens `batches.json` carries into the pass (see Carry-forward) and `carriedFrom` is the pass they came from, `null` when none carry. A carried screen retaken since `batches` planned it is not counted, and its area is not reviewed.
- `backlog`: `bySeverity` counts open findings only, and `reviewed` is -1 for a backlog without the list. `carried` counts the items copied from an earlier pass (`carriedFrom`); `byStatus` leaves them out, as the scoreboard does, so `byStatus` plus `carried` sums to `findings`. `previous` is the newest earlier pass that has a backlog. `checkpoints` counts one checkpoint per item (see Checkpoint files below).
- `boards` is `publish/boards.json` of the pass, else of the newest earlier pass that has one.
- `drift` is what changed from `capturedHeadSha` to the working tree (tracked edits and untracked, non-ignored files), by class. `app` is the changed paths inside `config.source`, relative to the config root (`../` for one outside it), sorted and capped at 20 (`appTotal` is the uncapped count). `rig` is the changed paths under `dir`, `vendor/` included, capped at 20. `spec` is the rule ids whose text changed in `spec`, sorted: a rule id is a token like `PWF-D01` (`[A-Z][A-Z0-9]*-[A-Z]*[0-9]+`). Each added or removed line names the ids on it, else the first id of the nearest line above it in its section, else of the nearest enclosing heading that carries one, else its section's heading text (so a spec without ids still reports something), else the spec's path. A heading closes a section, so an earlier section's last rule never claims an edit to the next section's prose. The lists are empty, never null. `drift` is null when it cannot be weighed: for a pass without provenance in a git repo, and for one whose revision this clone lacks (`state` also warns `CAPTURE_REVISION_MISSING`). `next` then treats the pass as drifted (the table below). Outside git no capture has provenance, so there is no drift to weigh and `next` routes as if there were none.
- `sourceUnchanged` is `drift.app` being empty. **Spec and docs commits never stale a pass; only app drift does.** The rig is repaired between review and verify, the basis reads the manifest and the spec at the captured revision, and a spec edit is reported by rule in `drift.spec`, never recaptured.
- `next` is the one router: the review-loop runs it verbatim. Its rules, in order:

  | Stage | When | `only` |
  |---|---|---|
  | `capture` | no shots yet | null |
  | `capture` | the pass is unpublished and `drift` is null | `[]` |
  | `capture` (`resume`) | the pass is unpublished, or it has provenance and no shots; with provenance, only while the tree outside `out` and `.vitrinka` (the rig's included) matches its revision, which is capture's own check for a resume | null |
  | `capture` | the pass has shots and provenance but is unpublished, and that tree changed, so capture would refuse the resume | `[]` |
  | `capture` | the review is incomplete and `drift.app` is not empty (`reason` names up to 3 drifted paths), or `drift` is null | `[]` |
  | `review` | no backlog, or an area has a batch left | null |
  | `fix` | open items lack a finishing checkpoint (`done`, `skipped` or `blocked`); drift is expected while fixing | null |
  | `done` | nothing is open, `drift.app` is empty and `drift` is not null | null |
  | `done` | the pass reached `--cap` | null |
  | `done` | open items remain and the round fixed nothing | null |
  | `verify` | otherwise: the round is checkpointed, or nothing is open but the app drifted (or `drift` is null) | the selection; `[]` when `drift` is null |

  An interrupted writer (`fix/recovery.json`) overrides them all with `fix` (`resume`), and a running capture (`capture.running`) overrides even that with `{stage: "wait", resume: false, reason: "pass N capture running since …", only: null}`: the pass moves under every stage, so a workflow runs no agent and asks again later. On `review`, `next.parallel` is how many identical review runs to launch (absent on every other stage): ⌈left batches no live claim holds / 4⌉ (4 reviewers a run; a batch a live run claimed needs no new run), at most 3, and at least 1, the run that synthesizes once no batch is left (see Pass leases). `only` is the screen ids to reshoot, sorted, on `verify` and on a `capture` that reshoots the pass; `[]` is a full reshoot, and `reason` says why. The verify selection is the screens of the open findings (any status but `met`), the `screens` of the `done` checkpoints, and every screen whose shot records' `sourceFiles` meet the full `drift.app` list (a source file meets a drift path when they are equal or one is a directory prefix of the other). Any `drift.app` path under a `--primitives` prefix (default `uiLoop.primitives`) makes it a full reshoot (`primitive changed → full reshoot: <path>`), and so does a selection that comes out empty. The `checkpoints` verb stays for a workflow that still computes the selection itself.

**`batches [--pass N] [--size 14] [--areas a,b] [--claim N --owner ID [--ttl 3h]]`** gives the reviewer batches: each area's screens sorted by id, in chunks of `--size`, with areas in config order. The ids are `<area>-<n>`. The verb writes every batch to `review/batches.json` (`{v: 2, pass, size, batches, carried}`), the one definition the review stage and `merge-review` share. It returns `{pass, passDir, file, size, screens, batches: [{id, area, screens, digests}], done, left, carried: [{screen, from}]}`, narrowed to `--areas` (the file never is). `digests` maps each batch screen to its evidence digest (see Durable workflow evidence); a reviewer copies the ones of the screens it read into its raw file's `screens`. `carried` (sorted by screen) are the screens no batch holds because they carry the previous pass's review; `screens` counts only the batched ones, so the batches plus `carried` are the pass's screens. Without `--size` the size the file was made with stays. A different `--size` once raw batches exist is refused (`SELECTION_INVALID`), because it would redefine what a finished batch covered. A review that started without v2 batches (a raw file exists and `batches.json` is v1 or missing) keeps v1 batches (`{v: 1}`, no digests, no carry) until its pass is done, because a carry would re-chunk the batches its v1 raws were judged against. In the file, each `carried` entry also holds the screen's `digest` when the carry was planned (see Carry-forward). A batch split after a stall (below) keeps its entry in the file with `parts: ["<id>.1", "<id>.2"]`, and its parts follow it; the verb's `batches` lists the parts in its place, never the split batch, so `screens` still counts each batched screen once. Every re-plan keeps a split and a block: a batch planned again with the screens it had keeps its parts (their screens frozen, their digests the plan's) and its `blocked`. One whose screens moved (a carry gained or lost re-chunks its area) is a new batch, neither split nor blocked, because it holds screens the old one did not: the stalls counted for it and its old parts are cleared, so its stalls count from one, and the raws of its old parts still count screen by screen. A blocked batch carries `blocked: true` and `blockedReason`, is never `left` and never claimed.

With `--claim N --owner <run id>` it also claims up to N of the `left` batches (narrowed to `--areas`) for that run, as `batch-<id>` leases held for `--ttl` (3h), and returns them as `claimed: [batch…]` in batch order; `batches` still lists every batch. The owner's own live claims come first (renewed), then unclaimed or stale-claimed batches; another owner's live claim is skipped. A batch complete by its raws (per-screen digests, v2) is never `left`, so it is never claimed, and neither is a split or blocked batch, even one split or blocked after `left` was judged: the claim reads `batches.json` under the lease mutex. Two claims at once never share a batch (see Pass leases). Without `--claim` nothing is claimed and `claimed` is absent; `--claim` without `--owner` is `SELECTION_INVALID`.

Three flags act on one batch of the persisted `batches.json` instead of planning, one at a time (two at once is `SELECTION_INVALID`, and so is an id the file does not hold). A pass with no `batches.json` yet is `PASS_MISSING`, refused before any lease is touched. They are the review-loop's stall protocol (see Stalls: split, then block):

- **`--stall <id>`** counts one reviewer stall of the batch in `review/attempts/<id>.json` (`{stalls: n, at: [RFC3339…]}`) and returns `{batch, stalls}`, this stall included. `state.review.stalls` shows the count. Only a batch a reviewer takes stalls: a split batch (stall the part) and a blocked one are `SELECTION_INVALID`.
- **`--split <id>`** halves the batch in batch order: the first ⌈n/2⌉ screens are `<id>.1`, the rest `<id>.2`, each part with its own screens' digests. It records `parts` on the batch, releases the batch's `batch-<id>` claim whoever holds it, and returns `{pass, passDir, file, batch, parts: [batch…]}`. A part splits again the same way (`<id>.1.1`). A batch already split returns its parts unchanged and writes nothing. A batch of one screen cannot split, and a blocked one is out of the review: both are `SELECTION_INVALID`. With `--claim N --owner <run id>` it also claims up to N of the split's left batches for that run, exactly as `--claim` claims, and returns them as `claimed`.
- **`--block <id> --reason <text>`** takes the batch out of the review and returns `{batch, reason}`: it leaves `left` and its claim is released, and `merge-review` lists its screens no raw read (or listed unreviewed) as unreviewed, `"<screen> (stalled: <reason>)"`; one without an `ok` shot keeps its plain id, a capture defect rather than a stall. A split batch is blocked through its parts (`SELECTION_INVALID`), and `--block` without `--reason` is refused.

**`merge-review [--pass N] [--owner ID] [--ttl 1h]`** takes the pass's `synth` lease first. With `--owner <run id>` it holds it for that run past the command, through the synthesis that follows, until `--ttl` (1h) runs out or the same owner takes it again; without `--owner` it holds it only while it runs. Held by anyone else, it is refused with `LEASE_HELD`, naming the holder and since when, so of N identical review runs one synthesizes. It folds `review/raw/*.json` (in batch order) and the previous pass's backlog into `review/backlog.draft.json`, a `{v, pass, reviewed, findings}` in the scoreboard contract:

- **Previous items:** each previous open item keeps its key and takes its worst verdict across the batches (`not-met` beats `partly`, which beats `met`). An item nobody judged stands as `not-met` and is listed in `unjudged`.
- **Fresh findings** are keyed by screen + title (slugged, 80 characters). Two with one key fold into one: the worst severity wins, and viewports, themes, shots and files are unioned. A fresh finding whose key is a previous item's is that item's verdict, never a second item. A raw finding's `area` defaults to its batch's.
- **Problems:** a raw finding missing its screen, title, acceptance, files or a valid severity stays out of the draft and is listed in `problems` (`{batch, index, screen, title, missing}`).
- **v2 raws** (with `screens`) speak only for the screens they read at their current digest: a finding or verdict on any other screen (one retaken since, or one the raw did not list) is left out, because that screen is reopened and reviewed again.
- **Carried screens** (`batches.json`'s `carried`, from the previous pass) take that pass's items on them as they stood, each with `carriedFrom: <that pass>`; by the carry rule they are all `met`, and an open one would stay open. They count as reviewed. A carried screen retaken since `batches` planned it (its digest moved) takes nothing and is listed `unreviewed`. The scoreboard never counts a carried item's status as this pass's verdict, nor does `byStatus` here (`carried` counts them).
- **`reviewed`** is every v1 raw batch's screens (from `batches.json`, written now if missing), every screen a v2 raw read at its current digest and every carried screen, minus the screens a reviewer listed as `unreviewed`. A screen of a batch a v2 raw names that no raw read is listed as `unreviewed`, and so is a blocked batch's screen no raw read or listed unreviewed itself, as `"<id> (stalled: <reason>)"` (a reviewer's own entry keeps its plain id, and so does a screen without an `ok` shot). An entry there is read up to its first space or parenthesis, so `"formio-cc-url (unreachable: …)"` skips `formio-cc-url`. Entries naming single shots (`<id>@<viewport>.<theme>`, or `<id>@<viewport>` for every theme) skip nothing unless they name every `ok` shot of the screen; otherwise the screen was judged at its other shots. The same rule decides whether a raw's acceptance verdict on a previous finding counts.

It returns counts plus what needs an agent's judgement: `{pass, passDir, file, previous, raw, left, findings, open, byStatus, bySeverity, carried, reviewed, unreviewed, unjudged, problems, invalid}`. `invalid` is what the draft still breaks of the contract. Planned batches without a raw file are listed in `left` and warned `REVIEW_INCOMPLETE`; like `state`'s, `left` lists a split batch's parts, never the split batch, and never a blocked batch, so a stalled batch never holds the synthesis off. A pass whose every batch is blocked merges without a raw file. The synthesis agent judges only those keys, writes `backlog.json` and validates it with `scoreboard`.

**`lanes [--pass N] [--primitives dir,dir] [--max 4]`** plans the fix round from `review/backlog.json` by **ownership**. It writes `fix/lanes.json` and returns the same:

```json
{ "v": 1, "pass": 1, "passDir", "file",
  "primitives": [{ "lane": "prim-1", "dirs": ["libs/ui-lib/src/lib/components/table"], "keys": ["…"], "bySeverity": {} }],
  "areas": [{ "lane": "area-1", "dirs": [], "keys": [], "bySeverity": {} }],
  "frozen": ["<every primitive dir>"], "foreign": ["<key>"], "i18n": ["<key>"],
  "open": 340, "finished": 47, "primitivePrefixes": [], "max": 4 }
```

1. **Home.** Each open item's home is the directory of its first file that is inside the repo and not an i18n catalog (a `.json` under a directory named `i18n`). An absolute path is made repo-relative. A path outside the repo (another repo, or `../`) never counts. An item without a home is `foreign` (warned `FOREIGN_ITEMS`), or `i18n` when its only in-repo files are catalogs.
2. **Groups.** Items group by home. A group is primitive when its dir is under a `--primitives` prefix (a directory prefix, `libs/ui-lib` matches `libs/ui-lib/…`; without the flag, `uiLoop.primitives`) or when items of two areas share it. `frozen` lists every primitive dir.
3. **Packing.** The groups pack greedily into at most `--max` lanes per phase: the biggest group first, onto the lightest lane, ties by dir. The primitives phase runs first.
4. **Order inside a lane:** worst severity first, then carried items (`not-met`, `partly`) before fresh ones, then key.
5. **A resumed round** classifies every open item, so `frozen` stays stable, but packs only the items without a finishing checkpoint (`finished` counts them).

On pwf-ui pass 1 this planned 4 × ~62 primitive items and 4 × ~22 area items. The union-find over shared files it replaces chained 244 items and 172 files into ONE lane through hub files (a table template named by 46 findings, the i18n catalogs).

**The ownership contract:**

- **Dirs.** A lane owns the files DIRECTLY inside its `dirs`, not their subdirectories: a subdirectory with items of its own is another group, and maybe another lane. A fix that needs a file outside its dirs is checkpointed `blocked`, with the exact change as its note.
- **Frozen.** Area lanes never edit a `frozen` dir.
- **Catalogs.** i18n catalogs are owned by nobody. Lanes return the keys they need (`{key, <locale>: text}`), and the fix stage's settle step applies them, type-checks, and resolves the `i18n` items.
- **Foreign items** get a `blocked` checkpoint that names where the fix lands, so the round can finish.

**Checkpoint files.** A lane writes each item's checkpoint to `fix/<key>.json`, so a key with `/` (`portal-shell-6/topbar-phone-touch-targets`) lands in a subdirectory. One reader maps those files to items, and `state`, `lanes` and `checkpoints` all go through it:

1. It walks `fix/` recursively. `lanes.json`, `recovery.json` and the top-level `fix/r<N>/` directories are left out: a round that rewrites an earlier checkpoint moves the original into `fix/r<N>/`, and an archive never counts.
2. The item is the JSON `key` field, else the path under `fix/` minus `.json`.
3. A file that does not decode, or has neither `key` nor `status`, is skipped with `CHECKPOINT_INVALID`.
4. In a pass with provenance, a stale file (see Durable workflow evidence) is not admitted.
5. Only admitted files compete for a key, so a stale file never hides a current one. The file at `fix/<key>.json` counts first, then the newer one; each other file is warned `CHECKPOINT_INVALID`, and the warning names the file that counts.

A workflow never globs `fix/*.json`: the glob misses a slash key's subdirectory and admits stale files. It reads **`checkpoints [--pass N]`**, which returns `{v, pass, passDir, checkpoints: [{key, status, lane, basis, commit, fileDigests, apiChanges, screens, i18n, note}]}`, one per item, with `i18n` passed through verbatim. For example, the screens a round changed are `vybava ui-loop checkpoints --pass N --json | jq -r '.data.checkpoints[] | select(.status == "done") | .screens[]?'`, and the strings it needs are `… | jq -c '.data.checkpoints[].i18n[]?'`.

## Pass leases: parallel runs on one pass

N identical review-loop runs can share a pass. Before the leases, a second concurrent `run` opened pass N+1, `state` read the newest pass even without shots, and every other run routed to it: three captures landed in one directory. The runs now coordinate through leases, one file per lease at `<passDir>/locks/<name>.json` (`lease.go`):

```json
{ "owner": "run-7f3a", "host": "lukas-mbp", "pid": 0, "startedAt": "2026-10-03T09:00:00Z", "ttl": "3h0m0s", "run": "" }
```

| Name | Taken by | Kind | ttl |
|---|---|---|---|
| `capture` | `run`, for its whole duration (`--wrap` included; `--print` releases it when it returns, so a printed command runs unleased); the lease's `run` field is the `createdAt` of the run.json it writes | process | 6h |
| `batch-<id>` | `batches --claim N --owner <run id>` (or `--split <id> --claim N --owner <run id>`, for its parts); released by a `--split` or `--block` of the batch | owner | `--ttl`, 3h |
| `synth` | `merge-review`: an owner lease with `--owner`, else a process lease | owner or process | `--ttl`, 1h |
| `publish` | `publish`, and `publish --follow` (renewed every tick) | process | `--ttl`, 1h |

- **Exclusive.** A lease is created with O_EXCL semantics, as the hard link of a fully written temp file (the link either lands or finds the name taken, so a reader never decodes half a lease), under the pass's mutex: an flock on `locks/.mutex`, which the kernel drops with its process. One taker wins; a lost race is a normal held result.
- **Stale.** A lease no longer holds when its `pid` is dead and its `host` is this one (a pid on another host cannot be judged), when `startedAt` + `ttl` is past, or when the pass's `done.json` names its `run`. The next taker replaces a stale lease (remove, then the exclusive create, under the mutex).
- **Process leases** (`pid` is the vybava process) are released when the verb exits, an error included; only a crash leaves one behind, and its dead pid stales it. A capture and a publish renew theirs every quarter of its ttl while they run, so the ttl bounds only a holder that stopped (a crash on another host, or a pid reused after one), never a long verb. **Owner leases** (`pid` 0) are held for a workflow run, named by `--owner` (its run id), past the verb that took it until the ttl runs out; the same owner taking one again renews it (`startedAt` kept, the ttl counted from now), from any host: an owner lease matches on its owner alone, since macOS renames the host per network. A release removes a lease only while it is still the releaser's, never one a new holder took over after it went stale.
- **The leases are the Mac's.** Devbox syncs `locks/` to the box like the rest of the pass, but nothing reads it there, and `publish --follow` never fetches it back.

How a parallel review uses them:

1. **Wait for a capture.** `run` refuses with `CAPTURE_RUNNING` (error, next `vybava ui-loop state --json`) while any pass's capture lease holds. While one does, `state` reports `capture.running` and routes `wait`, and the workflow returns without agents.
2. **Launch `next.parallel` runs.** On `review`, `state.next.parallel` names how many identical review runs keep the unclaimed left batches busy at 4 reviewers each (at most 3, at least 1).
3. **Claim, then review.** Each run's Prepare claims its batches with `batches --claim <reviewers> --owner <run id>`, and its reviewers take only `claimed`. A batch is claimable while it is left (not complete by its raws' per-screen digests) and unclaimed, or its claim is stale.
4. **One synthesizer.** The run whose `merge-review --owner <run id>` takes `synth` synthesizes, scores and posts. Every other run's merge-review is refused with `LEASE_HELD` (error, naming the holder and since when), and that run stops.

A run that died mid-review frees its claims when their ttl runs out, and its synth after 1h; remove `locks/synth.json` to hand the synthesis over sooner. `doctor`'s `pass` row warns a running capture and a process lease left by a crash (`LEASE_STALE`).

## Stalls: split, then block

A reviewer that stalls on a batch (the review-loop's watchdog gives up on it) used to leave the batch left, so every review run claimed it again and stalled again: the loop never reached the synthesis. Now a stall shrinks the batch, and a batch that cannot be reviewed leaves the review:

1. **Count.** On a reviewer's stall of batch X the review-loop runs `batches --stall X`.
2. **Split.** At 2 stalls it runs `batches --split X` (with `--claim 2 --owner <run id>`, which also claims the parts for that run): X's halves `X.1` and `X.2` replace it in `batches` and `left`, its claim is released, and the same run reviews the parts from its spare reviewers. X is done when both parts are.
3. **Block.** A part that reaches 2 stalls is `batches --block <part> --reason "reviewer stalled twice"`, never retried; only a re-plan that moves its screens draws a new batch over them, whose stalls count from one (see `batches`). Its screens no raw read go to `merge-review`'s `unreviewed` as `"<screen> (stalled: reviewer stalled twice)"` (a screen without an `ok` shot as its plain id, a capture defect), so the scoreboard counts them unreviewed (never clean), and the review goes on to synthesize.

The thresholds are the review-loop's (vitrinka `workflows-src/lib/uiloop-review.js`); this CLI counts, splits and blocks only when told to. Carried screens are in no batch, so a split never touches them.

**There is no re-batching by image weight, on purpose.** `ComputeBatches` chunks each area's screens by count (`--size`), and that stays: the weight of a batch's images does not predict a stall. On pwf-ui pass 4, `portal-overview-3` (154 PNGs, 1,245 MP) was reviewed while `portal-shell-2` (77 PNGs, 297 MP) stalled. A weight cap would have split the batch that passed and left the one that stalled whole. Splitting the batch that actually stalled needs no model of why it did.

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

Edit `internal/uiloop/harness/`, never a vendored copy. A sync that writes nothing keeps `STAMP.json`, so a clean vendor's stamp can name an older release than the binary; `check` reports `vendor.syncedBy` (the stamp's release) only when the vendor drifts, where it says which release synced it.

These tables are mirrored in Go, and tests keep them equal:

- `BUILTIN_VIEWPORTS` ↔ `viewports.go`;
- `LINT_RULES` ↔ `LintRules` and `LintInfoRules`;
- the run.json and record shapes ↔ `run.go` and `record.go`, and `done.json` (`teardown.ts` `DoneFile`) ↔ `follow.go` `DoneFile`. Bump `RUN_VERSION`/`RECORD_VERSION` on a breaking change.

The harness must type-check under TS 5.3 strict, with `noUncheckedIndexedAccess`, `exactOptionalPropertyTypes` and `noPropertyAccessFromIndexSignature`, in both CJS and ESM packages. It must stay Node 20 compatible: no bun-only APIs, no `import.meta`, no `__dirname`.

### Durable workflow evidence

The stage reader also reports `headSha`, `capturedHeadSha`, `drift`, `sourceUnchanged`,
`reviewBasis`, `scoreboardBasis`, `scoreboardCurrent` and `checkpointApiNotes`.
`run` writes `capture.json` atomically before starting the interruptible runner;
resume retains that revision and refuses any drift outside `out` and `.vitrinka`,
the rig's included, anywhere in the repo (a capture refuses an uncommitted change
the same way). `state` routes a legacy pass without provenance to a full reshoot.
Captures outside git have no verified revision.

`reviewBasis` is the SHA256 of the sorted `[path, SHA256(bytes)]` pairs over the
pass's shots (`*.png`, `*.json`), the manifest (`*.ts` under `dir`, `vendor/`
skipped) and the `spec`. **A pass's evidence is immutable**: with provenance, the
manifest and the spec are read from git at `capturedHeadSha`, never from the
working tree, so a recipe repair, a `knownIssues` correction or a spec edit
committed after capture leaves the reviews, backlog and checkpoints of that pass
current. The verify pass is the one that captures with the repaired rig; a spec
edit shows up as its rule ids in `state`'s `drift.spec`. Only the shots still move
the basis. A pass without provenance reads the manifest and the spec from the
working tree as before. So does a pass whose revision this clone lacks (gc'd after
its branch went, or copied from another clone), and `state` warns
`CAPTURE_REVISION_MISSING` for it: fetch the revision or capture a new pass.
Vybava 0.33 and earlier read the spec from the working tree, so a pass whose spec
changed between capture and review, reviewed under those, holds evidence stamped
with another basis: after the upgrade its reviews, backlog and checkpoints read as
stale once, and the pass is reviewed again.

For passes with provenance, a raw review is judged by its shape. A **v1 raw**
(`basis`, `screensRead`, written for v1 batches) completes its batch when it carries
the current `basis` and its own batch id, and its `screensRead` stays inside the batch
and names every batch screen with an `ok` shot. A **v2 raw** (written for v2
batches) carries `screens: {<id>: <digest>}`, its batch's `digests` copied verbatim
for the screens it actually read, and is judged screen by screen: a screen counts as
read when its digest equals the screen's current one. A `--resume` retake that
changes a PNG moves only its screen's digest, so that screen alone reopens and the
rest of the raw still counts. A batch is complete when every screen in it with an
`ok` shot was read by some v2 raw, whatever the raws are named: the split parts of a
batch, or a hand-merged raw, complete it without a hand-stamped basis. A batch with
no `ok` shot needs a v2 raw that names it in `batch`. A batch split after a stall
(`batches --split`) is complete exactly when its parts are, and a part is complete
when its batch is by its own raws. A screen's digest is the SHA256
of the compact JSON `{shots, screen, spec}`: the sorted `[file, SHA256]` pairs of the
PNGs of its `ok` shots (file relative to the pass directory), its manifest entry as
its shot records recorded it at capture (`id`, `app`, `area`, `kind`, `state`,
`title`, `route`, `parentId`, `variantOf`, `as`, `sourceFiles`, `knownIssues`,
`destructive`) and the spec's SHA256 as the basis reads it (at the captured
revision). A rig or spec edit after capture moves no digest. A screen the pass could
not shoot need not be read. `unreviewed` entries never hold a batch open: they are capture or
recipe defects the reviewer could not judge, and `merge-review` still keeps their
screen out of `reviewed` and lists it as `unreviewed` (a screen-level entry, or
shot entries naming every `ok` shot of the screen). Backlogs use `review/basis.json` as a sidecar.
Fix checkpoints need `basis`, an ancestor `commit`, `fileDigests` (source path to
SHA256 of its current bytes) and optional `apiChanges`; stale or reverted fixes
are reevaluated. Skips/blocks are reusable only with unchanged application source,
where the rig under `dir` does not count (only capture and its resume count it).
Manifest repairs may land between review and verify. A spec edit no longer stales
the pass, but it is still source to a skip or block (the rule above), so it
re-opens those items. API notes live beside source
and in ignored checkpoints. The state reader keeps item bodies on disk.

After scoreboard callouts are confirmed by board readback, the workflow writes
`review/scoreboard-receipt.json` atomically as `{basis: scoreboardBasis, posted:
[{area, url}]}`. The basis covers review inputs, backlog, publish index, board list
and scoreboard files. `scoreboardCurrent` requires a matching receipt, every area
board acknowledged and both scoreboard files present. Interrupted publication
must retry before a workflow can claim completion. Existing CLI-only passes
retain their legacy stage semantics; verified workflows require these receipts.

State computes capture hashes once per request and shares that snapshot with
publication and review/checkpoint checks; no digest cache survives a request.
Valid partial raw reviews still merge their findings, while unread screens and
incomplete batches remain explicit. Scoreboard validates the same evidence for
both the default backlog and an explicitly supplied backlog (with its adjacent
`basis.json`). Writers persist `fix/recovery.json` before editing. State relays
that bounded lane identity and schedules its cleanup before completed checkpoints
can hide a dirty interrupted writer, even at the pass cap.

### Carry-forward

`batches` carries a screen into pass N instead of batching it when its pixels did
not move since pass N-1 was reviewed, so a reshoot reviews only what changed.
Nothing carries unless pass N-1 has a backlog (then the one `merge-review` folds),
that backlog is current (its `review/basis.json` matches) and lists `reviewed`, and
every batch of pass N-1 is complete or blocked (a blocked batch's screens are
unreviewed there, so they never carry). A pass captured after one that was never
reviewed carries nothing, even from an older reviewed pass. A screen then carries
when:

- that backlog lists it in `reviewed` and names it in no open item (any status but `met`);
- it has an `ok` shot in pass N;
- its manifest entry as its shot records recorded it (the fields of its digest) and
  the spec's SHA256 are the same in both passes: a verdict judged against other
  known issues or other rules is not evidence for this pass;
- its `ok` shots in both passes are the same viewport × theme set (one missing on
  either side is a move), and every PNG of them (the viewport capture and the full
  companion) is byte-equal, or else the same size with at most
  `uiLoop.review.carryTolerance` (default 0.001) of its pixels differing, RGBA
  compared exactly. A size change, or a file that holds no decodable PNG, is a move.

A carried screen leaves the batches and is listed in `batches`' `carried` and in
`batches.json`; `state.review` counts the screens (`carried`) and names the pass
(`carriedFrom`); `merge-review` copies that pass's items on it with `carriedFrom` and
counts it reviewed, so it stays clean on the scoreboard and can carry again into the
next pass. A pass whose every screen carried has no batch, so `merge-review` drafts
its backlog from the carried review without a raw file. `batches.json` records each
carried screen's `digest` as planned, so a carried screen retaken since (a
`--resume`) reopens at once: `state` no longer counts it and its area is not
reviewed, and `merge-review` copies nothing onto the new pixels and lists it
unreviewed. The review stage runs `batches` every time, and that run batches it
(or carries it again, if the retake still matches pass N-1). A review that started
without v2 batches never carries.

Each carry copies `carriedFrom: N-1`, so a screen carried twice names the pass it
was copied from, not the pass that judged it; that pass's backlog names the one
before.

Pixels are compared only for an eligible shot whose bytes changed, and a screen
stops at its first moved PNG. On pwf-ui passes 3 → 4 (a fix round apart: 121
eligible screens, about 990 PNGs, a median 13% of pixels changed) the plan took
37 s at 220 MB and carried nothing. Byte-identical shots make carrying cheap and
likely; see Deterministic shots (`freezeClock`, `volatile`).
