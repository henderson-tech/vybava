# Lessons (the expensive ones)

These are distilled from the Reservine devlp run (2026-09-22/23: 12 lanes, four-device boards, merged-branch roll-up). Each backs a law or a rule.

- **`SendMessage` to a Workflow agent id spawns a DUPLICATE** beside the original, which keeps running. Lanes that may need corrections are named Agent-tool agents. (Law 1)
- **Verify the dev topology on ONE lane before fanning out.** Reservine lost hours to three topology faults that every lane hit at once:
  - a sibling repo synced its main clone instead of the lane's worktree;
  - a frontend proxy resolved to a docker-bridge IP;
  - the sync ignored a build directory the backend served.
  (Law 3)
- **Find the real resource ceiling.** It was a 40 GB container cgroup slice, not host RAM, plus `pids.max` 8192 exhausted by idle ClickHouse threads. The symptoms looked like app bugs: thrash, runc `setns` failures, DB SIGKILLs, false-green exit 0. (`capacity.md`)
- **Validity rules are what make a green board mean something.** Exit 137, a container restart mid-run, a browser launch error, or a skip on a DB-unreachable guard each invalidated runs that had "passed". (Law 4)
- **FIFO admission queues head-of-line block.** One strict-FIFO run queue sat behind standard-profile runs while small jobs could have gone. (`capacity.md`)
- **Only a merged-branch roll-up catches cross-lane test-data conflicts**: shared payment accounts, seeded tenants, one suite deleting an account another needs. (`final-phase.md`)
- **Repeated pre-merge audits catch real bugs.** One PR took 6 audit rounds to close a cross-tenant SSR leak through 4 paths. Keep `--audit` on for every lane: the surfaces that leak are rarely the ones you would have guessed.
- **Usage limits hit twice in one night.** Resume protocol: wait for the reset, resume transcript agents by message, and re-spawn dead lanes from a state snapshot plus the shared rules file. One rules file is what makes a re-spawn cheap. (Law 2, `messages.md`)
- **A publish to a story without a QA child lands on the epic's shared board.** Create every lane's QA task before any lane publishes. (Law 4)
- **Board shots can lie.** Playwright names every attachment `test-finished-1.png` and vitrinka maps shots by basename, so cards showed the wrong test and reported desktop widths for phones. Always check `get_card_image` per device. (`uniq-shots.sh`; tracked as fixit/vitrinka#3122)
- **The orchestrator relays.** Helpers and auditors often cannot message the lane owner. Findings die unless the orchestrator forwards them, and a wrong broadcast costs every lane until it is retracted.
- **The final phase needs a fresh context.** The orchestrator was past 70% by the time the lanes merged; the roll-up, re-runs and board went to a new agent with a rendered brief.
