# macwatch - Mac load sampler with owner attribution

macOS keeps no per-process CPU history: by the time `top` is opened the
offender has finished, and Activity Monitor cannot say which project,
worktree or agent session a `node` belonged to. `macwatch` is the sampler
that answers "which process, which project, which directory, which session"
over time, and the report that turns a run into something pasteable.

Born 2026-09-19, the day a whole-disk `bfs /` crawl, a per-look Appium probe
relaunching WebDriverAgent through `xcodebuild` every 15 s, four booted
simulators, three Metros, four API servers, an idle 10 GB Gradle/Kotlin daemon
pair and 55 Claude sessions in 74 Warp tabs pushed a 14-core Mac to load 680.

```sh
vybava install macwatch

macwatch sample                                  # 2 h, every 30 s, appends to
                                                 # ~/Exports/Personal/mac-monitoring/<date>-macwatch.tsv
macwatch sample --every 10s --for 20m --out /tmp/load.tsv
macwatch sample --for 0                          # until Ctrl-C
macwatch sample --once                           # one sample to stdout (--json for the struct)

macwatch report ~/Exports/Personal/mac-monitoring/2026-09-20-macwatch.tsv
macwatch report <file.tsv> --top 15 --md         # Markdown tables for a vitrinka artifact
macwatch report <file.tsv> --json                # stable struct for agents
macwatch report <file.tsv> --cores 14            # spike line = 2 x cores (default: this Mac's cores)
```

## What one sample costs

Five commands, one sample: `sysctl -n vm.loadavg`, `top -l 1 -n 0`, one
`ps -axo …` over every process, `xcrun simctl list devices booted`, and ONE
`lsof -a -d cwd -Fpn -p <pids>` for every selected process and its owner
session together. Around 1 s on a quiet machine, under 3 s on a loaded one -
`ps` and `lsof` scale with the process count. No cgo, no private frameworks,
so the same binary ships for every platform even though only darwin output is
parsed.

## Attribution

- **Owner session** - the nearest `claude` or `codex` ancestor within 8
  parents: its kind, pid and tty. "Which Warp tab" is the tty; the pid is what
  `claude-guards ctx` and `codexusage` key on. `-` means no agent spawned it.
- **Project** - the process's cwd under `~/Work/Projects`, reduced to
  `<org>/<repo>` or `<org>/<repo>:<worktree-slug>` (`.worktrees/<slug>` inside
  the repo and the devbox-style `<repo>.worktrees/<slug>` beside it both
  collapse to the slug). A process without a project of its own (a shell
  helper in `/`) inherits its owner session's project.
- **Selection** - the top 12 processes by CPU at or above 3 %, plus the top 4
  by RSS; the rest of the machine is only in the system row.

## The TSV

Tab-separated, append-only, one file per day. The shape is shared with the
bash prototype that preceded the applet, so `report` reads either.

`S` - one per sample:

| # | column | meaning |
|---|---|---|
| 1 | `S` | row kind |
| 2 | `ts` | local time, `YYYY-MM-DDTHH:MM:SS` |
| 3-5 | `load1` `load5` `load15` | `sysctl vm.loadavg` |
| 6 | `procs` | total processes (`top`) |
| 7 | `running` | running processes (`top`) |
| 8 | `threads` | threads (`top`) |
| 9 | `used_gb` | PhysMem used |
| 10 | `free_gb` | PhysMem unused |
| 11 | `compressor_gb` | memory in the compressor - the number that says "swapping" |
| 12 | `claude` | `claude` processes |
| 13 | `codex` | `codex` processes |
| 14 | `sims` | booted simulators |
| 15 | `xcodebuild` | processes whose command line mentions `xcodebuild` |
| 16 | `chrome` | `chrome-headless-shell` processes (Playwright) |
| 17 | `tsc` | command lines mentioning `tsc` |
| 18 | `java` | `java` processes (Gradle, Kotlin daemons) |

The prototype wrote megabyte memory cells as an unevaluated division
(`2613/1024`); the reader evaluates it. Note the `top`-derived order is
total, running, threads - the prototype's header comment had running first,
the data never did.

`P` - one per selected process, after its `S` row:

| # | column | meaning |
|---|---|---|
| 1 | `P` | row kind |
| 2 | `ts` | same timestamp as the `S` row |
| 3 | `pid` | |
| 4 | `ppid` | |
| 5 | `cpu` | `ps %cpu` at that instant |
| 6 | `rss_mb` | resident set, MB |
| 7 | `etime` | elapsed time, `ps` format (`03-04:23:36`) |
| 8 | `tty` | the process's own tty, `??` when none |
| 9 | `owner_kind` | `claude`, `codex` or `-` |
| 10 | `owner_pid` | session pid or `-` |
| 11 | `owner_tty` | session tty or `-` |
| 12 | `project` | `<org>/<repo>[:<worktree>]` or `-` |
| 13 | `cwd` | working directory or `-` |
| 14 | `cmd` | command with home, `/System/Library`, `/Applications` and simulator path prefixes dropped, cut to 80 characters |

`DONE <ts> <n> samples` - trailer written when a run ends (also on Ctrl-C);
a file without it is a run still going or one that was killed.

## The report

1. **Timeline** - load1/load5, free and compressor GB, claude sessions and
   booted sims; at most 24 evenly spaced rows, the load peak and the free-memory
   trough always included and marked.
2. **Top offenders** - by integrated CPU (Σ cpu% x interval, in core-minutes;
   the last sample gets the median interval) and by peak RSS, each with project,
   cwd, owner session, samples seen and first/last timestamps. A process is
   `pid + executable`, so a recycled pid does not merge two lives.
3. **Per project** and **per owner session** - core-minutes, the largest sum
   of RSS its processes held in one sample, and distinct processes.
4. **Spike ledger** - every sample with load1 >= 2 x cores and the three
   processes burning the most CPU at that moment.

`--md` prints the same sections as Markdown tables; `--json` the whole
`Report` struct with stable field names.
