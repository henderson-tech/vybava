---
name: polish
disable-model-invocation: true
description: "Polish a branch in its existing PR: fix its open findings, then drive the target's matrix - app (iOS 26, iOS 18 and Android chrome, lifecycle, network), ui (browser rows and the ui-loop capture pass), api (blip chaos, access-control replay, DB) - at quick, default or full intensity until a clean pass. Target and intensity are inferred from the diff, the findings and the cwd unless named."
---

# /polish - same PR, the target's matrix, until clean

`$ARGUMENTS` = `[app|ui|api|all] [quick|default|full] [findings]`, every word
optional. Findings: a pasted list, a vitrinka board slug, a PR URL or review.
Empty → the branch PR's unresolved review comments plus unresolved annotations
on the branch's vitrinka boards; none found → ask for the list once.

Two axes, both inferred by `polish-kit plan` unless named:

| Axis | Values | Meaning |
|---|---|---|
| target | `app` native apps · `ui` web · `api` server · `all` | which matrix runs; a diff spanning two targets runs both, heaviest first |
| intensity | `quick` · `default` · `full` | `quick` fixes and verifies live, then hands off · `default` adds the target's matrix · `full` adds the target's deep pass |

The per-target lanes, tiers and deep passes: `references/targets.md`.

## Laws

- Work lands in the branch's existing worktree and PR. Never a new branch, never a second PR. No worktree for the branch → stop and say so.
- State the plan's one-line verdict (targets, intensity, lanes) before step 1. An explicit word overrides the inference, never silently.
- Verify each fix live before the next finding: the lane that found it for UI, a request against the running server for API.
- A matrix breakage is a finding: fixed here, same branch. Exception: a pre-existing defect outside the feature's flows → `/vitrinka:spot` it and continue.
- A chrome finding is a pattern (`app-chrome.md` §7): every sibling is fixed in the same PR, a guard or manifest row keeps it closed, and the lane that found it is re-shot after a full dev-client reload.
- Simulator evidence is not handset proof for a lane the repo declares as a device; say which it was.
- Judgment stays with you: `polish-kit` shoots, ledgers, renders sheets and reports; it never rates a cell.
- Three matrix passes max. Still red → hand back with the red cells listed (`polish-kit status`).
- Never toggle the machine's network (Wi-Fi off, Network Link Conditioner, `/etc/hosts`). Faults go through `blip` or the process/container commands in the matrix.
- Surgical edits. New tests only where the repo already tests that behaviour class, one per fixed behaviour.
- "Recovered" = the UI leaves its error/loading state without a manual restart, the server holds exactly one effect per user action, no orphaned optimistic state.

## Protocol

0. **Plan.** `polish-kit plan --json` (`--target`, `--intensity`, `--findings` when named). Missing binary → `vybava install polish-kit`. No `polish` section → the diagnostic prints the block to add to the repo's `vybava.config.ts` (`references/targets.md` has a worked example); add it on this branch and rerun.
1. **Bind.** `git worktree list` → the feature worktree; `gh pr view --json number,url,reviews`. Order findings by user impact.
2. **Fix findings.** One at a time: reproduce, fix, verify live, commit path-scoped. `quick` ends here → step 7.
3. **Map flows.** The feature's user flows × the mutations each performs (endpoint, DB write, external call). Flows × the target's tiers in `references/matrix.md` = the matrix; drop cells the feature cannot reach. `polish-kit run init --pass <n>` writes the ledger; `polish-kit run add-cell` records each matrix cell.
4. **Lanes.** `polish-kit lanes --json` resolves every declared lane to a live device or host; a diagnostic's `fix` is the exact create or boot command. Boot only the lanes the plan names.
5. **Run the matrix.** Per cell: set the fault or device state (`blip …`, `polish-kit lanes set <lane> --theme dark --nav 3button`), drive the flow, watch the recovery, `polish-kit cell <id> pass|fail --shot <png>`. Screenshot every failing cell. Fail → step 2 for that cell. `full` adds the target's deep pass from `references/targets.md` (app: `references/app-chrome.md` over `polish-kit shoot`; ui: `ui-loop run`; api: `references/api-deep.md`).
   Access control (matrix tier 7): `blip api record on`, drive every flow once as the primary test user, `blip api record off`, then `blip api authz --as none` and `blip api authz --as env:OTHER_TOKEN` (a second test user in another tenant/role, logged in through the app's own login; the token reaches the env via onyx `run_command`, never pasted). Every candidate the report lists is a finding.
6. **Evidence.** `polish-kit sheet` (contact and edge sheets per screen), `polish-kit report` (the pass table with a delta against the previous pass). One vitrinka board per pass via `/vitrinka:publish`: the report, the sheets, screenshots of the fixed states.
7. **Hand off.** `blip down --all`, `polish-kit lanes set <lane> --reset` for every lane touched, app left running on the device, then `/prm` on the existing PR with the board URL and the report in its body.

## blip

```
blip up <name> --listen <addr> --to http://host:port | tcp://host:port
blip <name> set delay 800ms [--jitter 400ms]
blip <name> set drop [--rate 0.3]            # reset the connection
blip <name> set error 503 [--rate 0.5] [--body '{"error":"x"}']   # http only
blip <name> set timeout                      # accept, never answer
blip <name> set slow 20kbps
blip <name> set flap 5s/10s                  # down 5s, up 10s, repeat
   scoping on any set: --match '/api/orders*' --method POST --after 3 --for 30s
blip <name> cut                              # kill in-flight connections once
blip <name> ok                               # heal
blip <name> log --tail | status
blip <name> record on|off|clear              # http only: capture requests for replay
blip <name> authz --as none|'header:K=V'|'cookie:k=v'|env:VAR [--mutations] [--only <glob>]
                                             # replay as another identity; lists what still succeeds
blip ls | down <name> | down --all
```

Every verb takes `--json`; the envelope's `next` names the follow-up command. `authz` replays reads only unless `--mutations`; exit 2 with candidates, 0 clean.

## polish-kit

```
polish-kit plan   [--base <ref>] [--target app,ui] [--intensity quick|default|full] [--findings <ref>]
polish-kit lanes  [--target app] [--lane <id>]
polish-kit lanes set <lane> [--theme light|dark] [--nav gesture|3button] [--text <size>] [--reset]
polish-kit run init --pass <n> [--target app] [--lanes a,b] [--screens x,y]
polish-kit run add-cell --kind matrix --lane <id> --flow "<title>" --tier "<row>"
polish-kit cell <id> pass|fail|skip [--shot <png>] [--note <text>] [--finding <ref>]
polish-kit status [--pass <n>]
polish-kit shoot <lane> [--pass <n>] [--screens x,y] [--themes light,dark] [--nav …] [--text …]
polish-kit sheet  [--pass <n>] [--screen <id>] [--lanes …]
polish-kit report [--pass <n>] [--previous <n>]
```

Same envelope and exit codes as `blip`; `next` is the protocol. Contract: Výbava `docs/polish-kit.md`.
