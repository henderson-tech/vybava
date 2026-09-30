# polish-kit - the polish skill's deterministic layer

`/polish` is a two-axis verb: target `app | ui | api` x intensity
`quick | default | full`. The skill is thin (judgement, the adverse-condition
rows, the fix loop); `polish-kit` owns the mechanical parts: target
inference from the diff, device lanes, the pass ledger, native screenshot
capture, contact sheets and the report. It never touches the machine's
network (a browser or server lane is one GET of its configured URL) and never
modifies app code. Install: `vybava install polish-kit` (multicall link) or
`vybava polish-kit …`. Skill: `skills/polish`.

## Verbs

```text
polish-kit plan     [--base <ref>] [--target app,ui] [--intensity quick|default|full] [--findings <board|pr-url|file>]
polish-kit lanes    [--target app] [--lane <id>]
polish-kit lanes set <lane> [--theme light|dark] [--nav gesture|3button] [--text <size>] [--reset]
polish-kit run init [--pass N] [--target app] [--lanes a,b] [--screens x,y] [--intensity …] [--base <ref>] [--force]
polish-kit run add-cell --kind matrix --lane <id> --flow "<title>" --tier "<tier row title>" [--pass N]
polish-kit cell <id> pass|fail|skip [--shot <path>] [--note <text>] [--finding <annotation id or text>] [--pass N]
polish-kit status   [--pass N]
polish-kit shoot <lane> [--pass N] [--screens x,y] [--themes light,dark] [--nav gesture,3button] [--text default,large]
polish-kit sheet    [--pass N] [--screen <id>] [--lanes a,b]
polish-kit report   [--pass N] [--previous N | --no-delta]
```

Every verb takes `--json`. `--pass 0` or omitted means the latest pass
(`run init`: the next one).

- **plan** infers targets from `git diff --name-only <base>...HEAD` mapped
  through the `targets` globs (weight = changed files per target, ordered by
  weight, ties in app, ui, api order), widened by the cwd (inside a target's
  glob root puts that target first, reason `cwd`) and by `--target` (reason
  `flag`, first). The base is the configured one when it resolves, else the
  remote's default branch (`origin/HEAD`), else `main`/`master`. Data:
  `{targets: [{id, files, reason}], intensity, base, lanes, screensTouched,
  findings, changed}`. `screensTouched`: per target, the screens whose
  `area` or the last segment of `url` names a component of a changed path;
  when none derives, every screen of the target. `--findings` is recorded
  in the plan and copied into run.json; the applet never reads it.
- **lanes** resolves every declared lane to a live device NOW. Data per
  lane: `{id, kind, target, ready, booted, udid|serial|name, runtime, nav,
  theme, fix, boot, url, status}`; a lane with a problem carries its
  diagnostic (exit 2 when any lane has one). `next` is the boot command of
  the first ready-but-not-booted lane.
  - `ios-sim`: one `xcrun simctl list -j` (simctl takes at most one type
    filter, so devices, devicetypes and runtimes come from the single call).
    The newest available runtime whose version starts with `runtime` (`26`
    matches 26.0 and 26.5, `18.6` only 18.6) holding a device whose
    `deviceTypeIdentifier` is `deviceType`'s (resolved through the
    devicetypes list: `iPhone SE (3rd generation)` →
    `com.apple.CoreSimulator.SimDeviceType.iPhone-SE-3rd-generation`); sims
    are named per persona, so the display name is never required to match.
    Among the type's devices: the pinned `device` (name or udid) wins, then a
    name equal to or ending in the type name, then any shutdown sim; a booted
    sim the lane did not pin comes last (it is usually another session's).
    `theme` is read from `simctl ui <udid> appearance` when booted.
  - `ios-device`: `xcrun devicectl list devices --json-output <tmp>`; the
    device by name, udid or identifier (or the only one); ready when paired
    and tunnel-connected.
  - `android-device`: `adb devices -l`; the serial (or the only non-emulator
    device); `nav` from `settings get secure navigation_mode` (0/1 =
    3button, 2 = gesture), `theme` from `cmd uimode night`.
  - `android-emulator`: `emulator -list-avds` holds `deviceType`; running
    when an `emulator-*` serial answers `adb emu avd name` with it.
  - `browser` / `server`: GET of `url` with a 3 s timeout; any answer
    below 500 is ready. With `urlCommand` the base URL is resolved at run
    time: the command runs through `sh -c` from the repo root, its output is
    trimmed and the first line is the base (nothing appended); `url` may
    then hold a path starting with `/` that is appended to it (an absolute
    `url` is used as is and the command is not run). A command that exits
    non-zero, prints nothing or prints something that is not an http(s) URL
    answers `device-unavailable` with the command, its exit code and the
    last stderr line in `detail`, fix = "start the workspace app the
    command names, then: polish-kit lanes --lane <id> --json". This is how
    a Devbox-leased app is reached (`devbox url <ws> api` moves per run; a
    parked workspace has no address).
- **lanes set** applies device state: iOS sim `simctl ui <udid> appearance`
  / `content_size`; Android `cmd uimode night yes|no`, `cmd overlay
  enable-exclusive --category com.android.internal.systemui.navbar.gestural`
  (`…navbar.threebutton`), `settings put system font_scale <n>`. `--reset`
  restores light, gesture and the default size (`medium` / `1.0`). An
  `ios-device` lane answers `lane-unsupported` with the Settings path.
- **run init** writes `<out>/pass-<n>/run.json`: the plan snapshot, the
  lanes (the targets' lanes, or `--lanes`), the screens (the plan's touched
  screens, or `--screens`) and the chrome cell table. Idempotent: an
  existing pass is returned as it is (`existing: true`); `--force` rewrites
  the ledger and keeps the shots on disk. `<out>/.gitignore` (`*`) is
  written on the first init so the run root never needs a repo edit.
- **run add-cell** appends a matrix cell `<lane>--matrix--<flow>--<tier>`
  (slugged; idempotent by id). The tier rows come from the skill's
  reference; the applet does not know them.
- **cell** records a verdict; `fail` without a shot (given now or already on
  the cell) is `shot-required`. `--shot` must exist; it is stored
  pass-dir-relative when it lives under the pass, else absolute.
- **status** counts cells per lane, kind and verdict and lists the pending
  ones; `next` holds one `cell <id> …` command per pending cell.
- **shoot** captures every chrome cell of a ready, booted `ios-sim`,
  `android-device` or `android-emulator` lane: per device state (theme, nav,
  text size) it sets the device, then per screen opens the deep link
  (`simctl openurl` / `am start -a android.intent.action.VIEW -d`), waits
  `settleMs` and screenshots (`simctl io screenshot` / `exec-out screencap
  -p`) into `<pass>/shots/<lane>/<screen>--<theme>[--<nav>][--<text>].png`,
  marking the cell's `shot` and saving run.json after every shot (a cut-short
  run resumes by re-running). Verdicts stay pending: judgement is the
  agent's. Device state is reset at the end. An `ios-device` lane answers
  `lane-unsupported` naming the hand shot / Appium alternative and the
  directory to drop files in; a `browser` lane points at `ui-loop run`; a
  `server` lane at `run add-cell`. The lane is read from the pass's
  snapshot (`run.lanes`), like the screens: a lane edited or removed in the
  config after `run init` never captures another device under the old
  identity (`unknown-lane` names the pass's lanes and the `--force` init
  that would re-snapshot). The device is restored on EVERY return path,
  including a failed open, screenshot, state change or ledger write, with a
  bounded context of its own; a reset failure is reported together with the
  capture error, never instead of it.
- **sheet** renders, per screen with shots, `<pass>/sheets/<screen>.png`
  (columns = lanes, rows = device states, every shot scaled to 640 px high
  under a label strip; a missing file is a red cell), `<screen>--edges.png`
  (per shot: the four 120 px corners zoomed 3x with the device edge marked,
  then the top and bottom 120 px bands fitted to the sheet width, up to 3x;
  clip and rim defects hide at edges) and `<screen>.json`, the legend with
  every cell's rectangle. Pure Go (`image`, `image/draw`, `image/png`, a
  built-in 5x7 bitmap font).
- **report** writes `<pass>/report.md` and returns it: a table per lane
  (screen x state, glyphs ✓ ✗ - ·), the matrix cells, the failing cells with
  shots and findings, and the delta versus `--previous` (default: the
  immediately preceding pass number; its ledger must load, so an
  incompatible one answers `run-version` rather than being skipped for an
  older pass that happens to decode; `--no-delta` skips it): `fixed` (fail
  → pass), `regressed` (pass → fail), `new` (a failing cell the previous
  pass did not have). Pending cells make it a warning, not a refusal. In text mode the Markdown is the output; under
  `--json` it is `data.markdown`.

## Config: `polish` in vybava.config.ts

Typed by `PolishConfig` in `.vybava/config.ts` (mirrored by
`internal/polishkit/config.go`; change both together). Unknown keys are
rejected and every problem is reported at once.

```ts
polish: {
  targets: {
    app: ['apps/client/**'],
    ui: ['apps/web/**', 'apps/admin-web/**'],
    api: ['apps/api/**', 'packages/shared/**'],
  },
  base: 'origin/main',            // default; falls back to the repo default branch
  out: '.polish',                 // default; gitignored run root
  lanes: [
    { id: 'ios26', target: 'app', kind: 'ios-sim', runtime: '26', deviceType: 'iPhone 17 Pro', textSizes: ['accessibility-medium'] },
    { id: 'ios18', target: 'app', kind: 'ios-sim', runtime: '18.6', deviceType: 'iPhone 16' },
    { id: 'android', target: 'app', kind: 'android-device', nav: ['gesture', '3button'], textSizes: ['1.3'] },
    { id: 'phone', target: 'app', kind: 'ios-device', device: 'Lukas iPhone' },
    { id: 'web', target: 'ui', kind: 'browser', url: 'http://10.8.0.10:3111' },
    // A Devbox-leased app: the command's first output line is the base, url the path appended to it.
    { id: 'api', target: 'api', kind: 'server', urlCommand: 'devbox url fixit-polish api', url: '/health' },
  ],
  screens: [
    { id: 'home', title: 'Home', target: 'app', url: 'fixit://home', area: 'home' },
    { id: 'inquiry-new', title: 'New inquiry', target: 'app', url: 'fixit://inquiries/new', settleMs: 2500, area: 'inquiries' },
  ],
}
```

`themes` defaults to both; `nav` (Android only) to `['gesture']`; iOS
`textSizes` are `simctl ui content_size` names, Android ones `font_scale`
numbers as strings.

## The pass directory and run.json

```text
<out>/pass-<n>/run.json          the ledger (RUN_VERSION 1)
<out>/pass-<n>/shots/<lane>/<screen>--<theme>[--<nav>][--<text>].png
<out>/pass-<n>/sheets/<screen>.png, <screen>--edges.png, <screen>.json
<out>/pass-<n>/report.md
```

`run.json` is `{v, pass, passDir, createdAt, vybava, plan, lanes, screens,
cells}`. A cell is `{id, kind: 'chrome'|'matrix', lane, screen|flow, tier,
theme, nav, textSize, verdict: 'pending'|'pass'|'fail'|'skip', shot, note,
finding}`. Chrome cell ids are `<lane>--<screen>--<theme>[--<nav>][--<text>]`
and the table is lanes x screens of the lane's target x themes x nav
(Android) x text sizes (default first), in config order. Matrix cell ids
are `<lane>--matrix--<flow-slug>--<tier-slug>`.

**The RUN_VERSION rule.** `RunVersion` (`internal/polishkit/run.go`) is
bumped on any breaking change of `RunFile` or `Cell`. A pass written by
another version answers `run-version` on every verb that reads it and is
recreated with `run init --pass <n> --force`; it is never silently re-read.
Additive fields (a new optional key) do not bump it. Every writer (`run
init`, `run add-cell`, `cell`, `shoot` after each shot) is one
load-modify-save transaction under the pass's interprocess lock
(`<pass>/run.lock`, `flock`), so a verdict recorded from another terminal
while a shoot runs survives; the file is written to a uniquely named temp
file (pid + random suffix) and renamed into place, so a crash mid-shoot
leaves the last saved shot recorded and never a half-written ledger.

## Envelope, diagnostics and exit codes

Every verb emits one `{v, ok, verb, data, diagnostics, next}` (cli-craft,
`internal/runx`), JSON under `--json`; in text mode a short aligned table
(or the Markdown, for `report`), then diagnostics as `CODE: detail - fix`
and the `next` commands. Exit 0 ok, 1 infra (a command that could not run;
its stderr's last line is the detail), 2 when an error diagnostic is
present. `next` names the exact follow-up on success and on failure: `plan`
→ `lanes` and `run init`; `lanes` → the boot command, `run init`; `run init`
→ `shoot <lane>` per shootable lane, `run add-cell`, `status`; `shoot` →
`sheet`, the first pending `cell`; `cell` → the next pending `cell`,
`status`, or `report` when nothing is pending; `sheet` → the first pending
`cell`, `report`; `report` → `status` while cells are pending, `run init
--pass <n+1>` when cells fail.

The closed code enum (`internal/polishkit/diag.go`):

| code | fires when | fix |
|---|---|---|
| `no-config-section` | no vybava.config.ts, no `polish` section, or one that does not decode or validate | the exact `polish:` block to add |
| `no-changes` | the diff is empty and neither the cwd nor `--target` names a target | `plan --target app` |
| `unknown-target` | `--target` is not app, ui or api | the corrected invocation |
| `unknown-lane` | a lane id the config does not declare | `lanes` (lists them) |
| `unknown-screen` | a screen id the config or the pass does not have | `status` / `run init` |
| `lane-missing` | the runtime exists but no simulator / AVD of `deviceType` does | the exact `xcrun simctl create "<name>" "<deviceTypeIdentifier>" "<runtimeIdentifier>"` / `avdmanager create avd …` |
| `runtime-missing` | no installed iOS runtime matches `runtime` | Xcode > Settings > Components, naming the version |
| `device-unavailable` | a phone unpaired or disconnected, an adb device offline/unauthorized, several devices with none named, an emulator or simulator not booted when a verb needs it, a URL that does not answer, a `urlCommand` that exits non-zero or prints no URL (detail: command, exit code, last stderr line) | reconnect / boot / start command; "start the workspace app the command names, then: polish-kit lanes --lane <id> --json" |
| `lane-unsupported` | `set` on ios-device or a URL lane; `shoot` on ios-device, browser or server | the alternative (hand shot + drop directory, `ui-loop run`, `run add-cell`) |
| `tool-missing` | xcrun, adb or emulator is not on PATH | the install |
| `shot-required` | `cell … fail` without a shot, `--shot` names a missing file, `sheet` on a pass without shots (warning on `report` while cells are pending) | `cell <id> fail --shot <path>` / `shoot` |
| `pass-missing` | no pass under `<out>`, or not the one asked for | `run init --pass <n>` |
| `cell-unknown` | the cell id is not in the pass | `status` |
| `run-version` | run.json is another `RunVersion` or does not parse | `run init --pass <n> --force` |
| `usage` | a flag, argument or verb the applet does not accept | the corrected invocation |

## Testing

`internal/polishkit` tests drive every verb through a fake `Exec` with
simctl / devicectl / adb fixtures and a temp repo: plan inference, the cell
table, the run.json round trip and version gate, the report and its delta,
the sheet layout math, the parsers. Nothing in the tests reaches a device
or the network.
