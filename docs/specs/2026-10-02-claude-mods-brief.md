# Claude Code mods: platform + first wave — Brief

Epic: https://app.vitrinka.ai/w/fixit/p/vybava/t/4759 · brainstorm: terminal-only (no board) · log: 2026-10-02-claude-mods-decisions.md

## Intent
Lukáš runs a median of 17 and a peak of 67 Claude sessions. He loses time polling them ("is it stuck?" 59× a week), resuming them by hand after crashes and limit walls (70 bare "continue"), and in sleep-loops that every session runs on its own (~25k a week). Výbava gains a `mod` catalog kind and four Claude Code 2.1.287 mods (peek, lazarus, fleet, wake) that make this state visible and recoverable with one key. The domain logic stays in Go. It serves Lukáš at the keyboard and the agents in his sessions.

## Vocabulary
- `mod` — a Claude Code plugin of function hooks; in Výbava, a catalog kind installed into `~/.claude/skills/<id>`.
- `band` — the row above the prompt (`AbovePrompt` site); a mod draws there only when something can be pressed.
- `status line` — `$.ui.status`, one quiet text line per mod under the prompt.
- `pane` — a `$.ui.open` panel, opened on command (`/fleet`).
- `session registry` — `~/.claude/sessions/*.json`, the engine's per-process record (`status` busy|idle|waiting|shell, `waitingFor`, `pid`, `procStart`, `version`).
- `ledger` — lazarus's per-session record of the background jobs a session started.
- `watchd` — `vybava watch serve` as a LaunchAgent: one poller for every subscriber.
- `stall` — work stopped by a crash or restart, a usage limit, or an auth failure.

## As-is
- `internal/catalog/catalog.go` knows the kinds applet|skill|tool; `bundle.go` embeds `catalog/catalog.yaml skills`.
- `internal/installer/installer.go` `installSkill` stages inside the destination's parent, then RemoveAll and rename. Live sessions can see the gap.
- Claude hooks are classic settings.json commands: PreToolUse (claude-guards bash/read/browser, memorylint, memo, vitrinka touch), 8 SessionStart, 4 Stop. The status line is `~/.claude/statusline.sh`.
- `internal/operator/attention.go` guesses attention from transcript text. `internal/codexusage/live.go` enriches Codex processes through ps/lsof.
- `internal/plugingc` owns the rule that a process is dead only when proven (pid plus procStart). `internal/macwatch` attributes load to an owning session.
- Each session polls its own PR with `vybava gitkit pr-events` under Monitor (1,630× a week); `skills/prm` drives merges.
- `internal/vpn/daemon.go` installs LaunchDaemons through a `[]Step` plan with `--dry-run`. watchd's LaunchAgent follows that precedent.
- No mod exists yet. `~/.claude/dev-mods/<session>/` is the plugin-authoring prototype folder.

## To-be
1. As Lukáš, I want `vybava install claude-mods` to place the mods where every new session loads them, so that settings.json stays untouched.
2. As Lukáš, I want a mod upgrade to swap in with one rename, so that ~45 hot-reloading sessions never load a half-copied module.
3. As Lukáš, I want the spinner to say what the turn waits on and for how long, so that I stop asking "is it stuck?".
4. As Lukáš, I want turns over 30 min to end with a time and token breakdown, so that I see where a long turn went.
5. As Lukáš, I want a session to record the background jobs it starts, so that a crash doesn't lose them.
6. As Lukáš, I want one key after a crash or restart to resume the dead workflows, so that I never paste `Workflow({scriptPath})` or type "continue" again.
7. As Lukáš, I want a usage-limit stall to continue by itself at the reset, so that overnight work doesn't wait for me.
8. As Lukáš, I want `/park` to tell me whether a restart is safe, so that I restart without killing work.
9. As Lukáš, I want `/fleet` to show every session by project, waiting-on-me first and dead ones marked, so that I know where to look.
10. As Lukáš, I want to reply to a waiting session and copy a dead session's resume line from the pane, so that I stop hunting Warp tabs.
11. As Lukáš, I want a quiet "N waiting on you" status line in every session, so that I notice without opening anything.
12. As an agent, I want a `wake_when` tool that wakes me when a PR, CI, Eve, devbox, vitrinka or deployik condition holds, so that I stop sleep-looping.
13. As Lukáš, I want a band row and a toast when a watched target changes state (CI red, Eve done, merged, deployed), with `/prm` one key away, so that I act at once.
14. As a Codex lane or a script, I want `vybava watch … --until` as a blocking CLI, so that Monitor and Codex use the same single poller.
15. As Lukáš, I want N sessions watching one PR to cost one poll, so that the GitHub budget and the Mac aren't burned.

## Architecture
- `internal/catalog` + `bundle.go` — does: kind `mod` (requires `mods/<id>/.claude-plugin/plugin.json` and `hooks/hooks.json`; Claude-only), embeds `mods` · used by: installer, doctor.
- `internal/installer` (mod adapter) — does: stages under `~/.cache/vybava/stage`, renames into `~/.claude/skills/<id>`, writes the marker, never touches `.claude-plugin/types/` · depends on: catalog payload.
- `internal/doctor` — does: `claude plugin validate` on installed mods; flags a Claude Code newer than the mods were tested on.
- `internal/fleet` + `vybava fleet` — does: reads the session registry and decides liveness (pid plus procStart); adds Codex rows from codexusage's live view; owns the ledger (`fleet ledger record|show`, atomic, one file per session) and `fleet revive` · used by: fleet and lazarus mods, watch.
- `internal/watch` + `vybava watch` — does: `watch serve` daemon (LaunchAgent via a `[]Step` plan), subscriptions, dedupe, backoff, gh budget, probes (gh and gitkit Eve, devbox, vitrinka, deployik); serves a unix-socket API; publishes `~/.local/state/vybava/fleet/summary.json` · used by: wake mod, fleet mod, Monitor, Codex.
- `mods/peek` — does: a `tool.call` ledger in `$.state` drives the `Spinner` suffix, plus the long-turn line on `turn.complete` · depends on: nothing outside the engine.
- `mods/lazarus` — does: records jobs (Workflow runId+scriptPath, background Bash, Monitor, Agent) through `vybava fleet ledger`; band row on resume; limit-reset timer; `/park` · depends on: fleet.
- `mods/fleet-pane` — does: `/fleet` pane over `vybava fleet --json`, polled only while open; status line from the published summary · depends on: fleet, watch.
- `mods/wake` — does: `wake_when` tool; ≤ 25 s long-poll slices only while subscriptions exist; idle wake via `$.prompt.submit`; band row and toasts on transitions · depends on: watch.

## Decisions that matter
- One mod per capability, chosen over a bundled `vybava` mod because the engine's one-file `$` rule turns a bundle into one ever-growing module, and one crash would unload every feature.
- `~/.claude/skills/<id>` via the catalog, chosen over a marketplace plugin because it needs no settings.json entry (survives rewrites), stays out of the plugin cache and versions with the binary it calls.
- Band only when actionable, chosen over status-lines-plus-commands because one-key resume is the point. Passive state stays in quiet status lines.
- Limits auto-continue and crashes wait for a key, chosen over auto-resuming everything because a workflow caused the 09-27 Warp crash and 40 sessions resuming at once would hit the memory ceiling.
- One Go reader (`vybava fleet --json`) behind the pane, chosen over a SwitcherooBar-first view: one contract, and other consumers come later in their own repos.
- A LaunchAgent `watchd`, chosen over a watcher child per session: one poll per target, one GitHub budget, and no 45 resident children.
- Guard tier left as is, chosen over a house rule and a managed-tier move: no first-wave mod gates a tool call.

## Prerequisites
- Sessions on Claude Code ≥ 2.1.287 (restart; 42 of 49 live sessions are on 2.1.284).
- `claude` available where verify runs, for `claude plugin validate|test` (Devbox image; if missing, run locally and say so).
- Prove in WP1: a plugin in `~/.claude/skills/<id>` auto-loads and hot-reloads; whether the engine writes `.claude-plugin/types/` there.
- Prove in WP6: `$.http.fetch` over a unix `socketPath` reaches watchd; fallback `vybava watch next --timeout 25s` through `$.process.run`.
- `gh` signed in on the Mac (watchd uses the user's gh, never a stored token); Eve state comes from the source gitkit reads.
- A switcheroo repin verb for lazarus's auth row; if none exists, the row shows the command to run.
- No settings.json change and no classic hook removed.

## Acceptance criteria
1. S1 · `internal/catalog` unit: kind `mod` requires plugin.json and hooks.json, and refuses a Codex target.
2. S1 S2 · `internal/installer` unit: staging happens outside `~/.claude/skills`, the swap is one rename, the marker is written, and a reinstall leaves no `.vybava-*` dir behind.
3. S1 · `internal/doctor` unit with a fixture: an installed mod that fails validate is reported.
4. S3 · `mods/peek/peek.test.ts` (`claude plugin test`, terminal and desktop): a held Bash call shows "<description> · <age>" in the spinner suffix.
5. S4 · peek test: `turn.complete` with durationMs > 30 min returns the breakdown line; shorter turns return none.
6. S5 · `internal/fleet` unit: ledger record/show round-trips, writes are atomic, each session has its own file.
7. S6 · `mods/lazarus` test: after `classic.SessionStart{source:'resume'}` with an open Workflow job, the band row shows; pressing 1 calls Workflow with `resumeFromRunId`.
8. S7 · lazarus test (mock clock): a limit stall submits a prompt at `resetsAt` plus jitter, never before, and none if a turn started meanwhile.
9. S8 · lazarus test: `/park` lists the running jobs and gives a safe or not-safe verdict.
10. S9 · `internal/fleet` unit over fixture registry files: grouping, waiting first, died-while-busy detection, and an unknown registry shape fails loudly.
11. S10 S11 · `mods/fleet-pane` test: the pane renders fixture JSON; `[y]` sends the typed text via `$.session.send`; the status line counts from the summary file.
12. S12 S13 · `mods/wake` test: `wake_when` runs `vybava watch add …`; an event while idle submits a facts-only prompt; while busy it waits for `turn.complete`; a transition draws the band row and toast, and `[4]` runs `/prm`.
13. S14 S15 · `internal/watch` unit (fake gh): two subscriptions on one PR produce one probe per interval; `watch … --until` exits 0 on the condition; backoff and the gh budget hold.
14. S1–S13 · live: `vybava install claude-mods` on the Mac; a fresh 2.1.287 session exercises peek, `/park`, `/fleet` and `wake_when` on a real PR; a vitrinka board of the terminal shots.

## Work packages
Phases: 1 → 2 (parallel) → 3 (parallel) → 4. Shared files (`catalog/catalog.yaml`, `internal/cli/cli.go`, `CLAUDE.md`) are touched only in WP1, WP4 and WP8.
1. Platform + peek — files: `internal/catalog/**`, `bundle.go`, `internal/installer/**`, `internal/doctor/**`, `devbox.yaml`, `.gitignore`, `docs/mods.md`, `mods/peek/**`, the peek entry in `catalog/catalog.yaml` · done when: AC 1–5 pass and peek loads from `~/.claude/skills/peek` in a live session.
2. fleet Go — files: `internal/fleet/**`, `internal/cli/fleet.go`, `docs/fleet.md` · done when: AC 6 and 10 pass.
3. watch Go — files: `internal/watch/**`, `internal/cli/watch.go`, `docs/watch.md` · done when: AC 13 passes and the LaunchAgent plan renders with `--dry-run`.
4. Applet wiring (lead) — files: `internal/cli/cli.go`, the `fleet` and `watch` entries in `catalog/catalog.yaml` · done when: `go run ./cmd/vybava fleet --json` and `watch --help` work.
5. lazarus mod — files: `mods/lazarus/**` · done when: AC 7–9 pass.
6. fleet-pane mod — files: `mods/fleet-pane/**` · done when: AC 11 passes.
7. wake mod — files: `mods/wake/**`, `skills/prm/**` (prefers `wake_when` or `vybava watch` over pr-events Monitors) · done when: AC 12 passes.
8. Integration — files: the mod entries and the `claude-mods` group in `catalog/catalog.yaml`, `CLAUDE.md` pointer lines · done when: AC 14 holds, verify is green and the PR is opened via /prm.

## Open questions
None.

## Out of scope
- Later waves: doctrine (mod half), cockpit + context, weather, brief, vitrinka band (vitrinka-kit), hook-health.
- Moving claude-guards to the managed tier, or any house rule for gating mods.
- Retiring `statusline.sh` or any classic hook.
- A SwitcherooBar or operator fleet view. Mods for Codex (none exist).
- Auto-resume after crashes; a `/peek` command (the built-in `/btw` covers side questions).
- The Read clamp (epic #857), memo's Stop rescan and statusline's dead writes (their own PRs).

## Refs
- Brief (this file): load first.
- Decision log `claude-mods-decisions.md`: load before revising a decision.
- Research: session 51fc46df workflow wf_c7d8ae84-773, a capability map of the 2.1.287 API plus a week of transcript mining.
