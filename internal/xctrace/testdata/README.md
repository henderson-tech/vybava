# xctrace fixtures

Real `xctrace export` output from the 2026-10-01 calendar campaign
(`~/Exports/FixIt/perf/2026-10-01-calendar/`), re-exported with Xcode's
xctrace 16.0 on 2026-10-02. Device display names are replaced by "Lab iPhone"
and "Lab Mac"; nothing else is edited unless noted.

| File | Source | Notes |
|---|---|---|
| `ios26-smooth.toc.xml`, `ios26-smooth.hitches.xml` | `it4-confirm/calendar-view-switch-smooth-ios-1790904629136.trace` (iPhone Air, iOS 26) | `hitches` table: 7 hitches, 75.03 ms over 93.28 s, 0.80 ms/s |
| `ios26-smooth.hitches-renders.prefix.xml.gz`, `ios26-smooth.hitches-updates.prefix.xml.gz` | same trace | the rows starting before 10 s (a prefix keeps every ref valid) |
| `ios26-smooth.steps.json` | same trace's `.steps.json` (maptaps.ts) | trace-relative steps, no tap lag |
| `ios26-run.log` | `it4-confirm/run.log` | only the recording markers and `COMMAND performActions` lines |
| `ios18-iphone11.toc.xml`, `ios18-iphone11.hitches-summary.xml` | `it4-iphone11/calendar-view-switch-smooth-ios-1790905100234.trace` (iOS 18.7.8) | `hitches-summary` beside `*-interval` siblings: 12 hitches, 2.77 ms/s (the published 13.92 summed the siblings) |
| `menu-floor.toc.xml`, `menu-floor.hitches.xml` | `smooth-it1/calendar-mode-menu-ios-1790892152253.trace` | the UIMenu floor: 3 hitches, 25.01 ms over 27.97 s |
| `export-missing-template.stderr` | `before/calendar-view-switch-ios-1790880107904.trace` | `xctrace export --toc` stderr, exit 10 |
| `record-malformed.log` | `before/turn5-run1.log` lines 4236-4241 | a real `* [Error] Transferred trace file is malformed` block |
| `time-profile.prefix.xml.gz` | `/tmp/calendar-perf/it4c-tp.xml` (it4-confirm time profile) | the first 300 rows, `<binary>` elements stripped (no refs point at them) |
| `synthetic-no-hitches.toc.xml` | synthetic | a run whose only hitch tables are stage siblings |
