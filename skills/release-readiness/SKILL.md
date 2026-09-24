---
name: release-readiness
description: "Use when a whole integration branch must be made release-ready before production — 'release readiness', 'release prep', 'prepare the release', 'QA everything since the last release', 'overnight release lanes', or a go/no-go board per lane × device. The project needs a `readiness` section in vybava.config.ts (`readiness check` says)."
---

# release-readiness

This session is the orchestrator. It turns the diff between what production runs and the integration branch into lanes: one named lane agent per journey cluster, each with its own story, QA task, usertest board, dev stack and PRs. Every lane is bound by ONE rendered `lane-rules.md`.
- The `readiness` applet owns everything deterministic: the project adapter (the `readiness` section of vybava.config.ts), the frozen ranges, the run directory and every rendered file.
- The orchestrator owns judgment, relaying, capacity and the ledgers.
- The run ends when the integration branch is ready and the readiness board is published. It never runs the release itself.

## Protocol

```sh
readiness check --json                     # adapter valid, ranges resolve, simCap vs device budget, pending plumbing
# phase 0: ask the authority questions ONCE, batched (references/phases.md §0)
readiness init --json [--dir <run-dir>]    # run.json with frozen ranges, inventory-args.json, slot, uniq-shots.sh, inventory.workflow.js, ledgers
# record the answers in run.json .authority; write 3–4 journey clusters into inventory-args.json .clusters
Workflow({scriptPath: "<run-dir>/inventory.workflow.js", args: <inventory-args.json>})   # phase 1: readers + completeness critic
jq '.result' <workflow output file> > <run-dir>/inventory.json       # never read the result into context
# phase 2: write lanes.json, create the epic, record run.json .epic
readiness render --dir <run-dir> --json    # lane-rules.md, body-<slug>.md, device-runner.md, final-brief.md
# one story per lane (body = body-<slug>.md) + one QA task per story + its board; record the ids in lanes.json
readiness render --dir <run-dir> --json    # now also brief-<slug>.md
# phase 3: shared plumbing PRs merge before any lane needs them; verify the topology on ONE lane
# phase 4: Agent({name: "lane-<slug>", prompt: <brief-<slug>.md>}) per lane, in waves; Agent({name: "device-runner", prompt: <device-runner.md>})
# phases 5–6: govern capacity, relay, keep results.md / decisions.md / rotation.md
readiness render --dir <run-dir> --check   # before every spawn and broadcast: the rendered files match their inputs
# at ~70% context with lanes live: /handoff the orchestrator role (references/phases.md §6)
# phase 7: every lane has a results.md line; stop lanes + runner, then Agent({name: "final", prompt: <final-brief.md>})
```

Every verb emits `{v, ok, verb, data, diagnostics, next}` under `--json`.
**The envelope's `next` field IS the protocol.** Run what it says, verbatim, on success and on failure; diagnostics carry a closed code and an exact fix. A failure without an actionable diagnostic and `next` is a CLI bug: fix it in Výbava (`internal/readiness/`), never investigate around it.

References (`references/`):
- `phases.md`: what each phase does.
- `capacity.md`: limiter, waves, park/hold, validity.
- `messages.md`: park, GO, resume, broadcast and retract texts.
- `final-phase.md`: roll-up and the readiness board layout.
- `lessons.md`: why each law exists.

The adapter's shape is in Výbava `docs/readiness.md`.

## Hard laws

1. **Lanes, the device runner and the final agent are NAMED Agent-tool agents** (`Agent({name})`, messageable by name), never Workflow agents: a `SendMessage` to a workflow agent id resumes a DUPLICATE beside the original. Workflows only run the read-only inventory.
2. **One rules file, rendered.** Every lane gets its brief plus `lane-rules.md`. A rule change is an input change (adapter or run.json), a re-render and a broadcast; never per-lane prose.
3. **Verify before fanning out.** Walk the dev topology end to end on ONE lane (stack, one device evidence pass, publish, card check). Find the real resource ceiling (container cgroup slice, pids, simCap) before wave A.
4. **Evidence is valid or it is nothing.**
   - Every device result comes from ONE commit.
   - A retry-pass is not a pass.
   - A run is invalid on exit 137, a restart mid-run, a launch error, or a skip on an unreachable-dependency guard.
   - Fixes need failing-first tests.
   - Lanes publish only to their OWN QA task, and the QA tasks exist before any publish.
5. **Capacity is governed.**
   - Every heavy job goes through the adapter's `lane.heavy` wrapper (or `slot`).
   - Lanes run in waves with park/GO messages.
   - A stack is held while active and parked while waiting.
   - Processes are killed by recorded PID only.
   - With a device runner, only it touches simulators and emulators.
6. **The run stops at "integration ready + readiness board".** The release (the adapter's `final.handoff`) and every line of decisions.md belong to the human. Authority questions are asked once, up front; lanes act on run.json, never on a fresh question.
