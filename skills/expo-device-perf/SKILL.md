---
name: expo-device-perf
description: "Use when an Expo / React Native app janks, stutters, drops frames or responds slowly on a phone (hitches, fps on a 120 Hz / ProMotion display, slow taps, scroll or fling jank, a screen redrawing at rest, a perf fix or regression to prove), or when measuring, comparing or attributing frame cost on a physical iPhone or Android device: perflab, Instruments / xctrace Hitches, Time Profiler, Perfetto, gfxinfo framestats, release perf builds, before/after JS bundle variants, device leases."
---

# Expo device perf

`perflab` (a Výbava applet) owns the lab: the global ledger of physical phones and leases, the native build index keyed by a portable Expo fingerprint, JS bundle export and pack, scenario runs, every trace reader. The project owns the measured interaction (Appium spec, `perfWindow` bracket, scenario table, budgets) in the `perflab` section of its `vybava.config.ts`. You own the judgment: what to measure, how to read it, which render-cost rule fixes it.

## Protocol

    perflab adapter check --json                              # the project's section resolves
    perflab device list --json                                # new phone: device scan, then device add
    perflab lease acquire <device> --purpose "<why>" --json   # token printed once; next lines carry it
    perflab doctor --device <device> --lease <t> --json       # every preflight; fixes in next
    perflab build find --platform <p> --profile perf --json   # miss: build native, once per key
    perflab bundle export --platform <p> --profile perf --ref <base> --label before --json
    perflab bundle export --platform <p> --profile perf --label after --json
    perflab pack --native <key> --bundle <sha> --json         # once per bundle -> variant id
    perflab run <scenario>... --device <device> --lease <t> --variant <before> --variant <after> --alternate --repeat 2 --json
    perflab compare <runDir> --json                           # verdict per scenario, step and metric
    perflab lease release <device> --lease <t> --json

Without a scenario: `perflab probe rest|drag|fling` (Android Perfetto; iOS rest only). Without a device: `perflab hazards <appRoot>`. Stored evidence: `perflab analyze <path> [--marks <file>] [--window a-b --classify] [--sql <preset>] [--reread]`.

Missing binary: `vybava install perflab`. Adapter contract: Výbava `docs/perflab.md`. A native input the Expo fingerprint misses (Reanimated `staticFeatureFlags` in the app's `package.json`) goes in the app's `fingerprint.config.js` `extraSources`.

Every verb emits `{v, ok, verb, data, diagnostics, next}` under `--json`. **The envelope's `next` field IS the protocol**: run it verbatim, on success and failure alike; diagnostics carry a closed code and an exact fix. A failure without an actionable diagnostic and `next` is a perflab bug: fix it in Výbava per AGENTS.md "CLI contract", never work around it with ad-hoc scripts.

Run `build native`, `bundle export`, `run` and `probe` as background tasks of this session (never nohup) and follow the `^perflab\[` progress lines on stderr.

## The loop

1. Reproduce on a release perf build on a physical phone; never a dev client, simulator or emulator for a verdict.
2. `probe rest` first, after one touch: a screen at rest stops drawing (rules 9-11).
3. Measure the interaction with the project's scenario.
4. Attribute: per step (`--marks`), commit vs render, then the slow frame's work (`references/reading-traces.md`).
5. Fix one cause per variant (`references/rn-render-cost-rules.md`); export, pack and run it against the same before.
6. Confirm on the other refresh class (60 vs 120 Hz), the other platform, and once on `build native --kind bundled`.
7. Land it per rule 27, with the runs on a vitrinka board.

App-wide render sweep: `perflab hazards <appRoot>` lists loop, layer and list sites; review each loop for an ambient gate AND real visibility. Cover every route in the project's route inventory (each persona, every tab, top detail screens, each journey state). Per screen on the 120 Hz Android phone: `probe rest` (its touch at 540,210 must hit nothing: `--tap x,y` an inert spot when a control sits there, or the probe measures the state it opened), plus `probe drag` and `probe fling` when it scrolls. Done when every route has a verdict row (`perflab report`) and each failing screen is fixed or has its own task.

## Which instrument

| Question | iOS | Android |
|---|---|---|
| Does it stutter while moving? | hitch ratio (ms/s) + worst hitch | animating fps p10 / median at the display rate + janky % |
| Which step? | `analyze --marks` | `analyze --marks` |
| Commit or render? | hitch narrative + offscreen passes | Perfetto: main `doFrame` vs RenderThread draw |
| What runs in the slow frame? | `analyze <trace> --window a-b --classify` (Time Profiler) | `analyze <pftrace> --sql perframe` / `children` |
| Does it draw at rest? | `probe rest`: main-thread ms/s (`iosRestMainMsPerS`; hitches miss a smooth loop) | `probe rest`: frames, main-thread eglSwaps |
| Is the RenderThread clock-starved? | n/a | `--sql rtfreq` |
| Does it degrade with use? | slope over N cycles | same |
| Is the component compiled? | the project's React Compiler gate (rule 14) | same |
| How much JS per render? | dev-client React Profiler, relative only | same |

Never judge 120 Hz Android by Flashlight FPS or gfxinfo "Janky frames".

## Budgets

| Scenario budget key | Default |
|---|---|
| `iosHitchRatioMsPerSMax` | 5 ms/s (Apple: < 5 good, 5-10 warning, > 10 critical) |
| `iosHitchRatio120MsPerSMax` | 2 ms/s on a 120 Hz phone |
| `iosWorstHitchMsMax` | 16.7 ms, system UI excluded |
| `androidAnimatingFpsP10MinShare` | 0.75 x refresh (90 at 120 Hz) |
| `androidJankyPctMax` | calibrated per scenario |
| `androidRestFramesMax` / `androidRestRunMsMax` | 0 frames after the opening animation / one ambient run <= 6000 ms |
| `androidRestTicksMax` | unset; a screen showing a live value (a 1 Hz countdown) sets it: one short burst per change, the frames outside ticks still judged |
| `androidDragRtDrawMsMax` | 4 ms RenderThread draw per frame |
| `androidFlingTwoVsyncGapsMax` | the baseline's count per 20 s script |
| `slopeMax` | 1.05 (cycleN / cycle1) |
| `iosRestMainMsPerSMax` | 50 ms/s main thread in an iOS `probe rest` (a smooth loop never hitches) |

Scenario rows override with calibrated numbers; a row named `probe-<kind>-<screen>` overlays that probe's default. A loop the product keeps on purpose gets a named exemption on its row (`"exempt": [{name, reason, keys}]`): measured and listed in every report, never a silent pass. The 60-capped `androidFpsP10Min` is reported and never gates a 120 Hz phone.

## Hard laws

1. One holder per phone, perflab verbs only: touch a leased device only through perflab with your token. A one-off adb or devicectl call goes through `perflab device shell <id> --lease <t> -- <args>` (plus `device screencap` and `device pull`); claude-guards' `machine:device-leased` refuses any raw adb, devicectl, xctrace or Appium command naming a leased phone, your own included. Before acquiring, check ListAgents for a peer in the same worktree. A delegated agent (the Codex sidekick) gets the device id and token; never drive the device while it holds the work.
2. Never build native while a run measures on the same Mac; never bypass perflab's build/run serialisation with a raw xcodebuild or gradlew.
3. A verdict compares like with like: same native key, env hash, device and input source. A difference is `CONFOUNDED`, never explained away.
4. A personal phone's protected packages are never uninstalled, cleared or driven by a resetting Appium session.
5. Numbers come from `perflab analyze` over kept evidence (trace, dumps, marks, run.log), never hand-copied or read by an ad-hoc script; after a reader fix, `analyze --reread`.

## References

- `references/rn-render-cost-rules.md`: choosing and landing a fix; the code rules with evidence.
- `references/reading-traces.md`: judging a number, attributing a frame.
- `references/scenario-design.md`: writing or changing a measured scenario.
- `references/device-lab.md`: picking the phone, the perf profile, platform facts perflab cannot read.

Per-project notes: `memo find perf`.

