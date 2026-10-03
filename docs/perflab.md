# perflab

Physical-device performance testing for Expo / React Native apps. `perflab`
is the engine (this applet); a project plugs in through the `perflab`
section of its `vybava.config.ts` (the adapter); the `expo-device-perf`
skill (`skills/expo-device-perf/`) is the judgment on top: what to measure,
how to read it, which render-cost rule fixes it.

```sh
vybava install perflab expo-device-perf
perflab adapter check --json
```

Near neighbours: `framestats` reads Android frame dumps and Perfetto traces
(perflab uses it as a library), `perfrig` drills an API under load (server,
not device frames).

## Ownership

| Layer | Owns |
|---|---|
| `internal/devlab` | The machine-global ledger of phones, leases, device locks, discovery (devicectl, xctrace, adb), state probes, the token-checked passthroughs. |
| `internal/perflab/buildindex` | The portable native key, the build index, bundle export, pack, install and the install fence. |
| `internal/perflab/doctor`, `netfwd`, `wda`, `hostexec` | Preflights, the Android API forward, the prebuilt WebDriverAgent, the one process seam (process groups, timeouts, stall watchdog, progress lines). |
| `internal/xctrace`, `internal/framestats`, `internal/perflab/analysis` | Every number: iOS hitch tables, Android display-rate bins, poll sidecars, Perfetto present gaps and RenderThread cost, compare and report. |
| `internal/perflab` | The adapter section, token resolution, and the verbs that compose the above: run, probe, crashes, app, hazards, adapter check. |
| The project | The measured interaction (its Appium spec, its window bracket, its scenario table and budgets), its build profile env, its sign-in and world-reset hooks. |

The engine knows Expo, React Native, Hermes, xctrace, devicectl, adb,
Perfetto, codesign, zipalign and apksigner. It knows nothing about a project:
no env var names, specs, seed worlds or API paths. Analysis is a pure
function of stored evidence: `perflab analyze --reread <dir>` recomputes
every number.

## CLI contract

- Every verb emits one envelope on stdout under `--json`, success and
  failure: `{v: 3, ok, verb, data, diagnostics, next}` (`internal/runx`).
  Without `--json` the same diagnostics print as `CODE: detail - fix`, then
  `next:`.
- Exit codes: `0` ok (warnings allowed), `1` `INFRA_ERROR` (unstructured
  I/O or transport), `2` diagnostics present. A wrapped command's exit code
  is never passed through: the runner's goes to `data.runner.exit` plus
  `RUNNER_FAILED`; a passthrough's to `data.exit` plus
  `DEVICE_COMMAND_FAILED`.
- `next` is the protocol: exact commands with the device id and lease
  token filled in, on success and failure.
- Misuse is `USAGE` with the corrected invocation in `fix`, never a usage
  dump.
- Global flags: `--json`, `--project <dir>` (the adapter root; default the
  git toplevel of the working directory, a worktree resolves to itself),
  `--timeout <dur>` (a hard limit for the whole verb), `--log <file>` (tees
  the progress lines). `PERFLAB_STATE_DIR` and `PERFLAB_CACHE_DIR` move the
  state and the cache (tests, sandboxes); `PERFLAB_TUNNEL_REGISTRY` the iOS
  tunnel registry URL.
- Progress for Monitor: long verbs print `perflab[<verb> <device>] phase=<p>
  +42s` on stderr per phase change and `still phase=<p>` every 30 s. The
  grep anchor is `^perflab\[`. stdout carries only the envelope.
- Idempotent re-run is the recovery path: `build native` and `bundle
  export` return the index hit, `pack` is content-addressed, `run --resume
  <runDir>` skips finished blocks.
- Retries: one automatic infra retry, recorded in the verb's data, only for
  `XCTRACE_ATTACH_FAILED`, `WDA_STALLED`, `ADB_DISCONNECTED` and
  `INSTALL_TRANSPORT`.
- Sanitising: no credential URLs, tokens, keystore passwords or env values
  in an envelope. Env is reported as names plus `publicEnvHash`; the
  sign-in link of `app link` never prints (only its host). The lease token
  is printed once, by `lease acquire`.
- Children run in their own process group under the invoking perflab
  process, never daemonised or nohup'd. perflab stops only the groups it
  started and recorded, never by pattern.

## Verbs

| Verb | What |
|---|---|
| `adapter check` | Validate the section (unknown keys and `{tokens}` refused), show every templated command for this worktree, decode the scenario rows. |
| `device scan [--platform]` | Live phones (devicectl, xctrace Devices vs Devices Offline, adb) with suggested ids and `device add` lines. |
| `device add <id> [--udid --core-device-id --serial --label --expect-hz --protect-package… --personal --notes]` | Register a phone; re-running updates the row. |
| `device list [--no-live]` / `show <d>` / `remove <d> --yes` | The ledger merged with live state and the lease holder; remove is refused while leased. |
| `device probe <d> [--lease]` | State: online, lock, Developer Mode, tunnel, refresh, thermal, battery, memory, focus, plus `HUMAN_CHECK` rows. |
| `device shell <d> --lease <t> -- <args>` | adb (Android) or devicectl (iOS) arguments against the leased device. adb's arguments: a device command runs under `shell` (`-- shell input tap 540 210`). Every device subcommand takes `<d>` or `--device <d>`. |
| `device screencap <d> --lease <t> [--out f]` / `device pull <d> --lease <t> <remote> <local>` | A PNG; one file off the device. |
| `lease acquire <d> [--for 2h] [--purpose] [--wait]` | Mint the token (printed once). |
| `lease status [<d>]` / `renew <d> --lease <t>` / `release <d> --lease <t>` / `reap [--dry-run]` / `break <d> --reason` | Lease upkeep; no force-steal exists. |
| `doctor [--device --lease] [--platform] [--for build\|run\|probe\|all] [--wake]` | Every preflight; `next` lists the fixes in dependency order. |
| `fingerprint --platform [--profile] [--kind]` | The portable native key. |
| `build find\|native\|import <artifact>\|list\|gc` | The native build index; `--kind` defaults to `shell` on iOS and `bundled` (the only kind) on Android. Import refuses another app id or app version. |
| `wda find\|build [--wait]\|import <dir>\|list` | The prebuilt WebDriverAgent. |
| `bundle export --platform [--profile] [--ref] [--label]` / `bundle list` | JS bundles, content-addressed. |
| `pack --native <key> --bundle <sha>` | A variant: the bundle inside a copy of the native build. |
| `install <variant\|key> --device --lease` | Install, verify, record the fence. |
| `net forward\|status\|stop --device --lease [--device-port]` | Android: `adb reverse` plus an in-process forwarder to the API. `forward` streams until SIGINT; run it as a background task. |
| `app launch\|link\|reset --device --lease [--account --route --world]` | Start the app; deliver the adapter's sign-in link; run the adapter's world reset. |
| `run <scenario>... --device --lease --variant [label=]<id>... [--alternate] [--repeat N] [--resume <runDir>] [--no-analyze] [--max 90m]` | The measured runner (below). |
| `probe rest\|drag\|fling\|custom --device --lease [--package] [--seconds 20] [--label] [--tap x,y\|none] [--gesture-file]` | Quick device-only measurement (below). |
| `analyze <path>... [--marks] [--wdio-log --step-cycle] [--tap-lag] [--window a-b --classify\|--stacks] [--sql <preset>] [--reread]` | Every number from evidence. |
| `compare <runDir> [<runDir>] [--threshold 0.15] [--min-runs 2] [--allow-confound <field>]` | B against A under the noise rule; a side may be several run dirs joined by commas (a probe A/B: `a1,a2 b1,b2`). |
| `report [<runDir>...] [--gate] [--md f]` | The newest result per scenario x device against the budgets. |
| `hazards [<dir>] [--gate --baseline f] [--write-baseline f]` | Static render-cost sweep (no device). |
| `crashes --device --lease [--since <RFC3339\|30m>]` | Crash reports and error-boundary lines since a time. |

Default limits: doctor 2 min, device probe 60 s, lease flock wait 30 s,
fingerprint 3 min, build native 60 min with a 10 min stall, wda build 20
min, bundle export 10 min, pack 5 min, install 10 min, probe `seconds + 90
s`, run `--max 90m` with windows + 20 min per scenario per block, analyze 15
min per xctrace export.

`capture start|stop` is deferred: the project's window bracket keeps
capturing and perflab analyzes the evidence it leaves.

## Device ledger and leases

| What | Where |
|---|---|
| Ledger | `~/.local/state/perflab/devices.json` (`schemaVersion: 1`) |
| Leases | `~/.local/state/perflab/leases/<deviceId>.json`, one per device; the file persists after a release (token cleared, `released` recorded) so the generation counter and `lastInstalled` carry over |
| Locks | `~/.local/state/perflab/locks/`: `ledger.lock`, `lease-<id>.lock`, `device-<id>.lock` (kernel flocks naming their holder) |
| Run index | `~/.local/state/perflab/runs.jsonl`, one line per run or probe |
| Builds, bundles, variants, WDA | `~/Library/Caches/perflab/` (`PERFLAB_CACHE_DIR`); never in a repo, `~/Exports` or git |

- A device resolves by its ledger id or any alias (hardware UDID, CoreDevice
  id, adb serial). Marketing names never resolve: two pairs of iPhones
  share a name on one Mac.
- The lease token is the capability. In-process subagents share
  `CLAUDE_PID` and often `CLAUDE_CODE_SESSION_ID`, and a resumed copy runs
  beside the original, so neither id tells two copies apart; a token minted
  after the fork does. Only its sha256 is stored.
- `lease acquire` on a live lease is `DEVICE_LEASED` naming the holder. A
  lease is reclaimed (`LEASE_STALE_RECLAIMED`) only when it expired AND its
  holder process is dead (pid gone, or its start time changed). A dead
  holder within TTL is not stale: a session can restart and resume with its
  token. `lease break` works only on a dead holder or an expired lease.
  TTL default 2 h, max 8 h. A running `run` heartbeats every 60 s and
  keeps its lease at least 5 min ahead while it works, so a run started
  near the end of the TTL is never cut mid-block; a holder that stopped is
  bounded by the TTL.
- Every device-touching verb takes the device lock for its duration
  (`DEVICE_BUSY` names the verb, pid and elapsed time), so even two copies
  holding one token never interleave adb or devicectl calls. `run` installs
  inside the same lock it measures under.
- Fencing: `install` records `lastInstalled` (Android base APK sha256, iOS
  `CFBundleVersion` stamp) under the lease generation; `run` and `probe`
  verify the device still carries it (`DEVICE_STATE_CHANGED` otherwise).
  `probe` of an app perflab never installed there measures it as found,
  with a warning.
- Probe semantics: without `--lease`, `device probe` runs in-device
  readings only when nobody leases the device; a device leased by someone
  else gets host-side checks only (an info `DEVICE_LEASED` row), so a probe
  never loads a phone another holder is measuring. On iOS, xctrace listing
  the phone under Devices Offline is `DEVICE_OFFLINE` ("Instruments cannot
  attach"), separate from a screen-off warning; it was seen on an unlocked
  iPhone 11 with the screen on, and `doctor --wake` (launch the app, poll
  xctrace up to 120 s) brings it back.
- Passthroughs (`device shell|screencap|pull`) check the token and the
  device lock; they refuse another device, adb global options, host-wide
  adb commands (`kill-server`) and any uninstall or data clear of a
  protected package (`PACKAGE_PROTECTED`).
- iOS screencap goes through go-ios over the RemoteXPC registry tunnel
  (`TUNNEL_MISSING` without one). Never set `ENABLE_GO_IOS_AGENT`: it
  spawns a detached userspace tunnel daemon; perflab strips it from every
  child's environment.

### claude-guards

- `machine:device-leased` (PreToolUse Bash, Claude and Codex) refuses any
  raw adb, devicectl, xctrace, go-ios, `idevice*` or Appium command naming
  a leased phone's UDID, CoreDevice id or serial (as an argument, a
  `--flag=value`, or a `NAME=value` assignment, also after `env`, such as `ANDROID_SERIAL` or
  an Appium UDID variable), the holder's own included; a bare `adb` device
  command (no `-s`, `-e` or `ANDROID_SERIAL`) or `adb kill-server` while an
  Android phone is leased. perflab itself is exempt (it checks the token).
  The holder uses `perflab device shell <id> --lease <t> -- <args>`. There
  is no escape hatch: the passthrough costs what the raw call costs. It is
  enforced from the first release: the passthroughs made the warn-first
  rollout unnecessary.
- SessionEnd `claude-guards device-lease-release` releases the ending
  session's leases and stops the process groups they recorded (forwarders,
  runners, perfetto, xctrace); SessionStart `weather --reap` sweeps stale
  leases.

## Native build index, bundles and variants

- The portable key `pf1-<20 hex>` hashes every file `@expo/fingerprint
  --debug` lists with the `../` prefixes stripped (bun's isolated links are
  content-addressed already; main at depth 6 and a worktree at depth 8 get
  the same key), autolinking contents, the adapter's `extraNativeInputs`
  as JSON-pointer hashes, platform, profile, kind, signing (iOS team and
  identity SHA-1, Android keystore cert SHA-256) and the toolchain (Xcode
  build, JDK major). A second key, `pf1s-…`, covers only the native sources,
  platform and profile; `pack` compares that one (`FINGERPRINT_MISMATCH`).
  A native input the Expo fingerprint misses (Reanimated `staticFeatureFlags`
  in the app's `package.json`) belongs in the app's `fingerprint.config.js`
  `extraSources`; packages with a null dir hash are covered by path only.
  A `--ref` tree (`bundle export --ref`, `build native --isolated --ref`) is
  fingerprinted under the PROJECT's rule files (`fingerprint.config.js`,
  `.fingerprintignore`; the tree is restored after): its native inputs judged
  by today's rules, so a "before" ref older than the config still packs.
- Layout: `builds/<platform>/<profile>/<kind>/<key>/`, `bundles/<sha16>/`,
  `variants/<key12>-<sha12>/`. Writes are staged and renamed in; an entry is
  never overwritten; a deleted artifact is `BUILD_ARTIFACT_MISSING`. `build
  gc` keeps the newest per group and never deletes a lease's last install.
- Serialisation: a per-key lock, a Mac-wide host build lock, and a shared
  measure lock that `run` and `probe` hold: a build never starts while a
  run measures and a run never starts during a build (`HOST_BUSY_BUILDING`
  names the holder).
  Runs from ONE project checkout also take turns (one runner lock per
  checkout): the adapter's runner owns per-checkout resources such as its
  Appium port, so a second phone's `run` waits at `phase=runner-wait`
  within its `--max` (then `RUN_TIMEOUT`); runs from two checkouts overlap.
- Recipes: iOS `shell` = `expo prebuild` + `xcodebuild … SKIP_BUNDLING=1`;
  iOS `bundled` = the adapter's `build.iosBundled` writing one `.app` into
  `{outDir}` (a `*build-metadata.json` beside it is read as provenance);
  Android = the engine Gradle recipe (assembleRelease, arm64, template debug
  key) or the adapter's `build.android`. Every build runs under a stall
  watchdog and a timeout; a failed one keeps its log under `cache/logs`.
- Bundle export runs `expo export:embed --minify false --bytecode false`
  and then the project's own `hermesc` (`-emit-binary -O`) on a fixed
  `index.js`. Expo's own `--bytecode` path compiles from a random temp
  directory that hermesc embeds, so two exports of one tree never shared a
  sha; this one does, and differs from Expo's output only in that filename.
  The Hermes magic is checked (`BUNDLE_NOT_HBC`).
- iOS pack: copy the app, keep its entitlements, put the bundle and assets
  in the app root, `EXUpdatesEnabled=NO`, a numeric `CFBundleVersion` stamp
  for fencing, re-sign with the original identity, verify.
- Android pack (verdict GO on the S20): swap the bundle keeping its stored
  layout, drop the v1 signature files, flip expo-updates `ENABLED` to false
  in the binary manifest, zipalign, apksigner with the template debug key;
  resource names and densities are checked against `aapt2 dump resources`
  (`BUNDLE_ASSETS_CHANGED`). A marker-only bundle ran: the marker logged
  under `ReactNativeJS`, the app reached MainActivity, the crash buffer
  stayed empty. Turning expo-updates off is required: with network an
  over-the-air update could otherwise replace the swapped bundle under the
  APK-sha fence. A packed variant is `productionEquivalent: false`; confirm
  a fix once on `build native --kind bundled`.
- Install: iOS `devicectl device install app` + launch
  `--terminate-existing` + bundle version check; Android `install -r` (a
  downgrade or key change uninstalls first only for an unprotected package),
  `cmd package compile -m speed -f`, base APK sha256 check. An install
  under a protected app id replaces the owner's app in place (the iOS perf
  build signs the store bundle id) and warns `PACKAGE_PROTECTED`.

## Adapter

The `perflab` section of `vybava.config.ts` (TypeScript twin
`PerflabConfig` in `.vybava/config.ts`; refresh it with `vybava config init
--force`). Commands run with `sh -c` from the repo root; `{token}` values
are shell-quoted words there, raw text in `runner.env` values. FixIt's
(every command a verb of its `scripts/perf/perflab-adapter.ts`; the team is
a placeholder here):

```ts
perflab: {
  app: {
    root: 'apps/client',
    entry: 'index.js',
    ios: { scheme: 'FixIt', bundleId: 'app.fixit.client', team: 'XXXXXXXXXX' },
    android: { package: 'app.fixit.client.dev', activity: '.MainActivity' },
  },
  profiles: { perf: { env: 'bun scripts/perf/perflab-adapter.ts env --platform {platform} --api {deviceApiOrigin}' } },
  build: {
    iosBundled: 'FIXIT_PERF_DEVICE_KIND=physical FIXIT_PERF_API_URL={deviceApiOrigin} bun run e2e:build:perf -- --no-device-install --team {team} --out {outDir}',
  },
  scenarios: 'bun scripts/perf/perflab-adapter.ts scenarios',
  runner: {
    cmd: 'bun scripts/perf/perflab-adapter.ts run {platform} {scenarios}',
    env: {
      FIXIT_APPIUM_PLATFORM: '{platform}', FIXIT_APPIUM_MODE: 'release', FIXIT_APPIUM_PREINSTALLED: 'true',
      FIXIT_APPIUM_IOS_UDID: '{udid}', FIXIT_APPIUM_ANDROID_UDID: '{serial}',
      FIXIT_APPIUM_IOS_BUNDLE_ID: '{bundleId}', FIXIT_APPIUM_ANDROID_PACKAGE: '{package}',
      FIXIT_APPIUM_WDA_DERIVED_DATA: '{wdaDerivedData}', FIXIT_APPIUM_UPDATED_WDA_BUNDLE_ID: '{wdaBundleId}',
      FIXIT_APPIUM_API_URL: '{apiOrigin}', FIXIT_APPIUM_DEVICE_API_URL: '{deviceApiOrigin}',
      FIXIT_APPIUM_API_TIMEOUT_MS: '90000', FIXIT_APPIUM_MOCHA_TIMEOUT_MS: '{timeoutMs}', FIXIT_PERF_BUILD: '{variant}',
      FIXIT_PERF_OUT_DIR: '{runDir}', FIXIT_PERF_MARKS_FILE: '{runDir}/marks.jsonl',
    },
    unset: ['FIXIT_APPIUM_XCODE_ORG_ID', 'FIXIT_APPIUM_XCODE_SIGNING_ID'],
    appiumHome: 'appium/.appium-home',
    appiumServerLog: 'appium/reports/appium-server.log',
  },
  api: {
    origin: 'bun scripts/perf/perflab-adapter.ts api origin',
    hold: 'bun scripts/perf/perflab-adapter.ts api hold',
    health: '/api/v1/health/ready',
    device: {
      android: { strategy: 'reverse', devicePort: 23936 },
      ios: { strategy: 'bake', origin: 'bun scripts/perf/perflab-adapter.ts api origin --device' },
    },
  },
  hooks: {
    signInLink: 'bun scripts/perf/perflab-adapter.ts sign-in-link --account {account} --route {route} --platform {platform} --api {deviceApiOrigin}',
    resetWorld: 'bun scripts/perf/perflab-adapter.ts reset-world --world {world} --api {apiOrigin}',
  },
  out: '~/Exports/FixIt/perf/{date}-{topic}',
  hazards: { ambientGates: ['useAmbientMotion'], visibilityHint: ['shown', 'visible', 'isFocused'] },
},
```

Tokens (closed; `adapter check` lists the subset each field allows):
`{account} {apiOrigin} {appPath} {appRoot} {branch} {branchSlug}
{bundleId} {bundleSha} {coreDeviceId} {date} {deviceApiOrigin}
{devicePort} {nativeKey} {outDir} {package} {platform} {profile} {route}
{runDir} {scenario} {scenarios} {serial} {team} {timeoutMs} {topic} {udid}
{variant} {wdaBundleId} {wdaDerivedData} {world} {ws}`. `{scenarios}` is
one shell word per name in a command, the names joined by commas in an env
value. `{deviceApiOrigin}` is `http://localhost:<devicePort>` on Android
(the reverse strategy) and the `api.device.ios.origin` URL on iOS (bake).

Scenario rows (the `scenarios` command's stdout, a JSON array; unknown keys
are `CONFIG_INVALID`):

```json
[{"name": "calendar-view-switch-smooth", "windowMs": 105000, "platforms": ["ios", "android"],
  "budget": {"iosHitchRatioMsPerSMax": 5, "androidAnimatingFpsP10MinShare": 0.75},
  "stepCycle": ["menu", "multi", "menu", "team"], "stepGroups": {"modes": ["multi", "team"]}}]
```

The runner contract:

- perflab installs and verifies the variant first; the runner attaches to
  the installed app (no reinstall, no data reset on a personal phone).
- `{runDir}` is the block's case dir; the runner writes its evidence there,
  each scenario's named `<scenario>-<platform>-<stamp>`: an iOS `.trace`
  bundle, an Android poll sidecar under `frames/` (`.json` or `.json.gz`)
  or a `.pftrace`. A per-scenario result JSON beside them may name the
  evidence (`scenario`, `platform`, `recordedAt`, `tracePath`, `framesPath`,
  `pftracePath`); perflab reads only those fields, never its numbers.
- Step marks: `{runDir}/marks.jsonl` (or `marks.json`), JSONL or a JSON
  array of `{label, atMs}` (epoch ms) or `{label, from, to}` (trace
  seconds). Without marks, the runner log's `performActions` taps plus the
  row's `stepCycle` label the steps: the cycle starts at the first tap
  inside the captured window (setup taps before it, such as navigating to
  the screen, are not steps; the lab120 log has 93 taps, 36 = 3 cycles of
  12 in the window).
- xctrace's record output saved beside a trace as `<trace>.record.log` is
  read for `TRACE_RUN_ERRORS`.
- stdout and stderr tee to `<case dir>/run.log`, which ends with
  `EXIT=<code>`.

## Run

`perflab run <scenario>... --device <d> --lease <t> --variant before=<id>
--variant after=<id> --alternate --repeat 2`:

1. Doctor's run preflight (with `--wake`), then the device lock and the
   measure lock for the whole verb; heartbeat every 60 s.
2. iOS: the prebuilt WDA must exist (`WDA_MISSING`); its tokens fill
   `{wdaDerivedData}` and `{wdaBundleId}`. Android: `svc power stayon usb`,
   `am kill-all`, and the API forward for the run (reverse strategy).
3. Blocks: one runner invocation per variant and repeat, `a,b,a,b` with
   `--alternate` (the noise rule). Each block installs its variant inside
   the lock when the fence differs, verifies it, runs the runner with
   `runner.env` and without `runner.unset`, under a limit of the windows
   plus 20 min per scenario. One `runner.env` serves both platforms: an
   entry whose tokens resolve to nothing on this platform (`{wdaBundleId}`
   on Android, `{serial}` on iOS) is unset, and an Android runner gets
   `ANDROID_SERIAL` of the leased phone unless `runner.env` sets it. A
   watchdog kill or `--max` takes the runner's whole process tree (WDA's
   xcodebuild sits in a group of its own). The plan (`--alternate`,
   `--repeat`) is not stored: a `--resume` repeats it (the printed resume
   line does). Watchdogs: `WDA_REBUILDING` (a
   `build-for-testing` under the runner), `WDA_STALLED` (no WDA start line
   in the Appium server log 180 s after its xcodebuild), `RUN_TIMEOUT`;
   failures are classified from the log and the device
   (`XCTRACE_ATTACH_FAILED`, `ADB_DISCONNECTED`, else `RUNNER_FAILED`).
4. After each block: crashes since its start (`APP_CRASHED`), thermal and
   swap, the evidence per scenario (`EVIDENCE_MISSING`), and
   `perflab.run.json` rewritten (the `--resume` checkpoint).
5. `runs.jsonl` gets one line; the run is analyzed unless `--no-analyze`.

Run dir: `--out`, else `<adapter out>/run-<stamp>/`, with a `.gitignore`
for traces, pftraces, exports and crash reports: the numbers beside the
evidence are committable, the evidence never reaches git.

## Probe

Android (Perfetto, 128 MB + 8 MB rings; sched, cpu/gpu frequency, idle,
atrace gfx/view/input/hal/sched/freq/dalvik for the app, process stats,
FrameTimeline): the input focus must be the app (`INPUT_FOCUS_WRONG`), the
period comes from `dumpsys SurfaceFlinger --latency`, gestures scale from
`wm size` (recipes in S20 1080x2400 coordinates):

| Kind | Before the trace | Inside it (after a 2 s lead-in) |
|---|---|---|
| `rest` | tap 540 210 (`--tap x,y` an inert spot when a control sits there, `none`), 1 s | nothing for `--seconds` |
| `drag` | to top x2, mid | 6 x (1700 -> 1100 and back, 1200 ms, 0.3 s apart) |
| `fling` | to top x2, mid | 6 x (1900 -> 500 and 600 -> 2000, 90 ms, 1.3 s coast) |
| `custom` | none | `--gesture-file`: `[{swipe:[x1,y1,x2,y2,ms]} \| {tap:[x,y]} \| {sleepMs:n}]` |

A drag or fling whose gestures presented no frame warns `NO_APP_FRAMES`
(nothing scrolled: judge that screen by `rest`). The exact adb commands are
in `data.gestures`; the input source is `adb`
(pessimistic: it skips Samsung's touch boost). iOS probes `rest` only: an
xctrace window (Hitches + Time Profiler) attached to the launched app.
Each probe writes a run dir and is analyzed like a run; `data.verdict` uses
the default budgets below. Metric definitions: `restFrames` counts frames
after the first gap over 60 ms (every frame when the trace never rests);
`restRunMs` is the longest run of presents with no gap over 60 ms;
`rtDrawMs` is RenderThread `Drawing` per frame;
`presentGaps.twoVsync` counts two-period gaps; drops are gaps over 1.4
periods, blamed on main or RenderThread when either exceeded one period on
that frame or the one before; `display` is the framestats display-rate
reading (500 ms bins, animating fps p10 and median, janky %) of the same
presents, first to last. Every present reading but `rtDrawMs` needs
the app's own FrameTimeline surface frames (`present.frameTimeline`);
without them (no FrameTimeline, or only another process's) they are unread,
so the rest and fling budgets get no check and compare gets no sample. A
screen at rest is the exception the trace proves: FrameTimeline recorded
(display frames), the app's main thread traced its `doFrame`s and nothing in the
app drew (no RenderThread `Drawing`, no main-thread GL swap; a `DrawFrames`
that only synced queues no buffer), so zero presents read as zero frames.

## Analyze, compare, report

- iOS: the `hitches` table on iOS 26 devices, `hitches-summary` on iOS 18
  devices; the stage tables are never summed (`HITCH_TABLE_MISSING` when
  neither exists). The ratio divides by the TOC duration, never the time
  limit. Taps land on the trace clock at wall ms minus the TOC start plus
  0.25 s.
- Android: display-rate bins from the measured period (500 ms bins, an
  interval over 60 ms is rest, an animating bin has 6+ intervals, janky =
  over 1.5 periods); the Android 13 FrameInterval/FrameStartTime swap is
  detected (`COLUMN_ORDER_SWAPPED`); frames whose DisplayPresentTime lies
  before their own vsync use their completion time. That last fix moved
  published S20 numbers: lab120-before fpsP10 99.6 -> 100, janky 26.9 ->
  26.6, modes 91/77 -> 92/77; lab120-layer zooms 108/94 -> 106/94.
- Budget keys (closed): `iosHitchRatioMsPerSMax`, `iosHitchRatio120MsPerSMax`,
  `iosWorstHitchMsMax`, `androidAnimatingFpsP10MinShare`,
  `androidJankyPctMax`, `androidRestFramesMax`, `androidRestRunMsMax`,
  `androidDragRtDrawMsMax`, `androidFlingTwoVsyncGapsMax`, `slopeMax`,
  `androidFpsP10Min` (60-capped: reported, never gates a 120 Hz phone).
  Probe defaults (`analysis.ProbeBudget`, applied by `probe` and by `report`
  to a `probe-<kind>-<label>` row no adapter row names): rest 0 frames /
  6000 ms, drag 4 ms and animating p10 0.75 x refresh, fling animating p10
  0.75 x refresh, iOS 5 ms/s.
- Board rows (`data.boardRows`, 4740's table): one per screen x device. A
  probed screen merges its probes, each column from its own kind (rest
  frames from `rest`, draw ms and fps p10 from `drag`, two-vsync gaps and
  fps p10 from `fling`); another scenario fills the columns its budget names.
  The verdict is the worst of the screen's rows.
- Compare: minimum samples 2, relative MAD <= 0.25, p90/p50 <= 1.5, half
  drift <= 0.25, threshold 0.15; verdicts `improved | regressed |
  within-noise | noisy | too-few-runs`. A different native key, public env
  hash, device, input source or production equivalence is `CONFOUNDED`
  unless `--allow-confound <field>`. One run dir with two variants compares
  them in run order; two dirs (or variant labels from `runs.jsonl`; a side
  of comma-joined dirs pools them, as a probe A/B needs: one record per dir)
  compare dir against dir.

## Hazards

A regex sweep of `*.ts`/`*.tsx` (tests, `node_modules`, `ios/`, `android/`
and `hazards.exclude` left out; a comment is never a site or a gate).
Rows `{file, line, rule, gated, hints}`:
`infinite-repeat` (`withRepeat(…, -1`), `frame-callback`, `path-value`,
`clock` (gated when the file names an `ambientGates` hook; `hints` lists
the `visibilityHint` words found, for a human to check real visibility),
`hw-texture-collapsable` (`renderToHardwareTextureAndroid` on an element
without `collapsable`), `remove-clipped-subviews` (culling on;
`={false}` is no site), `scrollview-route` (a route under `app/` scrolling
without a list), `queries-without-combine`,
`intl-in-render`. `--write-baseline f` records the per-file counts; `--gate
--baseline f` fails on a site beyond them (`HAZARD_NEW`).

## Diagnostics

The closed enum is `perflab.Codes` (`internal/perflab/diag.go`); each code
is documented where it fires. Severities are the usual ones; `fix` is the
exact command where one exists.

| Code | When | Fix |
|---|---|---|
| `USAGE` | a missing or malformed argument or flag | the corrected invocation |
| `INFRA_ERROR` | an unstructured failure (exit 1) | the sanitized error |
| `CONFIG_MISSING` | no vybava.config.ts or no perflab section | add the section, `perflab adapter check --json` |
| `CONFIG_INVALID` | unknown key or token, missing field, bad scenario row | the field path, `perflab adapter check --json` |
| `ADAPTER_COMMAND_FAILED` | an adapter command exited non-zero or printed nothing usable | the resolved command to run by hand |
| `DEVICE_UNKNOWN` / `DEVICE_AMBIGUOUS` / `DEVICE_EXISTS` | handle not in the ledger / two live matches / an add rebinding another row's id or alias | `perflab device scan --json` / pass the UDID / `device show` |
| `DEVICE_LEASED` | another live lease holds the device (info in a probe of someone else's phone) | `perflab lease status <id> --json`; never a steal |
| `LEASE_REQUIRED` / `LEASE_INVALID` | no `--lease` / wrong, expired or released token | `perflab lease acquire <id> --json` / `lease renew` |
| `LEASE_STALE_RECLAIMED` | acquire took over an expired lease of a dead holder (info) | none |
| `LEDGER_LOCKED` | a ledger or lease-file lock stayed held 30 s | names the lock and its holder |
| `DEVICE_BUSY` | another verb holds the device lock | names verb, pid, elapsed |
| `DEVICE_STATE_CHANGED` | the installed artifact differs from the fence | `perflab install <variant> --device … --lease …` |
| `PACKAGE_PROTECTED` | an uninstall or clear would hit a protected package; as a warning, an install replaced a protected app in place (an iOS perf build under the store bundle id) | use the dev package; after the lab, reinstall the store app |
| `DEVICE_COMMAND_FAILED` | a wrapped device command exited non-zero (its code in `data.exit`) | the doctor line |
| `DEVICE_OFFLINE` / `DEVICE_UNPAIRED` / `DEVICE_LOCKED` | not reachable or Instruments cannot attach / not trusted / screen locked | `perflab doctor … --wake` / trust this Mac / unlock |
| `DEVELOPER_MODE_OFF` | iOS Developer Mode is off | Settings > Privacy & Security > Developer Mode (human) |
| `TUNNEL_REGISTRY_DOWN` / `TUNNEL_MISSING` | the RemoteXPC registry is down / has no tunnel for the UDID (warning on wired iOS 18) | the sudo `tunnel-creation` line (human) |
| `UI_AUTOMATION_OFF` | the WDA log says automation mode timed out at low load | Settings > Developer > Enable UI Automation (human) |
| `REFRESH_RATE_MISMATCH` | measured vsync differs from `expectHz` | Motion smoothness High; Low Power Mode and Limit Frame Rate off |
| `THERMAL_HOT` / `LOW_BATTERY` / `MEMORY_PRESSURE` / `AUTO_LOCK_ON` | device state, or a run recorded hot or swapping (compare warning) | cool down / charge / `am kill-all` / stay on |
| `DEVICE_TOOLING_UNSUPPORTED` | iOS < 17 for a devicectl-only step | the fallback invocation |
| `HUMAN_CHECK` | a setting perflab cannot read (info) | the checklist line |
| `WRONG_BINARY` / `NOT_PROFILEABLE` / `DEBUGGABLE_BUILD` | a dev client, or a release not profileable / debuggable | `perflab build find …` / rebuild with the perf profile |
| `INPUT_FOCUS_WRONG` | the foreground window is not the app | `perflab app launch --device … --lease …` |
| `APP_NOT_INSTALLED` | launch says the app is not installed | `perflab install …` |
| `PIPE_CAPACITY_LOW` | a fresh pipe buffers under 16 KiB (the xcodebuild hang) | close idle sessions, re-run doctor |
| `HOST_LOADED` / `HOST_BUSY_BUILDING` / `DISK_LOW` | load per core high / the host build or measure lock held / under 20 GB free | wait / the holder / `perflab build gc` |
| `TOOL_MISSING` / `JDK_WRONG` / `APPIUM_DRIVER_MISSING` | a missing tool, iOS platform or trace processor; JDK 21 not first; driver missing | the install line (`xcodebuild -downloadPlatform iOS` for the platform) |
| `WDA_MISSING` / `WDA_REBUILDING` / `WDA_STALLED` | no prebuilt WDA / the runner rebuilt it / no start in 180 s | `perflab wda build --json` / check `runner.unset` / doctor |
| `API_UNREACHABLE` / `FORWARD_DOWN` / `DEVBOX_PARKED` | health fails host- or forward-side / the workspace is parked | the adapter's `api.hold` / `perflab net forward …` (a background task) |
| `FINGERPRINT_FAILED` | the fingerprint command failed or printed nothing parseable | install deps, re-run |
| `BUILD_NOT_FOUND` / `BUILD_ARTIFACT_MISSING` | index miss / artifact deleted | `perflab build native …` |
| `BUILD_IN_PROGRESS` | another process builds this key | `--wait` |
| `BUILD_FAILED` / `BUILD_STALLED` | non-zero / no output for `--stall` | the build log path, the pipe probe |
| `FINGERPRINT_MISMATCH` | a bundle's source key differs from the native build's | `perflab build native …` for the bundle's key |
| `BUNDLE_NOT_HBC` / `BUNDLE_ASSETS_CHANGED` | not Hermes bytecode / Android asset set changed (info for compiled assets) | re-export / build the variant natively |
| `SIGNING_IDENTITY_MISSING` / `RESIGN_FAILED` / `VERIFY_FAILED` | no identity for the team / codesign or apksigner failed | `security find-identity -v -p codesigning` |
| `INSTALL_FAILED` / `INSTALL_TRANSPORT` | install rejected / USB dropped (retried once) | reconnect, re-run install |
| `NOT_PRODUCTION_EQUIVALENT` | a packed variant, not the as-shipped binary (info) | `perflab build native --kind bundled …` |
| `SCENARIO_UNKNOWN` | a name the adapter rows lack, or not for this platform | the names |
| `RUNNER_FAILED` / `RUN_TIMEOUT` | runner non-zero / hard limit passed | `run.log`, `perflab run … --resume <runDir>` |
| `EVIDENCE_MISSING` | a scenario left no trace, sidecar or pftrace in its case dir | `run.log`, `--resume` |
| `XCTRACE_ATTACH_FAILED` / `XCTRACE_NOT_READY` | attach exit 21 after 4 tries / no ready line | retried once, then `doctor --wake` |
| `APP_CRASHED` | a crash report or ErrorBoundary line in a measured window | `perflab crashes --device … --since …` |
| `ADB_DISCONNECTED` | the phone left adb mid-run (retried once) | reconnect, `--resume` |
| `TRACE_UNREADABLE` / `TRACE_RUN_ERRORS` / `HITCH_TABLE_MISSING` / `TRACE_EMPTY` | export failed / `[Error]` lines in the record output / no hitch table / zero length | re-record (Hitches + Time Profiler, physical device) |
| `PROFILE_MISSING` | `--classify`/`--stacks` on a trace without Time Profiler (warning) | re-record with Time Profiler |
| `STEPS_UNMAPPED` | marks or taps outside the recording (warning) | check the marks file or log belongs to the window |
| `FILE_UNREADABLE` | an input file cannot be read | the path |
| `NOT_FRAMESTATS` / `MISSING_COLUMNS` / `MALFORMED_ROWS` / `NO_FRAMES` | a gfxinfo dump that is not one / lacks columns / has bad rows / holds none | re-dump with `dumpsys gfxinfo <pkg> framestats` |
| `NOT_A_PERFETTO_TRACE` / `TRACE_INCOMPLETE` / `PACKAGE_NOT_IN_TRACE` / `NO_APP_FRAMES` / `NO_RENDER_THREAD` / `NO_VSYNC_IDS` / `NO_FRAME_TIMELINE` | the Perfetto trace cannot answer (as in `docs/framestats.md`) | re-record with the probe config |
| `COLUMN_ORDER_SWAPPED` / `CAPPED_FPS_SOURCE` | Android 13 rows (info) / a 60-capped FPS source on a faster display (warning) | none / use the display metrics |
| `PRESET_UNKNOWN` / `PRESET_FAILED` | `--sql` names no preset / trace_processor_shell failed | `perflab analyze … --sql frames` / install from get.perfetto.dev |
| `TOO_FEW_RUNS` / `NOISY` / `CONFOUNDED` | compare inputs | `perflab run … --repeat 2 --alternate` / `--allow-confound` |
| `OVER_BUDGET` / `NOTHING_MEASURED` | `report --gate` | the offending rows |
| `HAZARD_NEW` | `hazards --gate` found a site beyond the baseline | review it; `perflab hazards --write-baseline <file>` |
