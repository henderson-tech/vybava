---
name: journey-campaign
description: Run or resume sustained, multi-actor user-journey testing on real devices, preserving evidence, runtime ownership and coverage across sessions. Use for a live testing campaign or its handoff, rather than a one-off unit test or a release/merge workflow.
---

# Journey campaign

Drive the product as its users, and keep a trustworthy account of what actually
worked. Use the repository's existing drivers, fixtures and reporting tools.
This skill grants no new access and does not imply approval to commit, publish,
charge, deploy or reset data; inherit the user's actual authorization.

## Resume before starting anything

Read the latest short checkpoint, the repository's agent entry, and the named
journey/coverage contract. Use the existing worktrees. Confirm the current git
state and the particular service/session handles before changing anything.
An old log, PID file or prior successful request is a locator, not proof of life.

Recover these bindings: actor -> account -> role/company -> device/session;
repository/branch -> backend namespace -> API origin -> native build. Use IDs
from owned receipts, not a familiar-looking fixture name or an old browser tab.
Never print credentials or capability URLs while recovering them.

**Do not restart on an observation timeout.** Poll the same handle or inspect
the owning process/server log. Restart only after terminal/absent state, or a
diagnosed fault requiring a scoped restart. Reuse a persistent Appium session
per device. Never let two drivers operate the same handset concurrently.

For physical-device failures read [references/device-field-guide.md](references/device-field-guide.md).

## Preserve the real task

Keep the original journeys, selected cells, actor pairs and required checks.
An initial selected subset is not the full candidate universe. A preflight,
fixture setup, API-assisted shortcut, scripted AI reply or supplemental booking
does not finish the original user lifecycle. Describe such seams explicitly.

Maintain a small coverage ledger: requirement, current outcome, evidence,
remaining dependency. Preserve failed attempts and record reruns separately.
New code/build/config invalidates current-source claims where relevant; seal a
fresh plan when the engine requires it, without rewriting historical evidence.

When the project uses Výbava `journeys`, read its current `--help` and the
project adapter contract. `start` is preflight, `probe` is a read, `verdict` is a
separate evidenced judgment. The engine's acceptance of a record does not prove
that the UI was observed. Use fresh output paths for immutable receipts.

## Execute and verify one consequential transition

1. Identify the actual user intent and relevant prerequisites. Seed prerequisites
   through supported, owned paths; let the customer create the job through UI.
2. Observe the current screen, then operate it. Prefer stable test IDs; use
   coordinates only from a current view when native controls hide IDs.
3. Verify both the visible outcome and independent persistent state. A toast
   alone is insufficient. Reopen after save; inspect the exact owned entity and
   actor. For multi-role behavior, verify the other role's view as its own case.
4. Record the boundary proved. Payment success, fee reconciliation, supplier
   invoice, worker earning and bank payout are distinct. Likewise polling
   recovery is not webhook delivery, and foreground state is not OS push delivery.
5. Preserve evidence before fixing. Fix actual incorrectness, add a focused
   regression that fails without the fix, then rebuild/retest the affected native
   path. Unit tests do not replace the device result. Avoid repeating already
   proven branches unless a relevant change invalidated them.

Use one bounded mutation followed by reconciliation when a response is lost.
Do not replay payment, signature, acceptance or seed operations blindly. A
double-tap test needs an initial unprocessed transition, not a paid record replay.

## Handle blockers without manufacturing success

Separate application defects, test-harness faults and external prerequisites.
Fix what the authorized scope permits; continue independent unfinished work.
Do not weaken a product gate to unblock a test. Do not fabricate moderation,
identity/compliance, provider readiness or missing credentials. Record declared
development fixtures without promoting them into external-service coverage.

A healthy service does not prove its model, broker, callbacks or prompts work.
Trace the actual request/job. Stop repeating an unchanged failing credential or
vendor challenge; name the specific changed input needed for another attempt.
If a human is handling a hosted verification step, leave that device alone.

## Evidence and publication

Raw screenshots, device dumps, tokens and provider records stay in private
evidence storage; backups go to the backup home. Inspect originals AND converted
images before sharing. GPS, names, contact data, bank details and hosted URLs can
leak even in a test run. File names and hashes do not redact pixels.

Use the project's existing reporter. If using Vitrinka, follow its current
usertest/run contract: do not hand-write manifests or duplicate its bug creation.
Publish only reviewed evidence, preserve failure history, and return its exact
server handback rather than guessing task URLs.

## Pause or transfer

Pause an active goal only when requested. Stop new test actions, settle or record
any in-flight command, and preserve the current screen. Do not tear down a
hand-testable stack unless requested or required by its ownership policy.

Write a short, authoritative checkpoint using
[references/checkpoint.md](references/checkpoint.md). Put changing IDs, process
handles, pending cases and last observations there, never into this skill.
Keep older logs as evidence but give the next session one obvious entry point.
Say exactly which operation should happen next and which tempting actions would
repeat work or destroy state. A handoff is not a passing test or a completed goal.
