# Scenario design

A scenario is the project's measured interaction: one spec case wrapped in its `perfWindow` bracket, plus one row in the scenario table. perflab runs it; never write or run an ad-hoc webdriver session. Evidence cites FixIt, as in `rn-render-cost-rules.md`.

## The window

- It covers only the interaction. Seeding, sign-in, geometry probes, setup scrolls and a warm-up cycle stay outside it.
- Inside it: one batched W3C pointer sequence, or replayed recorded points. No element lookup and no HTTP call between strokes.
- Its length derives from the interaction's constants; never pad it.
  Evidence: a 600 ms engagement hold per stroke added 7.2 s of stationary time and raised the iOS hitch allowance 81 -> 117 ms (Eve 4164007621, 7d8910f0cb).
- iOS windows stay 60-105 s.
- No XCUITest query inside a window (each snapshots the accessibility tree on the main thread, ~100 ms). Record the round by element once (rect centres), then replay W3C touch taps (`move` duration 0, `down`, `pause` 40 ms, `up`) with fixed pauses. A round ends where it began.
- A recorded point is the element's unoccluded part: clip the rect at the overlay's top minus 8 pt, refuse below 16 pt, and allow up to 4 short drags to clear it.
- Prove the measured gesture moves: play one unmeasured stroke of the exact gesture, assert the anchor travelled > 64 px (twice the 32 px jitter tolerance) or left the viewport, restore it, and assert it is back within 32 px before the window opens. Settled reads are two rect samples 300 ms apart within 2 px; each probe moves at least half its travel; edge sentinels stay offscreen.
  Evidence: a swallowed fling passed with cheap frames (Eve 4163608428, 0470631507).
- On Android assert half-travel and edges, never 1:1 tracking.
  Evidence: a 600 px stroke scrolled 407-465 px on the CI emulator.
- A rest scenario touches once (the ambient gate open is the worst case), then holds still for 20 s.
- Every case asserts it ends on its expected screen, never the error boundary.

## Setup per platform

- UiAutomator2 lists only on-screen elements (XCUITest also lists below the fold): on Android, scroll first, then look up. A resourceId selector never finds an offscreen RN view; `allowInvisibleElements` exposes mounted offscreen sentinels for edge probes.
- A fresh iOS install chains permission prompts (notifications, then location): answer them until 4 s of quiet, capped at 20 s; an unknown alert fails the case. Android sets `autoGrantPermissions`.
- Sign-in and world reset go through the adapter hooks (`perflab app link`, `perflab app reset`), never typed UI.

## Table row and runner contract

- The adapter's `scenarios` command prints JSON rows `{name, windowMs, budget}`; budget keys are the closed vocabulary in SKILL.md "Budgets". Several scenarios of one spec share one session.
- The runner takes its mocha timeout from `{timeoutMs}` (windows + 20 min per case): wdio's wrapper timeout overrides `this.timeout()`, and a trace pull outlasted the 420 s default.
- `perfWindow` appends `{wallMs, label}` per step to the marks file the adapter maps to `{runDir}/marks.jsonl`; without marks there is no per-step attribution.
- An iOS capture records the Hitches and Time Profiler instruments only, never the Animation Hitches template.
  Evidence: its GPU and Metal tracks made 65 s weigh 7.4 GB and the pull came back malformed.
- An Android capture never runs Flashlight beside Perfetto (atrace fights over ftrace).

## Experiment design

- At least 2 runs per variant, alternated (`--alternate`), never batched.
- One change per variant when attribution matters.
  Evidence: one iteration measured three changes at once after a duplicate agent's edit landed before the export.
- Seed the data the question needs: a heavy world for render cost, a sparse one for ratchets (rule 18).
- N identical cycles and a slope for anything that can ratchet.
- Tag the input source (adb, W3C, human) and confirm gesture work with a real finger: adb input reads pessimistic (`device-lab.md`).
- Frame lab: when live data is not reproducible, an offline dev route drives the real components over typed fakes, with its window length imported from the route's own script.

## Visual parity (after layering or canvas changes)

Step-through screenshots, fling end states and 100% crops: no blank or missing cards after flings; layered corners and text crisp, no resampling blur; a pressed row shows its pressed state through the layer; ambient loops still animate. Assert canvas-drawn state the accessibility tree cannot see from screenshots: per-cell ink contrast against the cell background, with the cell rects saved beside each shot.

