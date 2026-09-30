# Physical-device field guide

These are diagnosed traps from sustained mobile campaigns, not universal claims
about every device or app. Recheck version-sensitive details against local code.

## Device and process control

- Appium observation timeouts can leave WDA completing the same request. Read
  its log or re-poll the same session; opening another session can relaunch WDA,
  consume gigabytes and destroy the observation you needed.
- An unsupported diagnostic route returning 404 is not a dead driver. Use
  documented driver commands; distinguish an unknown command from invalid session.
- iOS system alerts may belong to SpringBoard rather than the app's tree.
  Deep-link calls can wait behind an Open confirmation. Use the repository's
  system-alert helper instead of restarting blindly.
- `visible=false` may describe an on-screen native sheet row/header. Inspect
  geometry and the screenshot; expand a sheet so the target clears its footer.
  Tap current coordinates only after observing them. Do not blindly reuse old ones.
- A native keyboard hide is not always a field commit. When a button stays
  disabled, distinguish blur/Done from invalid data before diagnosing a bug.
- In a hosted webview, select the correct window handle and verify account/context
  before interpreting a return page. A prior account's success tab is not evidence.
- Screenshot and source can disagree because rendering froze or a native snapshot
  is stale. Diagnose first. A screenshot capture alone is not a UI interaction.
- Killing a launcher can leave its Node child serving the old bundle. Verify the
  listener and its cwd before stopping the particular owned process.

## Native build fidelity

- Know whether the phone runs a Metro dev client or an embedded Release bundle.
  Source edits/HMR do not update an embedded bundle. OTA can replace it again;
  record whether updates are enabled and verify the installed artifact identity.
- Keep test builds on the same development bundle/package ID and install over
  existing data. Do not silently uninstall to resolve a signing/version conflict.
- Build from the worktree, with its API origin and required test configuration.
  A shell export is not guaranteed to reach every native build phase. Pass the
  composed environment explicitly and verify required values without printing them.
- On constrained Macs, serialize native builds and limit worker/heap concurrency.
  Observe compiler progress instead of restarting a slow live build. Use a fresh
  dedicated temporary directory when an existing one has a diagnosed stale cache.
- Record executable/bundle hash, version, API origin and update mode. A manifest's
  default appVersion field is not proof of the installed version.
- Device tooling is OS-sensitive: an iOS 17 tunnel/file command may not support
  an iOS 16 handset. Use the previously verified compatible tool, not a fresh WDA
  compile or an OS upgrade just to get a screenshot.

## Save, retry and money observations

- Separate process restart from explicit Keep/Save. An ephemeral store resetting
  on restart alone does not prove a promised server save failed. Reproduce the
  actual Keep -> reopen sequence and inspect persisted fields.
- Native unmount cleanup can reset state before a dialog callback runs. Capture
  inputs before reset, wait for acknowledged writes, retain retry data, and guard
  actor/flow changes. A retry dialog whose every action is disabled by a stale
  context is itself a trap.
- Restore provenance as well as values: manual choices must remain manual so a
  late AI response cannot replace them. Untouched automatic defaults must not be
  promoted into user decisions on save.
- Draft persistence must not publish a listing, create a live auction or bypass
  readiness. Read the server's status/ownership contract before reusing an endpoint.
- Reconcile exactly-once claims with independent IDs and counts. Check original
  amount/currency, charge/intent, invoice, earning and events; avoid counting a
  refunded, pending or previous transaction as the current success.
- Delayed payment-provider fees can be normal. Inspect the documented recovery
  window before declaring a bug, and identify a manual sync as API-assisted.
- Native PDF rendering, a share chooser, print preview and cloud upload are
  different actions. Prove readable content; do not submit print/share merely to
  dismiss the chooser. Use supported platform intent helpers.
