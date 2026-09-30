# Checkpoint contract

Keep the front page short enough that the next session can act without reading
the entire historical log. Link large evidence and conditional recipes.

Record:

- Timestamp, user instruction (paused/continue), full objective, explicit scope,
  outstanding requirements and limits on what passed.
- Exact worktrees/branches; dirty work ownership; commit/PR state. No secrets.
- Current devices, bundle/package/version/hash, actors, Appium session IDs,
  driver ports, UI location, and last completed interaction. State whether its
  inputs are only in memory. Distinguish observed state from a proposed next step.
- Running service ports, owned PIDs/cwds/logs and last health observation. Tool
  exec handles may not survive a new session: include independent process locators.
- Open report run/case/board; already published failure and bug IDs; evidence
  directory; any publication pending. Do not mark an interrupted case passed.
- Source fix state, focused tests, generated contracts, migration state and
  verified backup location. Distinguish compiled/installed from device-retested.
- External blockers, last result/time, and precisely what input must change to
  justify retry. Mark human-owned device work.
- One concrete next action, with later steps in dependency order; operations
  that must not be repeated; where compatible tool/build recipes live.

Do not put tokens, passwords, full hosted capability URLs, or personal location
in the checkpoint. Keep private references separate from publishable summaries.

At resume: verify the named handles and source state, then continue the pending
step. If a handle is gone, diagnose before rebuilding. Do not re-seed or restart
the campaign simply because it is a new conversation.
