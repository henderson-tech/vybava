# Claude Code mods — decisions

Epic: https://app.vitrinka.ai/w/fixit/p/vybava/t/4759

## Summary

Claude Code 2.1.287 runs mods: plugins of in-process function hooks with
session state, UI (spinner, band above the prompt, status lines, panes,
toasts) and the power to rewrite, register tools and start turns. A week of
transcripts (2026-09-25 → 10-02: 603 main + 5,081 sub/workflow transcripts,
~745k tool calls) shows the pain is visibility and recovery, not disobedience:
119 killed background tasks and 70 bare "continue" prompts, 59 "is it stuck"
prompts, a median of 17 and a peak of 67 concurrent sessions, ~25k sleep/until
wait loops. Výbava ships mods as a new catalog kind; Go keeps the logic and a
mod is wiring plus UI. The first wave is peek, lazarus, fleet and wake.

## Decisions

| # | Decision | Call | Why |
|---|----------|------|-----|
| 1 | First wave | Platform + peek, lazarus, fleet, wake | The four largest measured pains: "is it stuck" (peek, fleet), hand-resume after crashes and limit walls (lazarus), polling loops (wake). |
| 2 | Packaging | One mod per capability, composed by a `claude-mods` group | Catalog law: one item owns one capability. The engine allows `$` calls only inside one file per mod, so a bundle grows one ever-larger module and one crash unloads every feature. A provider noun (`engine.create`) is premature below 3 sharers. |
| 3 | Distribution | Catalog kind `mod`, installed into `~/.claude/skills/<id>`, staged outside that folder and swapped in by rename | Claude Code auto-loads and hot-reloads plugin folders there: no settings.json entry (survives a 2026-09-18-style rewrite), never in the plugin cache (plugin-gc untouched), versioned with the binary whose `--json` the mod calls. ~45 live sessions hot-reload on change, so a half-copied module must never be visible. |
| 4 | Guard tier | Leave it: no house rule, no managed-settings move | Lukáš's call. No first-wave mod gates a tool call; revisit when a gating mod (weather, lanes, leases) is proposed. |
| 5 | Screen space | Band rows only when actionable; quiet status lines for passive state; peek in the engine's spinner; fleet as a pane on command | Two band users (lazarus, wake) stack via `await next(e)` with digit hotkeys and collapse when handled; nothing competes for rows while all is well. |
| 6 | Lazarus autonomy | Usage-limit stalls continue by themselves at the reset; crashes, restarts and auth failures wait for one key | Nothing broke in a limit stall. The 2026-09-27 Warp crash was caused by a workflow, and 40 sessions auto-resuming after a restart would hit the memory ceiling together. |
| 7 | Fleet home | `/fleet` pane fed by one Go reader, `vybava fleet --json`; Claude rows for every session on any version, Codex rows read-only | One reader; the engine's session registry already carries state and `waitingFor` for every Claude session. SwitcherooBar and operator can read the same JSON later in their own repos. |
| 8 | Wake engine | LaunchAgent `vybava watchd` plus a `vybava watch` CLI; the mod long-polls in ≤ 25 s slices | One poller machine-wide (N sessions on one PR cost one poll, one GitHub budget); the CLI serves Monitor and Codex; no 45 resident children on a Mac at its memory ceiling. |

## Assumptions

- Go owns all domain logic; a mod calls `vybava <applet> … --json` through `$.process.run`/`$.process.spawn` and renders. A Go table a mod needs in-process is generated as TypeScript with a drift check, the way `vybava config check` holds `config-helpers.ts`.
- Hard bans never move into a mod (mods fail open and do not exist in Codex). Nothing classic is removed in this epic: `statusline.sh` and every settings hook stay — 42 of 49 live sessions still run 2.1.284.
- First-wave mods never answer `tool.call` without `next()` and never hook `tool.check` (a fact of their design, not an enforced rule — decision 4).
- Build loop: prototype in the session's `~/.claude/dev-mods/<session>/`, graduate into `vybava/mods/<id>`; `claude plugin validate` and `claude plugin test` (terminal and desktop surfaces) join `devbox run verify`.
- peek adds no command: the built-in `/btw` already asks the busy session a side question.
- fleet's `[reply]` is typed and pressed by the human (`$.session.send`); a reply to a busy session lands in its running turn.
- lazarus keeps one ledger file per session, written only by that session, under `~/.local/state/vybava/`; never `$.store` (one JSON per plugin shared by ~45 processes, not atomic across them).
- A dead session runs no mod: Go finds it (process gone while `busy`) and fleet shows it with its `claude --resume <id>` line.
- wake probes: GitHub PR checks and merge state plus Eve (the source gitkit reads), devbox, vitrinka, deployik — ordered by measured wait loops (devbox 1,501, gh 1,322, vitrinka 878). An event on an idle session submits a facts-only prompt; during a turn it waits for `turn.complete`.
- Doctrine's Go half (clamp near-budget Reads with `updatedInput`) belongs to epic #857. The two real bugs found by the research (memo's Stop hook rescans the whole transcript; `statusline.sh` writes dead `.session-titles` files) are fixed as their own PRs.
- Scope: No sprint (Výbava has no active sprint).

## Architecture notes

```
 ~/.claude/sessions/*.json ──┐                       ┌── gh / gitkit (Eve) · devbox · vitrinka · deployik
 lazarus ledgers (per sess) ─┤                       │
 Codex readers (read-only) ──┤                       │
                       vybava fleet --json     vybava watchd (LaunchAgent, unix socket)
                       vybava revive --json            ▲ subscribe / long-poll ≤25 s
                             ▲                         │            ▲
   ~/.claude/skills/<id>  ───┼─────────────────────────┼────────────┼── vybava watch --until (Monitor, Codex)
   peek     spinner suffix · long-turn line    (tool.call ledger in $.state)
   lazarus  tool.call ledger → per-session file · band row [1] resume · limit reset timer
   fleet    /fleet pane · status "N waiting on you"
   wake     wake_when tool · band row on transitions · $.prompt.submit when idle
```

Installer: new `KindMod` in `internal/catalog`, payload `mods/<id>/` embedded
beside `skills`, a Claude-only adapter in `internal/installer` that stages
outside `~/.claude/skills` and renames in; `.claude-plugin/types/` is
engine-written at load and is excluded from drift checks and gitignored.

## Open questions

None for this wave. Later waves, each its own child epic and short map:
doctrine (mod half), cockpit + context, weather, brief, vitrinka band (in
vitrinka-kit), hook-health.
