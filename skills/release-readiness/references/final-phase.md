# Final phase

It runs in a FRESH agent (`Agent({name: "final", prompt: <final-brief.md>})`), or in a fresh session through `/handoff`, and only once every lane has a `results.md` line and the orchestrator has stopped the lane agents and the device runner. From then on the final agent owns the stacks and the devices, under the device-runner rules. Its brief is rendered; this file describes the board it ends with.

## Order

1. **Roll-up.**
   - ONE stack on the merged integration heads, every lane's device journeys, the whole matrix, one commit.
   - Publish it to the epic's roll-up QA task.
   - It is the only run that sees cross-lane collisions in shared test data: payment accounts, seeded tenants, suites that delete a shared account.
2. **Targeted re-runs.**
   - Every failure runs again on its own.
   - Passing alone means interference: fix the test data, not the app.
   - Failing alone means a regression: fix it, or add it to `decisions.md`.
3. **Real device walk** (when `run.json.authority.deviceWalk`).
   - A person-style walk of the top journeys on the release devices.
   - It is not a spec re-run: it shows the screens a user sees.
4. **Release gates.** The adapter's `final.checks`, read-only, on the merged heads. A failing gate is fixed (lane rules apply) or recorded in `decisions.md` with its evidence.
5. **Readiness board**, below.
6. **Close out.**
   - Lane tasks: done, or open with a reason.
   - `hand_back` on the epic.
   - Lane stacks parked. The roll-up stack stays up and hand-testable; its URL goes on the board and in the epic's hand_back.
   - Devices the final agent booted shut down by UDID.
   - The run directory committed to `~/Exports`.
   - The release command (`final.handoff`) named for the human, never run.

## Readiness board

Build one vitrinka board (`<project>-release-readiness-<date>`) from `results.md`, `decisions.md` and the roll-up. Its reading order:

| Card or section | Content |
|---|---|
| Verdict card | Two verdicts: **integration code** GO / GO-with-caveats / NO-GO, and **production deploy** GO / NO-GO until the blockers below are cleared. One paragraph on why. |
| Chips | Roll-up (cases, journeys, pass/fail/partial) · commits (every repo's merged head) · targeted re-run commit · final stack URL · device walk result. |
| Section "Merged-`<integration>` roll-up: go/no-go per lane × device" | A table of lanes × devices with ✅/⚠️/❌ and a note per cell, plus a table "Merged after the roll-up, or still open". |
| Section "Lanes: boards, PRs and lane evidence" | One row per lane: task, board, PRs (merged sha), journeys × devices, bugs fixed, verdict. |
| Section "Real device walk" | A verdict card: what passed, what failed and why, the shots. |
| Section "Production blockers & human decisions" | A verdict card for any incident, then tables from `decisions.md`: **Security**, **Production infrastructure**, **Data migrations & deploy runbook**, **Product decisions**, plus one prose card for dev-infra findings (not prod blockers). |

Every lane board, PR and task in the tables is a full `https://` link. The board's `url` is the one link the hand-back carries.
