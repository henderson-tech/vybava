# Orchestrator messages

Fill the `<…>` fields and send the text with `SendMessage` to each named lane agent it concerns; broadcasts go to every live lane. Log each one in `rotation.md`.

## Box-wide rule (limiter)

```text
Box-wide rule from the orchestrator, effective now. <symptom: load, memory, OOM-killed DBs, simultaneous restarts — with times>.
1. EVERY heavy job goes through <limiter command>: browser batches, builds, suites. It allows at most <N> heavy jobs box-wide and waits while <gates>. Its exit code is your command's. Keep jobs short; the other lanes queue behind you.
2. Never run setup/up/gen on a stack that is already up; after a config change recreate only your own service.
3. A run is invalid on exit 137, an app/DB restart during it, a launch error, or a skip on an unreachable-dependency guard. Re-run it through the limiter.
4. If your lane does not need its dev server right now, stopping YOUR OWN frees memory. Restart it before you finish.
```

## Root cause and park

```text
Root cause found (thanks to lane <slug>): <the real ceiling and its evidence>.
1. The limiter now also waits for <new gate>.
2. If your lane will NOT run a heavy job in the next ~15 min (waiting on CI, an audit or a review, writing code, publishing), park your stack now: <devEnv.park>. When you need it again: <devEnv.up> once, then queue through the limiter.
3. Stacks actively running jobs stay up. Park as soon as your last run is validated and published.
```

## GO

```text
GO lane <slug> (wave <X>). <What freed the room>. Bring your stack up once (<devEnv.up>), verify it per lane-rules, then continue where you stopped. Queue heavy jobs through <limiter>.
```

## Board-integrity alert

```text
Board-integrity alert, found by lane <slug>. <what is wrong on the boards, e.g. shots mapped by basename, wrong device widths>.
CHECK your board: get_card_image on one card per device. <expected widths>; the content must match the case.
FIX (no re-run needed): <the fix steps, e.g. uniq-shots.sh → qa run --dry-run manifest → same runId → qa run publish --task <your qa>>.
Publish all future runs the same way, and verify with get_card_image before you call the board done.
```

## Shared fix

```text
SHARED FIX from the orchestrator: <symptom every affected lane sees, e.g. the app hangs on the "<splash>" screen>. Cause: <root cause, with the bug id>. It is NOT your lane.
Fix: commit <sha> on branch <branch> (fixer <agent name>, PR <url>), landing standalone. Cherry-pick exactly that commit into your checkout (never re-author it), then restart only your own affected service. Once the PR merges, merge the integration branch into your branch so the cherry-pick drops out of your diff.
```

## Retraction

```text
RETRACTION of my <HH:MMZ> message about <topic>: it was wrong because <evidence>. Ignore <the instruction>; <what to do instead, or "nothing changes">.
```

## Resume after a usage limit or restart

```text
RESUME from the orchestrator (<HH:MMZ>). <What stopped the agents and when>. <State of the box and the branches>. Continue exactly where you stopped: first re-read your checkouts' git status and log, your lane task (its hand_back state and comments), your open PRs (gh pr view) and any run logs, and don't redo finished work. Rules unchanged: <the 3–5 rules most likely to have drifted, e.g. limiter, park/hold, one commit, own QA task, PID-only kills>. Anything that ran around <window> is suspect. Finish with hand_back and your lane JSON.
```
