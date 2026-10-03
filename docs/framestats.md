# framestats - Android frame metrics for agents

Two readers over the frame records Android keeps, each one verb with the
runx envelope (`{v, ok, verb, data, diagnostics, next}` under `--json`; exit
0 ok, 1 infra, 2 diagnostics). Every millisecond is rounded to 0.01; a metric
with no samples is `null`, never a sentinel number, so a budget such as
`<= 8.4` cannot pass on an empty run.

```sh
vybava install framestats
framestats parse drag-*.framestats.txt --json                  # one run, many dumps
framestats parse commits.framestats.txt --after-release-ms 250 --json
framestats perfetto swipe.pftrace --package app.fixit.client --json
framestats perfetto swipe.pftrace --package app.fixit.client \
  --count ReanimatedModuleProxy::commitUpdates --count mountViews --json
```

## `parse <framestats.txt>...`

Input: `adb shell dumpsys gfxinfo <pkg> framestats` output. The ring holds
about 120 frames (one second at 120 Hz), so a longer run is dumped several
times (after each gesture, or on a timer) and every dump is passed: rows are
deduped by `IntendedVsync`, and rows with `Flags != 0` (a frame the system
marked unusable) are counted in `rowsFlagged` and skipped. A dump without a
`---PROFILEDATA---` block is `NOT_FRAMESTATS`; a header without `Flags`,
`IntendedVsync`, `SwapBuffers` or `FrameCompleted` is `MISSING_COLUMNS`. A
row whose field count differs from the header's (a line cut mid-write), that
carries a non-integer, or that completes before its `IntendedVsync` is never
measured: it counts in `rowsMalformed` (per dump, not deduped, outside
`rowsTotal`) and raises the `MALFORMED_ROWS` warning.

Definitions (a frame is one kept row):

| Field | Meaning |
|---|---|
| `cadenceMedianMs`, `cadenceP90Ms` | Deltas of consecutive `IntendedVsync`, gaps over 100 ms excluded. `inputCadenceMedianMs` / `settleCadenceMedianMs`: pairs of input frames / pairs of non-input frames. |
| `presentIntervalP50Ms`, `P90` | Deltas of consecutive `DisplayPresentTime` (what the panel showed), gaps over 100 ms excluded. `inputPresentIntervalP50Ms` / `P90`: pairs of input frames, the "present while dragging" number. |
| `frameP50Ms` ... `frameMaxMs` | `FrameCompleted` (or a later `GpuCompleted`) minus `IntendedVsync`. |
| `over8ms`, `over16ms` | Frames over 8.33 / 16.67 ms. `deadlineMissed`: completion after `FrameDeadline`. |
| `stageMediansMs`, `stageP90Ms` | Per-stage durations: `wake(vsync->input)`, `input`, `animation`, `traversal`, `draw(record)`, `syncQueueWait`, `sync`, `issueCommands`, `swap`, `gpu` (`GpuCompleted - SwapBuffers`), `uiThread(vsync->syncQueued)`, `renderThread(syncStart->completed)`. |
| `firstInputFramesTotalMs` | The run's first five input frames, with their stages in `firstInputFramesStages`. |
| `gestures[]` | Runs of input frames (`InputEventId != 0`) no more than 100 ms apart: `firstInputFrameMs`, `inputFrames`, and `releaseWindow` = the frames starting within `--after-release-ms` (default 250) after the gesture's last input frame (`frames`, `over16ms`, `maxFrameMs`). |
| `firstInputFrameMaxMs`, `releaseOver16ms`, `releaseMaxFrameMs` | The worst first input frame, and the release windows summed over every gesture. |

`--rows` adds one row per frame (`tMs`, `vsyncId`, `input`, `totalMs`,
`uiMs`, `renderThreadMs`, `gpuMs`, `presentMs`). `NO_FRAMES` (warning) means
every row was flagged or malformed: dump again right after the interaction.

## `perfetto <trace.pftrace> --package <pkg>`

Input: the binary trace `perfetto` writes (text and JSON exports are
`NOT_A_PERFETTO_TRACE`). A protobuf stream that breaks after the first packet
(pulled while perfetto was still writing, or corrupt) is `TRACE_INCOMPLETE`
and measures nothing: its prefix would silently drop the later frames. The
config needs `linux.ftrace` with
`ftrace/print`, `atrace_categories` `gfx`, `view`, `input` (plus `dalvik`
for ART pauses) and `atrace_apps: "<pkg>"`, and the
`android.surfaceflinger.frametimeline` data source; `linux.process_stats`
names the pid directly. The pid comes from `--pid`, else the process tree,
else a FrameTimeline layer `<pkg>/...`, else a main thread whose comm is
the package's last 15 characters; none of them is `PACKAGE_NOT_IN_TRACE`.

Slices are rebuilt from the atrace `B|pid|name` / `E` markers per writer
thread, keeping those the app's pid began. A UI frame is the main thread's
`Choreographer#doFrame <vsyncId>`; its RenderThread work is every
`DrawFrames <vsyncId>` with the same id on the thread that draws the most
of them. A **drag frame** is a UI frame containing Choreographer's `input`
section (the frame consumed a touch).

| Field | Meaning |
|---|---|
| `uiFrames`, `renderFrames` | doFrame / DrawFrames durations: `p50Ms` ... `maxMs` over all, `dragP50Ms` ... `dragMaxMs` over drag frames. Unmatched draws (`DrawFrames -1`, a replay encoder) count in `renderFrames` but never as drag frames. |
| `counts[]` | Per `--count` substring (`Texture upload` is always first): `total` app slices, `inFrames` nested in a frame's doFrame (main thread) or DrawFrames (RenderThread), `inDragFrames`, `dragFramesWith`, `perDragFrameP50`, `perDragFrameMax`, `totalMs`. |
| `uploads` | Texture uploads by size (the `WxH` in the slice name): `large` = at least `largeMinPixels` (256 x 256; glyphs stay below, clip masks and layers are card-sized), `largeInDragFrames`, `largePerDragFrameP50`, `largePerDragFrameMax`, `uploadMsPerDragFrameP50`. |
| `artPauses` | `Mutator threads suspended for <cause>` slices: `count`, `totalMs`, `maxMs`, `duringDragFrames` (overlapping a drag frame), `byCause[]`. |
| `frameTimeline` | The app's actual surface frames: `presentTypes` (ON_TIME, LATE, EARLY, DROPPED, UNKNOWN, UNSPECIFIED), `jankTypes` (one count per bit: NONE, APP_DEADLINE_MISSED, BUFFER_STUFFING, ...), `jankyFrames` (any bit but NONE), and present-to-present intervals, a present being the end of the display frame the surface frame landed in (`presentIntervalP50Ms`, `dragPresentIntervalP50Ms`, P90s). |

A `totalMs` (in `counts[]` and `artPauses`) is `null` when no slice matched;
the integer tallies beside it stay `0`.

`--frames` adds one row per UI frame (`vsyncId`, `startMs`, `uiMs`,
`renderMs`, `drag`, `counts`). Warnings ride a successful envelope:
`NO_APP_FRAMES`, `NO_RENDER_THREAD`, `NO_VSYNC_IDS` (Android < 12),
`NO_FRAME_TIMELINE`, each with the config fix in `next`. Under
`NO_VSYNC_IDS` the outermost id-less doFrames are still the UI frames, so
`uiFrames` (drag ones included) and `artPauses.duringDragFrames` are
measured; what needs an id stays empty: `renderFrames` drag values, the
per-frame `counts[]` and `uploads` values, FrameTimeline drag attribution and
each `--frames` row's `vsyncId` (`null`).

## Diagnostics

The closed enum lives in `internal/framestats/diag.go`: `USAGE`,
`FILE_UNREADABLE`, `NOT_FRAMESTATS`, `MISSING_COLUMNS`, `MALFORMED_ROWS`,
`NO_FRAMES`, `NOT_A_PERFETTO_TRACE`, `TRACE_INCOMPLETE`,
`PACKAGE_NOT_IN_TRACE`, `NO_APP_FRAMES`, `NO_RENDER_THREAD`, `NO_VSYNC_IDS`,
`NO_FRAME_TIMELINE`, `COMPACT_SCHED` (Android 16's traced writes compact
sched bundles by default; the RenderThread's CPU and clock placement needs
full `sched_switch` events: record with `compact_sched { enabled: false }`).

## Origin

Ported from the 2026-09-30 S20 swipe-deck measurement (`fsparse` and
`pfwalk`); `internal/framestats/testdata/s20-drag.framestats.txt` is 40 rows
of that run and the parse test pins the original tool's numbers on it.
