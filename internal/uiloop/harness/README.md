# ui-loop harness (vendored)

This directory is written by `vybava ui-loop sync` from the Výbava binary
(`internal/uiloop/harness` in henderson-tech/vybava). `STAMP.json` records the
vybava version and every file's sha256; `vybava ui-loop check` fails when a
file drifts.

**Never edit these files in the repo.** Change them in Výbava, release, then
run `vybava ui-loop sync` here. `sync` refuses to overwrite a file edited in
place unless `--force` is given. Keep this directory out of the repo's
formatters and linters (prettier, eslint): a reformat is drift.

What the repo owns instead: `../project.ts` (sign-in, params, theme, settle,
chrome selectors) and `../screens/*.ts` (the screen manifest).

| File | Role |
|---|---|
| `manifest.ts` | The screen vocabulary: `Screen`, `Step`, `Recipe`, `defineScreens`, `validateScreens`. |
| `project.ts` | `defineProject`: the repo's glue. |
| `states.ts` | Empty / error / loading recipes by request mocks (`listStates`, `emptyList`, …). |
| `viewports.ts` | Built-in viewports (mirrored in Go). |
| `run.ts` | The run.json contract, selection, params, pass layout. |
| `capture.ts` | Context, safe area, recipe runner (`runSteps` for recipe functions), settle, shots, glass probe, shot records. |
| `lint.ts` | The in-page layout lint (self-contained; runs through `page.evaluate`). |
| `capture.spec.ts` · `setup.ts` · `teardown.ts` · `playwright.config.ts` | The Playwright entry: one test per screen × viewport × theme. |
| `report.ts` | `report.json` / `report.md` per pass; the teardown then writes `done.json` (`{ v, pass, run: run.json createdAt, finishedAt, shots }`), which `publish --follow` stops on. |
| `check.ts` · `render-app-map.ts` | Manifest validation and the app map, run by `ui-loop check` / `ui-loop map`. |

Docs: `docs/uiloop.md` in Výbava.
