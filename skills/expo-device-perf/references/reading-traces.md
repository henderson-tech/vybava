# Reading traces

Evidence cites FixIt, as in `rn-render-cost-rules.md`.

## Any platform

- Report frames presented under a fixed script with the gap buckets, never drop counts alone. Frame counts compare only between runs of the identical script on the same device.
  Evidence: after the hub fix, drops ROSE 340-369 -> 545 because frames presented rose 1189 -> 1592; RenderThread-caused drops fell 299-309 -> 202.
- At rest, frames near refresh x seconds means something loops (rules 9-11). One bounded ambient run (582 frames per 20 s at 120 Hz for a 6 s pulse) is not a leak.
- Degradation is a slope: per-cycle totals over N identical cycles, cycleN / cycle1. Then park 60 s on another tab and push and pop 20 screens; the app should feel like a cold start.
  Evidence: a calendar went 1.30 -> 1.008 after its fix set.
- A win is `improved` in `compare` over at least 2 alternated runs per variant; `within-noise` is not a win. Expect wide spread on a warm phone: one build's worst-decile fps ranged 90.6-102 over three runs, a zoom baseline's median 104-117 over eight.
- Judge an Android draw-cost fix on `rtDrawMatchedClockMs` (the clocks every run drew at), not only `rtDrawAvgMs`: without touch boost the governor's clock mix moves the average more than the fix.
  Evidence: My Offers row layers on the S20, average 4.12/3.90 -> 3.17/2.92 ms, matched 3.89/4.08 -> 2.76/2.94 ms; worst-decile fps stayed within noise.
- Release Hermes JS frames are not symbolicated: attribute by native frames and counters. JS cost per component comes only from the dev client's React Profiler, as relative numbers.
- Proof counters: temporary JS counters in the perf build, never committed, prove a mechanism: commits per transition, formatter constructions per render, mapper starts per mount, GETs per screen open.
- A whole-app slowdown: score each candidate cause against each symptom (tap latency, animation jank, progressive degradation), design a discriminator test that splits the top candidates, and keep a refutations list.

## iOS (Hitches + Time Profiler)

- The hitch ratio is hitch ms per recorded second. On a 60 Hz phone one missed frame is a 16.7 ms hitch: judge 60 Hz and 120 Hz phones separately.
- Per step (`--marks`): "Potentially expensive app update(s)" is our commit on the main thread; "expensive render, N offscreen passes" or "GPU work" is the render server. Offscreen passes per frame: ours 11-16, a UIMenu dismissal 58-82. Long app updates are level 0, app process, >= 6 ms.
- In the hitch window, `--classify` splits main-thread time into mount (create, props, children), Yoga, ShadowTree commit, Reanimated commit, text draw and measure, Skia, CA commit, Hermes GC, UIContextMenu, accessibility automation and run-loop wait. `NSDateFormatter` / `udat_open` in a commit is Intl construction (rule 16).
- System UI has its own floor: measure the system component alone (a menu-only scenario) and exclude a hitch as system only with evidence: the main thread in `_UIContextMenuView _handleSelectionGesture:` > `_UIContextMenuPresentation dismiss`, no RN mount, no JS in the frame.
  Evidence: menu-only 0.89 ms/s over the 27.97 s recorded (0.71 was the same hitch time over the 35 s time limit; the ratio divides by the TOC duration), worst 8.34 ms (Air); on the iPhone 11, 12 of 15 hitches sat at the menu opening.

## Android (framestats at the display rate, Perfetto)

- framestats bins are 500 ms at the display rate: a present interval of n periods drops n-1 vsyncs, a gap over 60 ms is rest, an idle bin counts full rate, an animating bin holds >= 6 intervals, janky = interval over 1.5 periods. Read animating fps p10 and median, and janky %.
- Never judge a phone above 60 Hz by Flashlight FPS (capped at 60; a 120 Hz S20 read 60 in every bin, 554fd04739) or gfxinfo "Janky frames" (62.89% and 0.71% on one identical script).
- Perfetto present gaps at 120 Hz: 7-10 ms is one vsync, 10-18 two, 18-40 three to four, over 40 longer.
- Drops are gaps over 1.4 vsync, blamed on main, rt, both or neither by which thread exceeded a vsync. Most "neither" drops (both threads under 8.3 ms) are the app's: `drops.appDeadline` counts those FrameTimeline marks App Deadline Missed, where main, RenderThread and GPU together overran the frame, so cut the whole frame's cost; only the remainder is the compositor or input cadence.
  Evidence: S20 injected drags, appDeadline of neither: customer home 209/234, worker offers 153/193, money 152/180; the GPU completion wait never passed a vsync.
- RenderThread `Drawing` ms is draw cost; main `Choreographer#doFrame` is UI-thread cost; they join per frame by vsync id.
- Budget for the worst RenderThread clock the governor picks, not the peak: slow drags held it at 0.5-1.2 GHz on little and mid cores (Exynos 990), where a 7.7 ms draw missed 8.3 ms.
- A main-thread `doFrame` every vsync at rest is no loop evidence on React Native 0.86: `FabricEventDispatcher` re-posts its TIMERS_EVENTS callback each frame while the activity is resumed (`BatchEventDispatchedListeners` plus `scheduleVsyncLocked`). Judge rest by presents and RenderThread draws.
  Evidence: S20, 20 s at rest, ~2390 doFrames at 0.75-0.85 ms on every screen, a 0-present chat list included.
- Main-thread eglSwapBuffers per frame is a Skia or GL canvas redrawing. `drawLayer` counts say how often each hardware layer re-renders.

`analyze <pftrace> --sql <preset>`:

| Preset | Answers |
|---|---|
| `frames` | FrameTimeline jank, top-level main and RT slices, SurfaceFlinger composite |
| `present` | present gaps per bucket, SF `gpu_composition` and jank, main and RT running ms per CPU cluster |
| `perframe` | per frame: main `doFrame` (animation, traversal, input) and RT `DrawFrames`, by vsync id |
| `perframe-names` | main-thread slices that run every frame |
| `children` | children of the animation, traversal and input phases, RT depth 2, busy app threads |
| `layer`, `damage` | `drawLayer` and `Drawing` counts and bounds |
| `rtfreq` | RT draw ms by CPU and MHz |
| `egl-anc` | what triggered a main-thread eglSwap |
| `anim-kids` | per-frame callbacks under the main-thread animation slice |
| `upload` | bitmap uploads, Fabric mount items, commits |
| `skia` | RenderThread Skia ops |
| `input` | work under `deliverInputEvent` |
| `jank` | jank type by present type |

