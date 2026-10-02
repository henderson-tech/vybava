---
name: codex-sidekick
description: "Claude Code only. Use when manual or verification work should run on a Codex sidekick thread instead of in this session: user test / usertest, verify it in the UI, browser or simulator, computer use, explore the app, map routes or journeys, write, rework or run e2e or other tests. Also use when the user says 'codex high', 'codex xhigh' or 'sidekick', or asks to hand work to the Codex sidekick. Claude stays the orchestrator and the only author of app source; the sidekick is the native `codex` subagent inside `cc` sessions, else `switcheroo codex run`."
---

# codex-sidekick: hand the manual work to a Codex thread

This session orchestrates and writes app code. Driving, looking, mapping and test
writing go to a Codex thread on gpt-6.1-sol through `switcheroo codex run`. The
CLI prepends the sidekick contract to every run. That contract says the
sidekick may touch test files only, never runs git, reports to
`findings.md` and returns one structured result. Claude reads that result.
Flags, exit codes and report paths: `switcheroo codex run --help`. Account failover
and run state: claude-switcheroo's `docs/specs/2026-10-01-codex-sidekick-run-decisions.md`.

**If you are Codex, this skill is not for you.** You are the sidekick: do the brief
yourself and never call `switcheroo codex run` from inside a run.

## Inside a `cc` session: the `codex` subagent first

A `cc` session with a Codex account registered carries two native subagents:
`codex` (medium) and `codex-high` (for "codex high"). Only their model turns bill a
Codex account, and those fail over like `cx`. Otherwise they are ordinary subagents,
with a row in the subagent view, background runs and `SendMessage`. They exist when
the Agent tool lists them. Rules: claude-switcheroo's
`docs/specs/2026-10-02-codex-subagent-decisions.md`.

| Lane | Door |
|---|---|
| `verify`, `computer-use`, `explore`, `rework` | the `codex` subagent |
| `usertest`, `e2e-write`, `e2e-run` (they lead a skill), "codex xhigh", or no `codex` agent listed | `switcheroo codex run`, below |

- Spawn it with the Agent tool: `subagent_type: "codex"` (`"codex-high"` for "codex
  high"), the brief from the templates below, in the background. Keep one agent per
  lane and continue it with `SendMessage`, which replaces `--resume`. Never spawn a
  fresh agent for each step.
- Its final message opens with `STATUS: done|blocked|failed`. After that come the
  summary, findings, files written, tests and the blocker. Act on it as the exit-code
  table says for 0, 10 and 1. There is no `findings.md` and no Exports run dir, so
  skip commit duty 3.
- A brief for parallel lanes names each lane's browser
  (`browser_start(session: "$CLAUDE_CODE_SESSION_ID-<lane>")`). Without that line,
  the agent takes the session's own browser.
- An agent that ends with `No Codex account …` means exit 3: fall back as below.
- An `explore` brief drops the `sol_explorer` fan-out. The subagent maps the clusters
  itself, one after another.

## Route

| Work | Goes to |
|---|---|
| user test / usertest, "QA this like a user" | sidekick, `usertest` brief |
| verify a change in the running web app or Expo app (browser, simulator) | sidekick, `verify` |
| computer use: native or desktop apps | sidekick, `computer-use` |
| explore the app, map routes, journeys or testID gaps, e2e discovery, other bulk read-only mapping | sidekick, `explore` |
| write or rework e2e, unit or integration tests | sidekick, `e2e-write` / `rework` |
| run e2e or other suites | sidekick, `e2e-run` |
| app source edits, fixes for findings, design, a one-grep code lookup mid-task | Claude, here (never routed) |

Effort is `medium`. Use `--effort high` only when the user says "codex high", and
`--effort xhigh` only for "codex xhigh".

## Run, then resume. One thread per lane

```bash
switcheroo codex run --cwd <ABS worktree> --slug <lane> --json - <<'EOF'
<brief>
EOF
```

- Use the Bash tool with `run_in_background: true`. The completion notification
  carries the envelope `{run, thread, exports, status, result, error, resume, …}`. Keep
  orchestrating in the meantime and don't poll or sleep.
- Pass `--cwd` as the literal absolute path of the worktree the work belongs to.
  The persistent shell's cwd drifts, so never rely on it.
- Start with `result`: `status` (`done`|`blocked`|`failed`), `summary`,
  `findings[{severity: blocker|major|minor|info, title, detail, evidence, files[]}]`,
  `files_written[]`, `tests{written[], ran[], passed, failed}` and
  `blocker{reason, evidence, fix_hint, files[]}` or `null`. Open
  `<exports>/findings.md` (one `## Turn n` per turn) only when the summary falls short.
- Every further step of the same lane resumes the same thread with
  `switcheroo codex run --resume <run> --json - <<'EOF' … EOF` (the envelope's
  `resume` field). Never start a fresh run per feature or screen: the thread keeps
  the selectors, seeds and conventions it has already found. A new lane gets a new run.
- Run at most 4 live lanes. The mobile (Appium) lane is always exactly one, because there is one simulator.
- Don't edit app source while a `verify`, `usertest` or `e2e-run` lane is driving
  that worktree, because its verdicts become nondeterministic. Edit between turns,
  then resume. `e2e-write` and `rework` are static under `--no-run` (no app, no
  browser), so they may run alongside your edits: the tests follow the code.
- Never put secret values in a brief. Name the seed account or the onyx ref.

## Exit codes

| Exit | Status | Claude does |
|---|---|---|
| 0 | `done` | Fix the findings that need app source here, then do the commit duties. |
| 10 | `blocked` | `result.blocker` gives the reason, evidence, `fix_hint` and files. Fix it here (app source, seed, env), commit, then `--resume <run>` with "Fixed: <what> (<sha>). Continue." A human-only gate (sign-in, 2FA, payment): `/codrive` or ask the user, then resume. |
| 1 | `failed` or codex error | Read `error` and findings.md. Resume once with a corrected brief, or re-run if `thread` is null. If it fails a second time, report it to the user with the error. A failed run is not a reason to fall back. |
| 2 | usage error | Fix the invocation (`--help`). |
| 3 | no Codex account left | Fall back. Do the same when `switcheroo codex run --help` itself fails (the CLI is not installed). |

**Fallback.** Use one `general-purpose` Agent with `model: "opus"` per lane. Give it
the same brief plus the contract essentials: test files only, no git, the same
result shape. Strip every Codex-only step, because a subagent has no `sol_explorer`
or `sol_tester`: an `explore` lane maps its clusters itself, one after another. Keep
the agent persistent through SendMessage for the lane and shut it down when the lane
ends. A lane briefed to lead the `e2e` skill (`e2e-write`, a full `e2e-run`) is the
exception: don't forward its Codex-lead brief. Follow the `e2e` skill's exit-3 path
instead, where Claude leads Phases 1-2 and spawns one named Opus writer per lane.
Tell the user in one line:
"Codex unavailable: <lane> ran on an Opus subagent."

## Browser and device etiquette

- By default the run shares this session's onyx browser (`$CLAUDE_CODE_SESSION_ID`).
  While a run is live, Claude makes no playwright, chrome-devtools or onyx
  browser calls. Two drivers on one page corrupt each other.
- Parallel live lanes each pass `--browser $CLAUDE_CODE_SESSION_ID-<lane>`. A Codex
  run binds one browser for its whole life, so the `e2e` lead's web lane is serial
  inside a run. Static work (`explore`, `e2e-write`, `rework`) passes `--browser none`.
- Never stop or restart a browser a lane is using. Claude also leaves the simulator
  or the computer-use app alone while a lane is driving it.

## Brief templates

The brief carries only the job. Every brief states: **Goal** · **Scope**
(routes, files, `base..HEAD`, task id) · **App access** (URL or device, login, seed
roles) · **Done when** · the mode lines below.

- **usertest**: "Use the vitrinka `usertest` skill, lane 2: `vitrinka qa usertest
  start [--task <id>] [--app web] [--platform …] [--device …]`, then `case` /
  `board capture` / `verdict` per journey, then `finish --bugs intake`. (Lane 1
  instead when the target is a suite: `vitrinka qa run -- <test command>`.) Build
  the role matrix from the app's seeds first, and treat edge cases as the job. Fix
  nothing: a code bug is a `fail` case with its note plus a finding, and a bug that
  blocks the remaining cases means status blocked. Put the hand-back block `finish`
  printed into `summary`, links verbatim."
- **verify**: "Change: <what, files, sha>. In <URL at a 1440×810 viewport | Expo on
  <device> via the dev client>, check: 1. … 2. …. Dual-verify every mutation: the UI
  changed AND the backend accepted it (network 2xx, log or state endpoint). Make one
  finding per expectation that fails, with screenshot evidence."
- **computer-use**: "App: <name or window>. Do: <steps>. Expect: <outcomes>. Work
  in the background and never take the human's foreground. Any sign-in, 2FA,
  payment or consent gate means status blocked."
- **explore**: "Static and read-only: no browser, no device. Map <scope>: routes,
  actions with their testID or role, intents, 5–15-step journeys per persona,
  testID gaps, and conflicts (code vs copy, DTO or DB). Fan out `sol_explorer` per
  feature cluster. [e2e discovery: write `<app>/journeys.md` per the `e2e` skill's
  Phase 1.]"
- **e2e-write** (no run): "Run the `e2e` skill as its Codex lead with `--no-run` on
  <scope>: `sol_explorer` discovery, then write or update the specs and `journeys.md`
  statically from source. No app, no browser or device, and never execute the
  runner. Lint and typecheck only."
- **rework** (no run): "<specs> broke because <app change, sha>. Update them to the
  new behavior (selectors, flows, fixtures). Never weaken an assertion to make it
  pass; behavior you believe is a bug is a finding. Lint and typecheck only."
- **e2e-run**: "Run the `e2e` skill as its Codex lead on <scope> (drive live,
  dual-verify, run each spec alone), or run <specs | --grep @e2e-<slug> | the suite>
  with the repo's runner, where the repo runs tests (Devbox when it has `devbox.yaml`), workers capped.
  Classify each failure. A spec bug: fix the spec and rerun it once. An app bug:
  record a finding with evidence and don't fix it. Report the counts in `tests`."

**When suites run.** While code is still changing, test lanes use `--no-run`. Do one
full `e2e-run` at the end of the work, or right after a risky change.

## Commit duties (Claude, when the lane ends)

The sidekick never commits.

1. **Check the diff.** `result.files_written` and `result.tests.written` must be
   test files only, and `git status --short` in the worktree must show no app-source
   change you didn't make. Anything else breaches the contract: review it as a
   stranger's change, keep or revert that path, and tell the user.
2. **Commit the test files, path-scoped.** Follow the `push-all` §2 conventions:
   `git -C <wt> add -- <files>`, then commit, e.g. `test(e2e): <lane>`.
3. **Commit the Exports run dir on main, path-scoped.** Other sessions write to
   that repo too, and a credential must never be committed (check the findings
   first): `git -C ~/Exports add -- <exports>` then
   `git -C ~/Exports commit -m "docs(<project>): codex <slug>" -- <exports>`.
4. **Report.** Fix or file what the findings say, then report the result with the
   exports path.
