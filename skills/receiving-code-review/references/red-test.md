# RED test

## Classifier

1. **Hard skip-list:** `vybava gitkit tdd-classify <file>` non-null
   (`migration|deps|ci|iac|generated|docs`) → direct-fix. Also pure naming, style
   and comment changes.
2. **Gate 1 — behavioral defect?** No wrong observable result for some input →
   direct-fix.
3. **Gate 2 — reproducible without infra?** A unit or integration test can reach the
   seam → RED first. Only a running stack reaches it → e2e in the repo's runner
   (Playwright for web, Appium for Expo/RN) when one exists; else direct-fix with
   the reason.

The record states which: `+ regression test <path>` or `(no test: <skip-category |
gate-1 | gate-2>)`.

## Seam ladder

Smallest seam that asserts the claimed symptom — unit → integration → e2e. The test
goes where the neighbouring tests of the touched file live, in that runner, sized
like them. No runner reaches the seam → the `diagnosing-bugs` skill's Phase 1 ladder
(curl script, CLI fixture diff, replayed trace, throwaway harness) builds the red
loop; it is the proof, not a committed test, and the record says so.

The RED run must fail on the finding's symptom: same wrong value, same error type and
message substring, same timing class. Different failure → different bug; iterate the
test, never the verdict. Green against untouched code → the finding is STALE or
INVALID, the test as evidence; never adjust the test until it fails.

## Blast-radius gate

Before commit. Observable contracts: API shape, status or error format, return
semantics, event payload, shared type, config default.

1. Internal-only (same signatures, same outputs) → done.
2. Contract-changing → sweep consumers repo-wide and known sibling services; update
   in-repo consumers in the SAME commit. An unfixable consumer (other repo, deployed
   client) → never push the silent break; narrow the fix or DEFER, naming it.
3. Pin the contract with a test asserting the externally observed behavior —
   mandatory when the surface had no test, even on a direct-fix.
4. Record: `+ contract test (N callers updated)`.
