# Shared: output convention — full, clickable links

When a git: skill prints ANYTHING about a PR, branch, comment, commit, or CI run,
show the FULL URL so the user can click it directly. Terminals linkify raw full URLs;
masked markdown (`[#202](…)`) and bare refs (`#202`) are not one-click targets.
Optimize for "clearly visible + one click".

## Rules

- **Full URLs, not shortened.** Print the whole URL:
  - PR:       `https://github.com/<owner>/<repo>/pull/<N>`
  - comment:  the finding's `url` (e.g. `…/pull/<N>#discussion_r<id>`)
  - commit:   `https://github.com/<owner>/<repo>/commit/<sha>`
  - CI run:   `https://github.com/<owner>/<repo>/actions/runs/<id>`
  - branch:   `https://github.com/<owner>/<repo>/tree/<branch>`
- **Summary header** carries the canonical PR URL on its OWN line, up top.
- **Per-item lines** carry that item's own deep link, so each is independently clickable.
- **De-dup, don't spam.** Never repeat the identical URL on consecutive lines. When a
  bare `#202` sits right next to a full link just shown for the same target, don't
  re-link it. One visible full link per distinct target per block.
- Prefer the raw URL over masked link text so the destination is visible at a glance.

All URL parts come from the resolve-fetch envelope (`owner`, `repo`, `pr`, `headSha`,
`headRef`, and each finding's `url`) — no extra API calls needed to build them.

## Resolved vs answered

Say which closure a finding actually got — they are not interchangeable, and a
reader deciding whether to merge needs the difference:

- **resolved** — an inline / pathless review thread, closed via `resolve-thread`.
  GitHub records it; the readiness gate can see it.
- **answered** — a review summary or PR conversation comment. There is no thread
  to resolve, so closure is a quoting PR comment (+ a reaction). **GitHub records
  nothing**, and `merge-precheck` is blind to it — so if the summary does not
  mention it, nobody knows it happened.

Mark the unresolvable ones inline (`(answered — no thread)`) and, when any exist,
carry a count in the round header. Never write "all comments resolved" when some
were answered instead.

## Summary shape (example)

```
PR #202 — Fix offer race
https://github.com/leftEq/fixit/pull/202
Round 2 · 1 new thread + 1 conversation ask · pushed 3f9a1c2

✓ VALID  @alice  apps/api/src/offer.ts:88  (resolved)
  https://github.com/leftEq/fixit/pull/202#discussion_r1234567
  fixed + test → https://github.com/leftEq/fixit/commit/3f9a1c2

✓ VALID  @bob  (PR conversation)  (answered — no thread)
  https://github.com/leftEq/fixit/pull/202#issuecomment-2233445
  covered by the same fix; quoted + 👍 in the conversation

CI: https://github.com/leftEq/fixit/actions/runs/99887766  (queued)
```
