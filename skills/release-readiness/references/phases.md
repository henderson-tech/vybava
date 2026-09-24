# Phases

The run directory (`readiness init`; default `~/Exports/<exports>/release-readiness-<date>/`) holds everything:
- `run.json`: the frozen ranges, the authority answers and the epic.
- `inventory-args.json`, `inventory.json`, `lanes.json`.
- The rendered files: `lane-rules.md`, `body-*.md`, `brief-*.md`, `device-runner.md`, `final-brief.md`.
- The ledgers: `results.md`, `decisions.md`, `rotation.md`.
- The copied scripts: `slot`, `uniq-shots.sh`, `inventory.workflow.js`.

Commit it to `~/Exports` when the run ends.

## §0 Authority questions: once, batched, up front

Ask them before anything else, in ONE AskUserQuestion round, recommended option first:
- **Merge authority:** `/prm --auto --audit` (lanes merge after the audit, with CI and bots green), or human merge.
- **Device matrix in scope:** the adapter's `devices.matrix` ids, and whether the final phase includes a real device walk.
- **Concurrency ceiling:** lane agents alive at once. The orchestration default is ≤ 4 per phase; the human may raise it (Reservine ran 12 lanes plus fixers under a ceiling of 20).
- **Finish line:** "integration ready + readiness board", never the release itself. Also: attach to an existing epic, or create one.

Record the answers in `run.json.authority` (`merge`, `devices` (matrix ids), `deviceWalk` (true for a final device walk), `concurrency`, `finish`). `readiness render` refuses to run without `merge` and `finish`.

## §1 Inventory

1. Pick 3–4 **journey clusters**: families of user journeys, not code areas. To do so, skim `git -C <path> log --no-merges --format='%h %s' <range>` and `git diff --dirstat <range>` for each range in `run.json`. Write each cluster into `inventory-args.json.clusters` as `{name, focus}`, where focus is one sentence on what belongs.
2. Run the workflow: `Workflow({scriptPath: "<run-dir>/inventory.workflow.js", args: <inventory-args.json>})`. It runs one read-only reader per cluster, then a completeness critic that attributes every commit.
3. Write `inventory.json` from the workflow's output file with `jq '.result'`. Read only the critic (`jq '.critic'`) and per-cluster counts, never the whole file.
4. Act on the critic:
   - fold every unassigned functional commit into a lane: list them with `jq -r '.critic.unassigned|to_entries[]|"\(.key) \(.value.ref) \(.value.subject)"' inventory.json` and put the indices in that lane's `unassigned`;
   - drop phantom claims;
   - settle overlaps.
   Every open question becomes a line in `decisions.md`.

## §2 Epic, lanes, stories, QA tasks

1. **Split the features into lanes** in `lanes.json`: `[{slug, title, features: ["<i>.<j>"], journeys: ["<i>.<j>"], unassigned: [<k>], stack: "full"|"light"}]`. A ref `<i>.<j>` is 0-based: cluster i of `.inventories`, then item j of its `.features` (or `.journeys`). `jq -r '.inventories|to_entries[]|.key as $c|.value.features|to_entries[]|"\($c).\(.key) \(.value.name)"' inventory.json` lists them. Slugs are unique lowercase kebab-case.
   - A lane is one coherent journey family, about 3–7 features.
   - `light` means tests only, with no dev stack.
   - Reservine: 4 clusters became 12 lanes.
2. **Create the epic** in the adapter's vitrinka project, then record `run.json.epic {id, url}`: `vitrinka task create "<title>" --type epic --project <project> --json`.
3. **`readiness render`** writes one `body-<slug>.md` per lane: features, refs, surfaces, existing tests and a gap checklist.
4. **Per lane, create** (through the CLI, so no body passes through your context):
   - the story: `vitrinka task create "<title>" --type story --parent <epic id> --project <project> --body "$(cat <run-dir>/body-<slug>.md)" --json`;
   - its QA task: `vitrinka task create "QA: <title>" --type qa --parent <story id> --project <project> --json` (the server accepts `qa` though the help lists only the common types);
   - its board: `vitrinka task qa-board <qa id>`.
   Then check that `vitrinka task resolve-qa --task <story id>` resolves to that lane's QA task, for EVERY lane. A story without a QA child silently falls back to the epic's board (fixit/vitrinka#3261).
   Only after all lanes pass, create the roll-up QA task (`--type qa --parent <epic id>`) and record it as `run.json.rollup`. Created earlier, it would catch every stray lane publish.
5. **Record** `story {id, url}`, `qa {id, url}` and `board` in `lanes.json` (ids as vitrinka returns them), then run `readiness render` again: it now writes `brief-<slug>.md`, and its `next` names the spawns.

## §3 Shared plumbing first

Small PRs merge before any lane needs them:
- the adapter's `plumbing` list;
- every `DEVICE_BUILD_MISSING` from `readiness check`;
- device-matrix projects, CI browser or driver installs, and dev-env fixes.

Then verify the topology on ONE lane: bring its stack up, run the adapter's `devEnv.checks`, take one device evidence pass, publish it, and check the cards (`scrape_board`, `get_card_image`). What breaks here would break every lane; fix it in plumbing, re-render and only then fan out.

## §4 Lane execution

- **Spawn** each lane as `Agent({name: "lane-<slug>", prompt: <contents of brief-<slug>.md>})`, in the waves of `capacity.md`. The brief points at `lane-rules.md`; never paste rules into the prompt.
- **Device runner.** With `devices.runner: device-runner`, spawn it once with `device-runner.md`. Lanes send `DEVICE REQUEST …` to the orchestrator, which queues them to the runner and relays `BUILT` and `DEVICE RESULT` back.
- **What a lane does:**
  - tests its story's features live on its stack and closes the gap checklist;
  - fixes what it finds, with failing-first tests;
  - takes device evidence at one commit;
  - publishes to its own QA task;
  - merges its PRs in the adapter's order (the evidence-gated PR stays draft until the evidence exists);
  - `hand_back` on its story, then returns the lane JSON.
- **Record** each lane outcome as one line in `results.md`, and each human-owned item as one line in `decisions.md`.

## §5 Capacity governance

See `capacity.md`.

## §6 The orchestrator's role

- **Relay.** Sub-agents such as helpers, auditors and fixers often cannot message a lane owner: forward their findings, attributed.
- **Broadcast** box-wide findings (a root cause, a new limiter rule, a board-integrity bug) with `messages.md` templates, and **retract** a wrong broadcast just as fast.
- **Shared fixes.** A bug that blocks several lanes (a tenancy bug that hangs every stack, say) gets ONE named fixer agent and its own PR. The other lanes carry that commit as a cherry-pick only (`messages.md` "shared fix") and merge the integration branch once it lands, so it drops out of their diffs. Record each fixer's outcome in `results.md` like a lane's.
- **Ledgers.** Keep `results.md` (lane outcomes), `decisions.md` (the human's gate list) and `rotation.md` (waves, GO/park, resumes).
- **Stop finished agents.** Send a shutdown to lanes that handed back, stop monitors, and end the swarm when its goal ends. Never park idle agents.
- **Usage limits.** Wait for the reset. Resume transcript agents by message (`messages.md` "resume"). Re-spawn a dead lane from a state snapshot (git status/log, PR state, run logs) plus its brief.
- **Hand off the orchestrator role** at ~70% context (`claude-guards ctx "$CLAUDE_CODE_SESSION_ID"`) while lanes are still live: `/handoff` to a fresh session with the run directory, `rotation.md` and `results.md`. The lanes keep reporting to "the orchestrator", which is now that session.
- **Start the final phase** only when every lane in `lanes.json` has a line in `results.md`. First send every lane agent and the device runner a shutdown and `TaskStop` their monitors: the final agent then owns the stacks and the devices, and never overlaps a live lane.

## §7 Final phase, in a fresh agent

`Agent({name: "final", prompt: <final-brief.md>})`, or a `/handoff` into a fresh session when this one is past ~70%. The brief's step 0 refuses to start while any lane lacks a `results.md` line. See `final-phase.md`.
