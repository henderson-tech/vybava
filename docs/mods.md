# Mods — Claude Code function-hook plugins shipped by Výbava

A mod is a Claude Code plugin of function hooks (Claude Code 2.1.287+): one
TypeScript module whose hooks run in-process on every event, with session
state, UI (the spinner, the band above the prompt, status lines, panes,
toasts) and the power to rewrite or register tools. Výbava ships mods as the
catalog kind `mod`; the payload lives in `mods/<id>/`.

```sh
vybava install peek              # → ~/.claude/skills/peek, loaded by every new session
vybava update peek               # swap in this release's copy; live sessions hot-reload it
vybava uninstall peek
vybava doctor                    # runs `claude plugin validate` on every installed mod
```

Mods are Claude Code only: Codex has no mods, so `install peek --agent codex`
is refused, and a group installed with `--agent codex` skips its mods and
names them on stderr.
Epic and decisions: `docs/specs/2026-10-02-claude-mods-decisions.md`.

## Layout

```
mods/<id>/
  .claude-plugin/plugin.json   name = the catalog id; "types" names the contract
  hooks/hooks.json             {"modules": ["./register.ts"]} — one module
  hooks/register.ts(x)         export const register: Register = on => { … }
  types/index.d.ts             the $.state contract (interface PluginState)
  <id>.test.ts                 `claude plugin test mods/<id>`
```

The catalog refuses a mod whose `plugin.json` name is not its id or whose
`hooks.json` is missing. `.claude-plugin/types/` is the engine's: Claude Code
writes the API declarations there at each load; it is gitignored, never
shipped, and carried (not deleted) across an upgrade.

## Install: one exchange, never a half-written module

Claude Code auto-loads every plugin folder under `~/.claude/skills/<id>` and
watches it, so each of ~45 live sessions hot-reloads on any change. The
installer therefore stages the payload under `~/.cache/vybava/stage/` and
swaps it with the prior copy in one atomic exchange (`renamex_np`
`RENAME_SWAP` on macOS, `renameat2` `RENAME_EXCHANGE` on Linux), so the
folder changes content and never vanishes; the old copy, now in the stage,
is deleted outside the watched folder. A filesystem without the exchange
falls back to two renames, restoring the prior copy if the second fails. A
session never sees a half-copied module, a staging directory, or two plugins
of one name. When
the stage root sits on another volume (a project-scope install elsewhere),
it stages in `.claude/.vybava-stage` beside the skills folder instead.

No `settings.json` entry is involved, so a harness rewrite of that file
cannot drop a mod, and nothing lands in the plugin cache plugin-gc sweeps.

## Authoring

1. Prototype where the engine hot-reloads: a session's `/plugin-authoring`
   folder (`~/.claude/dev-mods/<session>/<mod>/`, one "Enable hot reloading?"
   prompt) or `claude --plugin-dir <folder>`.
2. Read the API in the declarations the engine wrote for the running build
   (`.claude-plugin/types/claude-code/index.d.ts`, ~20k lines — grep it).
   The API is early access; the declaration file's first line names the
   version it was written by, and it is the authority, not this page.
3. Graduate into `mods/<id>/` with a catalog entry, then check:

   ```sh
   claude plugin validate mods/<id>     # what it hooks and calls, what the engine would refuse
   CLAUDE_CODE_ENABLE_FUNCTION_HOOKS=1 claude plugin test mods/<id>
   ```

   `plugin test` reports hooks "turned off in this process" without the
   variable even where sessions load mods; set it for the test run only.
   Loop UI tests over `['terminal', 'desktop']`. Type-check with the
   tsconfig in the declaration file's header, kept outside the mod folder.

## Runtime rules

- **Go owns the logic.** A mod is wiring and UI: it calls
  `vybava <applet> … --json` through `$.process.run` and renders. A Go table a
  mod needs in-process is generated as TypeScript with a drift check.
- **No per-session timer that spawns a process.** Every live session runs the
  mod; a 30 s `$.process.run` timer is ~45 processes every 30 s. Timers may
  touch `$.state`; shared data comes from one producer's file or daemon.
- **State:** `$.state` is the session's (survives a hot reload, reset by
  /clear and /resume); `$.store` is one JSON per plugin shared by every
  session and not atomic across them — per-session records go to a file the
  session alone writes, through a Go verb. Module variables die on reload.
- **Watch, don't gate.** A mod's `tool.call` hook runs above the settings
  hooks (claude-guards): one that answers without `next()` skips them. The
  hard bans stay classic Go hooks, which also cover Codex and fail closed.
- Hooks fail open: a hook that throws or overruns its 10 s budget is skipped.
  Nothing load-bearing for safety may live in a mod.
