# claude-guards

claude-guards is the enforcement layer under `~/.claude/CLAUDE.md`. CLAUDE.md is
context — Claude reads it and usually complies. The bans in this applet are
incident-born and must hold unconditionally, including under bypass
permissions and inside subagents, where skills do not even load. It runs as a
Claude Code PreToolUse hook: one compiled process per Bash or Read call,
single-digit milliseconds, no fork storms.

Wire it once in `~/.claude/settings.json` (the applet symlink lives at
`~/.local/bin/claude-guards`, so an older `~/.claude/hooks/claude-guards`
symlink keeps working):

```text
PreToolUse    Bash    claude-guards bash
PreToolUse    Read    claude-guards read
PreToolUse    mcp__playwright__.*|mcp__plugin_chrome-devtools-mcp_chrome-devtools__.*    claude-guards browser
SessionStart          claude-guards swarm-teardown --dead-only
SessionEnd            claude-guards swarm-teardown
SessionEnd            claude-guards browser-teardown
```

A block prints its reason and the sanctioned alternative on stderr and exits 2;
that text is what Claude sees, so every message ends in the next command, not
in a bare "denied". Malformed payloads fail open — a guard that blocks
everything on a parse error would brick the session.

Rule families, each with its own escape hatch named in the block message:

```text
destructive:*     git stash · checkout/switch/restore . in the primary clone ·
                  compose down -v · db volume rm/prune · keychain value reads
secrets:*         env / printenv dumps, /proc/*/environ, docker inspect .Config.Env
simulator:*       cliclick / AppleScript System Events against the Simulator
e2e:*             raw simctl screenshots and raw .e2e PNG reads
commit-secrets    key files, secret-shaped lines, private infra strings in a public repo
context:*         inline python/node scripts that write files · cat/tee over an
                  existing file · cat/sed/head/tail or Read above 200 lines ·
                  dumping a ~/.claude/projects transcript · any raw read of a
                  locale catalog declared in vybava.config.ts (lok.catalogs) —
                  ranges included; the message points at lok get/grep/add
```

`compose down -v` carves out worktree stacks — their databases are disposable
by construction — but only when the call NAMES one: `-p wt-<slug>` or
`-p wk-<slug>` (both worktree layouts), or a `wt-`/`wk-` prefixed worktree
directory as cwd. A bare `down -v` inside a worktree stays blocked: compose
resolves the project from that tree's `.env`, and a copied `.env` is exactly
how one worktree's teardown dropped another stack's volumes. Script-driven
teardown (`bun run worktree:cleanup … --remove`) never trips any of this — the
hook sees the command Claude runs, never what that command spawns.

The `context:*` family exists because the bypass-permissions harness text
tells Claude to prefer Bash over Read, Edit and Write. Measured on one epic
session (2026-09-10, 750k tokens): 271k of the agent's own shell-written
files, ~190k of whole-file dumps, 18k of reading its own transcript, and zero
Edit calls. The rules make Edit and ranged reads the only cheap path. Piped or
redirected reads never reach the context and are never blocked; `/tmp`,
`/var/folders` and `$TMPDIR` targets are exempt from the write rules.

`swarm-teardown` and `browser-teardown` are the lifecycle verbs. They block
nothing and decide nothing: they clean up what the ending session owns, log a
line to stderr, and always exit 0.

`browser-teardown` asks the resident onyx-mcp daemon whether the ending session
has a browser and, if it does, sends exactly one `browser_stop` for that
session's own id (the SessionEnd payload's `session_id`, else
`CLAUDE_CODE_SESSION_ID`). It asks first because `browser_stop` answers
`{"stopped":true}` for a session that never had a browser, and a hook that
reports a stop it did not perform is one nobody can read. Since Onyx went
HTTP-only one daemon owns every session's Helium, and it reaps a browser when
the browser's owning process dies — which is now the daemon itself, which never
dies. The idle watchdog is no backstop either: agents are routinely told to pass
`idle_timeout_seconds: 0`, which disables it. So browsers accumulate at ~0.5 GB
apiece — 23 of them on one Mac on 2026-09-12, the oldest 21 hours old. The
daemon cannot fix this from inside, because a session that will never call again
looks exactly like a session thinking; session end is knowledge only the ending
session has, and this hook is how it hands it over.

Two properties, both incident-born. It stops its OWN browser and nothing else —
no sweep, no pkill, never a peer's id, because an earlier cleanup that reached
wider killed every peer's browser and wiped their logged-in profiles. And it
fails open on everything: no token file, no daemon, refused connection, non-200,
JSON-RPC error, and the tool-level failure the daemon reports as `isError`
inside an HTTP 200. A hook that errors or hangs here would degrade every session
end on the machine, so the only failure it even prints is one the daemon itself
reported.

Try a rule without a hook payload:

```text
claude-guards check bash "git stash" --json
claude-guards check bash "cat apps/client/locales/cs.json" --cwd ~/Work/Projects/FixIt --json
claude-guards check read apps/client/locales/cs.json --json
```

`browser-teardown` has no `check` form, because it decides nothing — run it by
hand and it really does stop that session's browser. Always with `--session`:
without it the command waits for a hook payload on stdin, and a terminal never
ends one.

```text
claude-guards browser-teardown --session "$CLAUDE_CODE_SESSION_ID"
```

## What counts as a command

Every rule family inspects the same thing: the list of commands a string would
actually run. That list comes from one shared layer in `input.go`, so a fix
there lands in all of them at once. Three properties are load-bearing.

**Quoting decides whether text is a command or an argument.** Separators inside
quotes do not split, so `grep -nE "vault|env|path"` is one grep and not a pipe
into `env`, and `git commit -m 'fix: stop the crash; git stash was the cause'`
runs no stash. Single quotes suppress everything; double quotes suppress the
control operators but **not** command substitution, because the shell still
expands `$(…)` and backticks inside them — which is what keeps
`echo "$(git stash)"` from laundering a hard ban.

**Quoting does not make a payload inert.** `ssh host 'env'` and
`docker exec c sh -c "env"` really do dump an environment, so the quoted
arguments of a runner — `ssh`, `docker`, `kubectl`, `sh -c` and friends — are
recursed into and scanned as command lines. Every other command's quoted
arguments are data.

**A leading `NAME=value` is environment, not the command.** `FOO=1 env` runs
`env`, so assignment prefixes are stripped before a rule reads the first token.
They are stripped only from the front: `env FOO=bar make build` runs `env` as a
runner and keeps its assignment.

## Context tiers and diagnosis

The hook tails at most 4 MiB of `transcript_path` and uses the latest assistant
input + cache-creation + cache-read usage, not cumulative billed tokens.
Recognized Fable/Opus/Sonnet 5 models have a 1M window; Haiku has 200k.
Unknown models or unavailable usage fail open, reporting why on stderr. That
notice deliberately never travels back as `additionalContext`: it would enter
the model's context on every passing tool call, which is the cost these rules
exist to prevent.

At 50%, a private `<transcript>.budget-50` marker makes a model-visible reminder
once per transcript. The hook emits `hookSpecificOutput.additionalContext` on
stdout (exit 0); stderr alone is not a reminder delivery mechanism.
At 70%, text Reads or shell reads above 100 lines are denied until compaction
lowers usage. Screenshots remain available; existing E2E hygiene rules still
apply. `CLAUDE_ALLOW_CONTEXT_DUMP=1` is the explicit escape hatch (an inherited
environment variable for Read, a leading assignment for Bash).

`context:unbounded-output` requests caps for `docker logs`, GitHub run logs,
`git log` and unspecialized `git diff/show`. Examples: `docker logs --tail 200
app`, `git log -n 20`, `git diff --stat`. The suggested form keeps the
arguments you typed. Test runners are deliberately not covered: a passing suite
prints little, a failing one puts what matters at the end, and every repo here
documents a bare `go test ./...` / `bun test` as its verify step — a guard that
refuses the documented command only teaches people to route around it. List one
under `guards.unboundedCommands` if a specific suite really does flood. This is
a command-shape guard, not a shell interpreter or a guaranteed byte limit.

A pipe exempts a read only when the downstream command shrinks its input
(`| grep`, `| head`, `| jq`, `| wc`). `| cat`, `| tee` and `| less` reproduce
the file whole, so they are treated as the dump they are. Redirects to a file
remain exempt.

`guards.noRead` denies raw reads of generated paths, including short ranges and
non-reducing pipes; use `rg` or inspect their generating source.
`guards.maxDumpLines` replaces the normal 200-line allowance. See
[config](config.md) for discovery.

```text
claude-guards ctx latest
claude-guards ctx f9ee8c4e --json
```

`ctx` resolves a unique session filename prefix and reads it without modifying
it. `latest` considers sessions only — subagent and workflow transcripts are
skipped, or it would report another agent's context as yours. The report
includes recorded output/thinking usage, peak context, per-hour growth,
per-tool text estimates, top 15 results, image dimensions and estimates, and
recorded SessionStart todo hooks. Missing/malformed records are counted;
unknown image dimensions are explicit. Character-based text estimates and pixel
estimates (long edge capped at 1568, area / 750) are approximate. Dimensions are
read from PNG, JPEG, GIF and WebP headers; a header we cannot read is charged
the per-image maximum rather than zero, and counted as unknown. Output tokens
include thinking; do not add thinking again. Saved transcripts may not retain
every resume's hook event, so hook counts describe recorded evidence.
