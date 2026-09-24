# readiness — release readiness across a whole integration branch

`readiness` is the deterministic layer of the `release-readiness` skill (`skills/release-readiness/`). The skill orchestrates a multi-lane release prep:
1. authority questions up front;
2. an inventory with a completeness critic;
3. one named lane agent per journey cluster, each with its own story, QA task and usertest board;
4. capacity governance;
5. a merged-branch roll-up and a readiness board.

The applet owns everything that must not depend on judgment: the project adapter, the frozen ranges, the run directory and every rendered file.

```text
readiness check [--no-fetch] --json       # validate the adapter, resolve every range, compare the device budget with guards.simCap
readiness range [--no-fetch] --json       # production..integration and its commit count, per repo, now
readiness init [--dir D] [--date D] --json  # seed a run directory (never overwrites run.json, ledgers or copied scripts)
readiness render --dir D [--check] --json  # lane-rules.md, body-<slug>.md, brief-<slug>.md, device-runner.md, final-brief.md
```

Every verb emits the `{v, ok, verb, data, diagnostics, next}` envelope; the diagnostic codes are the closed enum in `internal/readiness/diag.go`.

## The adapter: `readiness` in vybava.config.ts

One section per project, typed by `ReadinessConfig` in `.vybava/config.ts`. Unknown keys are rejected (`Config.Section`), and `Validate` reports every problem at once:
- enums;
- ids;
- merge order;
- `{token}` placeholders a field does not define.

```ts
readiness: {
  vitrinka: { workspace: 'fixit', project: 'fixit' },
  exports: 'FixIt',                              // default run dir ~/Exports/FixIt/release-readiness-<date>
  repos: [
    { id: 'app', path: '.', github: 'henderson-tech/FixIt', production: { tag: 'v[0-9]*', exclude: '*-*' }, integration: 'main' },
  ],
  lane: {
    worktree: 'bun run worktree:create rr-{slug} --devbox',          // {slug}
    devEnv: { up: '…', hold: 'devbox hold {ws} --for 2h', park: 'devbox unhold {ws}; devbox park {ws}' },  // {ws} {slug}
    heavy: "devbox run -- '{cmd}'",                                  // must contain {cmd}
  },
  tests: [{ name: 'api-unit', framework: 'jest', cmd: 'bun run api:test:unit -- {pattern}' }],
  devices: {
    runner: 'device-runner',                    // or 'lane'
    build: 'release',                           // or 'dev-client'
    concurrent: 4,                              // keep ≤ guards.simCap
    matrix: [{ id: 'ios-phone', platform: 'ios', framework: 'appium', host: 'mac', name: 'iPhone 17 Pro', run: '…{specs}…' }],
    realtime: { specs: ['appium/specs/golden/multi/**'], roles: ['customer', 'worker'], directions: 'both', run: '…{customer}…{worker}…{spec}…' },
  },
  merge: { command: '/prm --auto --audit', order: ['eve', 'app'] },
  final: { checks: ['bun run release:preflight'], handoff: '/release:monitored' },
  rules: ['project traps rendered into lane-rules.md'],
  plumbing: ['prerequisites known to be missing; phase 3 builds them first'],
}
```

The tokens each field accepts:

| Field | Tokens |
|---|---|
| `repos[].worktree` (every repo but the first) | `{slug}` `{path}` (the repo's resolved main clone) |
| `lane.worktree` / `lane.workspace` | `{slug}` |
| `lane.devEnv.up/hold/park/checks` | `{ws}` `{slug}` |
| `lane.devEnv.url` | `{ws}` `{app}` |
| `lane.heavy` | `{cmd}` (required) `{ws}` |
| `tests[].cmd` | `{pattern}` |
| `devices.matrix[].build` | `{commit}` `{apiUrl}` `{out}` `{worktree}` |
| `devices.matrix[].run` | `{specs}` `{apiUrl}` `{app}` `{runDir}` `{udid}` `{worktree}` `{grep}` `{port}` `{driverPort}` |
| `devices.realtime.run` | `{spec}` `{apiUrl}` `{runDir}` `{worktree}` `{iosApp}` `{androidApp}` `{port}`, plus per role `{<role>}` (its platform) and `{<role>Udid}` |

The applet fills `{slug}` and `{path}` in rendered briefs. Every other token is filled by the agent that runs the command. `{port}` and `{driverPort}` are the device runner's to assign, so concurrent runs never share an Appium or WDA/UiAutomator2 port. A shell expansion (`${VAR}`) is not a token.

## Production and integration

`production.tag` is a glob. The production ref is `git describe --tags --abbrev=0 --match <tag> [--exclude <exclude>] <remote>/<integration>`: the newest matching tag reachable from the integration branch. That is correct for trunk-based repos whose deploys tag main, and whose hotfix tags are merged back. `production.branch` names a production branch instead. A tag cut on a hotfix branch that main has not merged yet is not reachable from main; the range then starts at the previous tag and includes commits production already runs, which widens the inventory but never hides anything. `init` freezes the ranges into `run.json` as `<production sha>..<integration sha>`, because every later fetch moves `origin/<integration>`. The symbolic names stay beside the shas. `range` shows the live ones.

## The run directory

`init` writes these, and never overwrites any of them:
- `run.json`: `{v, date, dir, epic, rollup, authority: {merge, devices, deviceWalk, concurrency, finish}, ranges}`. Task ids are numbers, as vitrinka returns them (quoted ids are accepted).
- `inventory-args.json`: the inventory Workflow's args. The orchestrator fills in `clusters`. Every init re-derives the rest from run.json and the adapter and keeps the clusters (`ARGS_REFRESHED` when the repos had drifted).
- The payload copies `slot`, `uniq-shots.sh` and `inventory.workflow.js`. `PAYLOAD_DIFFERS` reports a run that adapted one.
- The ledgers `results.md`, `decisions.md` and `rotation.md`.

`render` needs `run.json.authority.merge`, `authority.finish` and `epic.url`. It renders `lane-rules.md`, `device-runner.md` (with a device runner) and `final-brief.md`. With `lanes.json` and `inventory.json` it also renders one `body-<slug>.md` per lane, plus a `brief-<slug>.md` once the lane has a story and a QA task. `lanes.json` entries are `{slug, title, features: ["<i>.<j>"], journeys, unassigned: [<critic index>], stack, story, qa, board, wave}`. Slugs are unique kebab-case. A render removes the body and brief files of lanes no longer listed, and `device-runner.md` once no device runner is in scope. `--check` is the drift gate: it reports `RENDER_DRIFT` and writes nothing.

## Load-bearing rules

- **The templates are the skill's own files** (`skills/release-readiness/templates/`), read from the embedded payload. Change a rule by editing the template; the skill text and the renderer cannot disagree.
- **Payload files are copied into the run directory, never referenced by skill path**, so Claude Code and Codex installs run identical scripts, and a run can adapt a script in place.
- **`slot` and `uniq-shots.sh` are workarounds.**
  - `slot` stands in for Devbox admission that gates on cgroup-slice headroom, load and a slot count (fixit/devbox#3260; henderson-tech/devbox#88 covers the headroom).
  - `uniq-shots.sh` stands in for vitrinka keying uploaded shots by full path (fixit/vitrinka#3122).
  Delete each one when its fix ships.
