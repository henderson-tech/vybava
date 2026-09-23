# claude-guards

claude-guards is the enforcement layer under `~/.claude/CLAUDE.md`. CLAUDE.md is
context — Claude reads it and usually complies. The bans in this applet are
incident-born and must hold unconditionally, including under bypass
permissions and inside subagents, where skills do not even load. It runs as a
Claude Code PreToolUse hook: one compiled process per Bash, Read or browser
call. Its main costs (measured 2026-09-22/23 on a 14-core Mac at load 11–27,
~2000 processes, under 0.5 GB free; a no-op `/usr/bin/true` takes 3–4 ms the
same way):

```text
every call        ~15 ms   process start: package init of every applet in the
                           multicall binary (~5 ms, mostly the sqlite libc's
                           netdb init) and the embedded catalog load
                  +2–3 ms  the last ≤4 MiB of transcript_path, on every allowed
                           call. No rule forks or calls the network outside the
                           rows below; a few stat or read small files
config found      +~2 ms   one load per call: a stat walk to the config, the
(vybava.config             git dir found without forking git, the cached
in cwd or above)           document read; the lok check reuses it. The first
                           load after the config file changes, and in every
                           new worktree, runs `bun -e` (~0.5 s); a failed
                           evaluation caches nothing, so every call re-runs it
line counts       ≤4 MiB   read per file a Read names (no limit or one above
                           guards.maxDumpLines; every text Read at ≥70%
                           context) or a cat/sed/head/tail names
commit-secrets    any `git … commit` in the command text (fail closed, no
                  shell parsing): per repo it names (cwd, `cd`, `-C`,
                  `--git-dir`) 5 git forks, staged and unstaged diffs; with
                  `git add` in the command also the untracked files (≤4 MiB
                  read). With added lines, a synchronous `gh repo view`
                  (≤2.5 s) once per repo — a failure is cached as unknown and
                  retried in the background (~80 ms per commit without gh)
machine caps      one `ps -axo` (~0.45 s) when a local segment boots a
                  simulator or starts a dev server (a `dev:*` script counts
                  when its package.json body is one)
browser           a loopback GET to onyx (1.5 s timeout) on every
                  playwright/chrome-devtools call
```

The lifecycle hooks have no matcher, so SessionStart fires on startup, resume,
`/clear` and every compaction, and SessionEnd on every exit reason including
`/clear`. They run in parallel within an event:

```text
SessionStart  doctor --fix                reads settings.json; writes only on drift
              weather --reap              one `ps -axo`, sysctl, vm_stat and a
                                          config load (~0.5 s); the orphan sweep
                                          reuses the table and, when it kills,
                                          adds a `pgrep` per victim process and 2 s
              swarm-teardown --dead-only  a tmux socket scan; per dead swarm
                                          `tmux list-panes` and `kill-server`, and
                                          a `pgrep` per process + 2 s if panes live
SessionEnd    swarm-teardown              the same, also for this session's own
                                          swarm, plus ≤8 `ps` up the ancestry
              browser-teardown            an onyx lookup (1.5 s) and stop (3 s)
              reap                        one `ps -axo`; kills as weather --reap does
```

At load ~400 the per-call figure rises by a quarter to a half (18–21 ms),
fork-bound paths about double (`ps -axo` ~1 s), and SessionStart's weather
takes 0.8–5 s.

Wire it once in `~/.claude/settings.json`. `doctor --fix` writes
`~/.local/bin/claude-guards`, the applet symlink `vybava install` creates; an
entry on the older `~/.claude/hooks/claude-guards` counts as present too,
since entries match on the verb:

```text
PreToolUse    Bash    claude-guards bash
PreToolUse    Read    claude-guards read
PreToolUse    mcp__playwright__.*|mcp__plugin_chrome-devtools-mcp_chrome-devtools__.*    claude-guards browser
              # browser:onyx-first (this session's Onyx browser must be running) and
              # browser:screenshot-dir (a screenshot file lands under .vitrinka/mcp/ —
              # the onyx playwright wrapper's --output-dir; chrome-devtools' filePath
              # is refused elsewhere; ignore the dir once in ~/.config/git/ignore)
SessionStart          claude-guards doctor --fix      # the hooks above are still wired
SessionStart          claude-guards weather --reap    # one line of machine pressure into context, then
                                                      # reap orphaned xcodebuild/WDA/Appium from the same `ps`
SessionStart          claude-guards swarm-teardown --dead-only
SessionEnd            claude-guards swarm-teardown
SessionEnd            claude-guards browser-teardown
SessionEnd            claude-guards reap
```

`claude-guards hooks` prints this wiring as JSON and `doctor` checks the live
file against it; `doctor --fix` also removes wirings an older manifest
installed (`retiredHooks` — the separate SessionStart `weather` and `reap`
that `weather --reap` replaced). `claude-guards list [family]` prints every rule from the
registry (`registry.go`): id, event, what it blocks, escape hatch.

A block prints its reason and the sanctioned alternative on stderr and exits 2;
that text is what Claude sees, so every message ends in the next command, not
in a bare "denied". Malformed payloads fail open — a guard that blocks
everything on a parse error would brick the session.

Rule families, each with its own escape hatch named in the block message:

```text
destructive:*     git stash · checkout/switch/restore . in the primary clone ·
                  compose down -v · db volume rm/prune · keychain value reads
secrets:*         env / printenv dumps, /proc/*/environ, docker inspect .Config.Env
simulator:*       cliclick / AppleScript System Events against the Simulator ·
                  running a script that opens a webdriverio remote() session
                  and deleteSession()s it per look outside appium/support,
                  appium/adhoc/lib, appium/specs, *.spec.ts or e2e
                  (guards.appiumSessionDirs extends the allowlist)
machine:*         playwright test / vitest / jest started on this Mac with no
                  worker cap, or one above guards.testWorkerCap (default 2);
                  ssh and devbox payloads, bun test, --version/--help/--list
                  and playwright install/codegen/show-report pass
                  (escape: CLAUDE_GUARDS_ALLOW_TEST_WORKERS=1) ·
                  a command matching a repo's guards.devboxOnly run outside
                  devbox run / ssh (escape: CLAUDE_GUARDS_ALLOW_LOCAL_STACK=1) ·
                  a simulator boot past guards.simCap (default 2) or a
                  Metro/next/API dev server start past guards.devServerCap
                  (default 3) (escape: CLAUDE_GUARDS_ALLOW_MACHINE_CAP=1)
e2e:*             raw simctl screenshots and raw .e2e PNG reads
plugincache:*     bun/npm/pnpm/yarn installs targeting ~/.claude/plugins/
commit-secrets    key files, secret-shaped lines, private infra strings in a public repo
context:*         inline python/node scripts that write files · cat/tee over an
                  existing file · cat/sed/head/tail or Read above 200 lines ·
                  dumping a ~/.claude/projects transcript · any raw read of a
                  locale catalog declared in vybava.config.ts (lok.catalogs) —
                  ranges included; the message points at lok get/grep/add ·
                  find/bfs/fd rooted at /, ~, /Users, /Users/<name>, /Volumes
                  or /Library (or run there with no root) without -maxdepth
```

The two `simulator:`/`context:` machine-health rules are incident-born
(2026-09-19, the day a `bfs /` crawl plus a per-look Appium probe pushed the
Mac to load 680). `context:root-walk` accepts a scoped root, `-maxdepth N`
(`fd -d N`), and points a whole-disk name lookup at `mdfind -name`.
`simulator:appium-session-churn` reads the script the command would run and
fires only when the file both imports `remote` from `webdriverio` and calls
`deleteSession(`; every fresh XCUITest session relaunches WebDriverAgent
through `xcodebuild` (5-10 s on every core, 1-2 GB), so a screenshot is
`xcrun simctl io <udid> screenshot <file>` and the a11y tree goes through ONE
session kept open for the task via the repo's session factory. Neither rule has
an escape variable: the sanctioned form is cheaper than the blocked one.

`machine:test-worker-cap` is the third machine-health rule (2026-09-19 again:
six headless Chromes at 40-110% CPU each, started by one uncapped
`playwright test`, pinned the Mac while 28 Claude sessions shared it). It reads
every local simple command, through `timeout`/`nice`/`env` and the package
launchers (`bunx`, `npx`, `bun run`, `npm run`/`exec`, `pnpm`, `yarn`), and
blocks `playwright test`, `vitest` and `jest` unless a worker cap at or below
`guards.testWorkerCap` (default 2) is on the line: `--workers N`/`-j N`,
`--maxWorkers N`, `--poolOptions.threads.maxThreads=N`,
`--poolOptions.forks.maxForks=N`, `--no-file-parallelism`, `--runInBand`/`-i`.
A cap above the limit is blocked too, and a percentage (`--workers 50%`) is no
cap. `bun test` always passes (single process, no worker flag), as do
`--version`/`--help`/`--list`, `vitest list`, `jest --listTests` and every
non-`test` playwright subcommand (`install`, `codegen`, `show-report`). A
command carried by `ssh` or `devbox run` runs on the box and is never read.
Unlike its two siblings this rule HAS an escape, because a deliberate
full-parallel run on a quiet Mac is legitimate:
`CLAUDE_GUARDS_ALLOW_TEST_WORKERS=1 <command>` as the command's env prefix.

`machine:devbox-only` is config-driven: a repo lists RE2 patterns under
`guards.devboxOnly` and any local command segment matching one (through
`timeout`/`nice`/`env` and leading assignments) is refused unless `devbox run`
or `ssh` carries it. FixIt's 2026-09-17 rule routing `apps/api`, `apps/web`
and `apps/admin-web` dev servers and suites to the Devbox lived only as prose,
and on 2026-09-19/20 three Metro bundlers, four API servers and two
next-servers ran on the Mac anyway. The message prints the exact `devbox run
-- '<cmd>'` form; a hand test the user asked for here sets
`CLAUDE_GUARDS_ALLOW_LOCAL_STACK=1`.

`machine:sim-cap` and `machine:dev-server-cap` count what already runs before
a boot or a start. A simulator boot (`xcrun simctl boot`, `expo run:ios`,
`bun run run:sim:*`, `run.ts sim`, `emulator -avd`) is refused at
`guards.simCap` booted simulators and emulators (default 2, one `launchd_sim`
per booted iOS device); a dev-server start (`expo start`, `next dev`, `turbo
dev`, `bun run dev*`, `run:api|web|admin`, `nest start`, `vite`) is refused at
`guards.devServerCap` running Metro, next and API servers (default 3, an
orchestrator and its leaf counted once). The process table is read only when
the command IS a boot or a start. The message lists the running servers by
pid and points at `/wk:pause` and the Devbox; a deliberate extra instance on
a quiet Mac sets `CLAUDE_GUARDS_ALLOW_MACHINE_CAP=1`.

`claude-guards weather` (SessionStart) prints one line the session starts
with — load against cores, free and compressor GB, claude and codex sessions,
sims, metro, next and api counts — and a second, warning line only under
pressure (free < 2 GB, load above the core count, or a cap already reached)
that names the Devbox, `/wk:pause` and how many sessions are older than 10 h;
`--text` adds those sessions as a table. `claude-guards reap` (SessionEnd,
and SessionStart through `weather --reap`, which hands over the process table
it already read) kills orphaned WebDriverAgent runners, `xcodebuild
test-without-building` and Appium servers older than ten minutes whose
claude/codex ancestor is gone; a process with a live owning session is never
touched. On 2026-09-19/20 four booted simulators, three Metro bundlers and
four API servers held about 25 GB and a thousand processes at the memory
ceiling, two orphaned xcodebuilds were 2 h and 19 h old, and the load average
peaked at 680.

`compose down -v` carves out worktree stacks — their databases are disposable
by construction — but only when the call NAMES one: `-p wt-<slug>` or
`-p wk-<slug>` (both worktree layouts), or a `wt-`/`wk-` prefixed worktree
directory as cwd. A bare `down -v` inside a worktree stays blocked: compose
resolves the project from that tree's `.env`, and a copied `.env` is exactly
how one worktree's teardown dropped another stack's volumes. Script-driven
teardown (`bun run worktree:cleanup … --remove`) never trips any of this — the
hook sees the command Claude runs, never what that command spawns.

`plugincache:package-install` is the one rule with **no escape hatch**, because
nothing legitimately installs packages into an installed plugin's cache.
`~/.claude/plugins/cache/<marketplace>/<plugin>/<version>/` is a clone of a
published plugin; the loader reads `skills/`, `agents/` and `.claude-plugin/`
and nothing else. Measured here: 18 cached versions of one plugin carrying
~478 MB of `node_modules/.bun` each, 8.4 GB total. Claude Code does not install
into plugin caches, `node_modules` is not tracked in the source repo, and the
marketplace is a GitHub source — yet every `node_modules` appeared hours after
its version was installed (active version: installed 12:02, `node_modules`
created 14:00). The culprit was never identified, so this rule is also the
detector: its message asks whoever tripped it to name the workflow that led
there. It fires on the cwd, on a `cd` into the tree earlier in the same
command, and on an explicit `--cwd` / `--prefix` / `--dir` / `-C` pointing
inside it. Reads, `ls`, and `bun run` / `npm run` inside the cache stay
allowed — sessions legitimately load skill files from there. Reclaiming what
already accumulated is `plugin-gc` (`docs/plugin-gc.md`).

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

## Self-check and host setup

On 2026-09-18 Claude Code rewrote `~/.claude/settings.json` outside any tool
call and dropped the claude-guards PreToolUse hooks; every session ran
unguarded for 36 h, which is how a whole-disk crawl and a per-look Appium loop
reached the machine. The wiring is therefore data (`claude-guards hooks` prints
the manifest as JSON) and `claude-guards doctor` diffs the live file against it
on every SessionStart. Without `--fix` it prints the missing entries to stdout,
so the session itself learns it is unguarded; with `--fix` (the SessionStart
wiring) it re-inserts them as a surgical merge — only the `hooks` key is
re-marshalled, every other key is written back from its raw bytes — and points
at `git -C ~/.claude diff settings.json`. A malformed or absent file is one
stderr warning, never a failed session. `vybava doctor` runs the same check.

```text
claude-guards doctor            # report only
claude-guards doctor --fix      # re-insert what a rewrite dropped
claude-guards hooks             # the manifest
vybava setup mac [--dry-run]    # idempotent host settings
```

`vybava setup mac` applies per-machine settings a Mac needs under many parallel
agent sessions; each step is checked first and a machine already set up is
left untouched. First step: `~/.gradle/gradle.properties` carries
`org.gradle.daemon.idletimeout=600000` — Gradle's 3 h default parked 10 GB of
idle Gradle/Kotlin daemons on 2026-09-19. The key is edited in place or
appended; other lines are never rewritten.
