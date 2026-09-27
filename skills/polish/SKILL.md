---
name: polish
disable-model-invocation: true
description: "Fix a branch's open findings in its existing PR, then harden the feature on device, browser and server against network blips, restarts and common pitfalls with the blip chaos proxy until a clean matrix pass."
---

# /polish — same PR, adverse conditions, until clean

`$ARGUMENTS` = the findings: a pasted list, a vitrinka board slug, or a PR review. Empty → the branch PR's unresolved review comments plus unresolved annotations on the branch's vitrinka boards; none found → ask for the list once.

## Laws

- Work lands in the branch's existing worktree and PR. Never a new branch, never a second PR. No worktree for the branch → stop and say so.
- Verify each fix live before the next finding: simulator/emulator/browser for UI, a request against the running server for API.
- A matrix breakage is a finding: fixed here, same branch. Exception: a pre-existing defect outside the feature's flows → `/vitrinka:spot` it and continue.
- Three matrix passes max. Still red → hand back with the red cells listed.
- Never toggle the machine's network (Wi-Fi off, Network Link Conditioner, `/etc/hosts`). Faults go through `blip` or the process/container commands in the matrix.
- Surgical edits. New tests only where the repo already tests that behaviour class, one per fixed behaviour.
- "Recovered" = the UI leaves its error/loading state without a manual restart, the server holds exactly one effect per user action, no orphaned optimistic state.

## Protocol

1. **Bind.** `git worktree list` → the feature worktree; `gh pr view --json number,url,reviews`. Order findings by user impact.
2. **Fix findings.** One at a time: reproduce, fix, verify live, commit path-scoped.
3. **Map flows.** The feature's user flows × the mutations each performs (endpoint, DB write, external call). Flows × the tiers in `references/matrix.md` = the matrix; drop cells the feature cannot reach.
4. **Wire blip.** Client leg: `blip up api --listen :9091 --to http://localhost:<api-port>`, point the app's API-base env at it, restart the app. Server leg: `blip up db --listen :5433 --to tcp://localhost:5432`, point the server's `DATABASE_URL` at it. Missing binary → `vybava install blip`.
5. **Run the matrix.** Per cell: set the fault, drive the flow on the device, `blip <name> ok`, watch the recovery. Screenshot every failing cell. Fail → step 2 for that cell.
   Access control (matrix tier 7): `blip api record on`, drive every flow once as the primary test user, `blip api record off`, then `blip api authz --as none` and `blip api authz --as env:OTHER_TOKEN` (a second test user in another tenant/role, logged in through the app's own login; the token reaches the env via onyx `run_command`, never pasted). Every candidate the report lists is a finding.
6. **Evidence.** One vitrinka board per pass via `/vitrinka:publish`: the matrix with cell verdicts, screenshots of the fixed states.
7. **Hand off.** `blip down --all`, app left running on the device, then `/prm` on the existing PR with the board URL in its body.

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
