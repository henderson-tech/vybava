# release-readiness — decisions (2026-09-24)

**Problem.** Turn the release-readiness workflow run on Reservine devlp (2026-09-22/23) into a reusable Výbava skill with a small per-project adapter, then write a FixIt adapter. Source run: `~/Exports/Reservine/release-readiness-2026-09-23/` (WORKFLOW-NOTES.md, lane-rules.md, the ledgers).

## Decisions

- **Adapter.** A `readiness` section in `vybava.config.ts`, read by a new `readiness` Go applet (`check`, `range`, `init`, `render`). Its TypeScript twin `ReadinessConfig` lives in `config-helpers.ts`. Why: typed in the editor, unknown keys rejected, and schematic rendering of lane rules, bodies and briefs.
- **FixIt iOS and Android.** One orchestrator-owned device-runner agent on the Mac drives the iOS Simulator and an Android emulator at the same time. Why: FixIt's realtime features (chat without refresh, inquiry live tracking) must be tested across devices at once; runs happen at night.
- **Device concurrency.** FixIt `guards.simCap` goes from 2 to 4, always (two iOS+Android pairs).
- **Realtime pairing.** Both directions: customer@iOS × worker@Android, and the swap.
- **App build.** Release builds everywhere, including lane evidence.
- **Per-lane concurrency.** Every FixIt golden resets its lane's one e2e DB. So realtime specs run iOS + Android at once (both directions); single-device specs run one platform after the other within a lane; two lanes run at once to fill the 4 devices. The adapter says `oneRunPerLane: true`.
- **Tool fixes.** Filed and linked, not built here:
  - fixit/devbox#3259: sibling-worktree sync by default;
  - fixit/devbox#3260: admission gates on slot count, load and pids;
  - fixit/vitrinka#3261: refuse a publish that silently climbs to the epic's QA task;
  - existing: fixit/vitrinka#3122 (shots by full path) and henderson-tech/devbox#88 (slice headroom).
  `slot` and `uniq-shots.sh` stay as workarounds until these land.
- **Go live.** Cut a Výbava release after the merge, then `vybava upgrade`.

## Assumptions

- `eve-ai-layer` rides in the FixIt adapter as a second repo: production = the newest `fixit-v*` tag, integration = `main`.
- Web surfaces (web, admin-web) stay on Playwright desktop 1440×810 on the Devbox. `supportsTablet: false`, so there is no iPad in the matrix.
- Heavy Devbox jobs go through `devbox run` (its backfill queue is no longer strict FIFO). `slot` is for jobs that bypass it.
- Run directories live in `~/Exports/<Project>/release-readiness-<date>/`.
- Lanes are named Agent-tool agents; only the read-only inventory is a Workflow.

## Found while building

- prm's `merge.md` readied drafts automatically, while `SKILL.md` said a `--draft --auto` merge waits on `gh pr ready`. Aligned: a draft is a STOP. The lane rule "the evidence-gated PR stays draft until the device evidence exists" depends on it.
- FixIt has no iOS-simulator release E2E build (the `e2e-android` APK bakes `localhost:3001`). The adapter lists both release builds, the AVD and the cross-platform realtime proof as phase-3 plumbing; `readiness check` flags them.
- An adversarial review (4 lenses) found, among others, that every FixIt Appium run binds port 4723 / WDA 8100, that `vitrinka qa run --` has no shell, and that FixIt already had an iOS release build (`e2e:build`). The applet now has `{port}` / `{driverPort}` / `{<role>Udid}` tokens, `oneRunPerLane`, per-repo `worktree` commands, SHA-frozen ranges, and a final phase gated on every lane having a results line.
