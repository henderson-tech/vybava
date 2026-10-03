# framestats fixtures

| File | Source | Notes |
|---|---|---|
| `s20-drag.framestats.txt` | S20 drag capture | the `framestats parse` reference dump |
| `a13-switch-multi-kept.framestats.txt` | `~/Exports/FixIt/perf/2026-10-01-calendar/head-s20/switch-multi-kept-framestats.txt` | Android 13: the header lists FrameInterval before FrameStartTime, the rows the other way round; the last four rows carry a DisplayPresentTime ~3.5 s before their own vsync |
| `lab120-{before,syncprops,layer}.calendar-view-switch-smooth-android-<stamp>.json.gz` | `lab120-*/frames/<same name>.json` | the poll sidecar compacted: every dump kept (uptime, host time), each frame listed once in the last dump that listed it (the frame the reader keeps). The TypeScript reader reproduces the published display and per-step numbers from these files |
| `lab120-*.taps.log` | `lab120-*/run.log` | only the `COMMAND performActions` lines |

Perfetto traces are too big for the repo: `present_test.go` synthesizes
packets, and `TestReadPresentOnTheS20LabTraces` reads
`$PERFLAB_FIXTURES_DIR/lab120-traces/fixit-switch-before.pftrace` when the
variable is set (point it at `~/Exports/FixIt/perf/2026-10-01-calendar`).
