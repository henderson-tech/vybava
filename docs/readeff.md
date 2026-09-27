# readeff — how agents navigate code

An agent finds its way around a repository by reading and searching, and every
line it reads lands in context. `readeff` measures that traffic against what
the agent changed, and names the files and habits behind it: the file every
session re-reads, the whole-file read a grep would have answered, the search
run from the wrong directory.

```sh
vybava install readeff
cd <repo> && readeff report          # this repository, last 14 days
readeff report --all --days 3        # every repository
readeff files --top 20               # the files read most, with re-reads
readeff session <id>                 # one session, call by call
readeff report --json                # one runx envelope
```

`--project <dir>` reports on another repository, `--days N` sets the window,
`--big N` the whole-file threshold (default 400 lines) and `--window N` how
many calls after a search still count as following it up (default 3).
`--claude-root` and `--codex-dir` override `~/.claude/projects` and `~/.codex`.

## What it reads

Claude Code transcripts (main sessions, subagents, workflow agents) and Codex
rollouts whose file was written inside the window, through
`internal/transcripts`. Every tool call is paired with its result and reduced
to counts and paths: lines returned, file ranges, files changed. No command
text or output leaves the scan, and nothing is stored — every report scans
on demand, so there is no index to keep fresh.

A scoped report opens only the repository's transcripts: a Claude project
directory is named after its launch directory, and a rollout's first record
names its cwd. Scanning every repository reads every transcript in the window —
a few GB for a busy week — on up to four workers, one session in memory per
worker.

## Metrics

| metric | means |
|---|---|
| lines read / line changed | read + search + mixed lines per line an edit or write changed — the headline cost |
| mixed lines | one result holding output of different kinds (`cat a.go; rg foo`): the split is unknowable, so it is never guessed |
| re-read rate | reads whose range overlaps an earlier read of the same file, with no edit of it and no compaction in between |
| whole-file big reads | a read that returned an entire file above `--big` lines |
| search → read hit rate | searches that printed files, followed within `--window` calls by a read of one of them |
| empty searches | searches that returned nothing; "re-run" when the same search found something within the window |
| stale-state re-shows | an edit the harness had to recover, or a shell command that changed a file the session had read |
| guard blocks | claude-guards denials, by rule |

Most-read files fold worktree copies (`<repo>/.worktrees/<name>/x`) into the
repository's `x`; re-reads stay per real path.

## Classifying shell commands

Most navigation is shell, not the Read tool: `cat`, `sed -n`, `grep`, `rg`,
often behind `cd … &&` or `W=… &&`. Commands are split through
`internal/shellseg` — the same segmentation claude-guards enforces — then
classified **per pipeline**, because only a pipeline's last stage prints into
the result:

- **edit** — any write: a redirect into a file (scratch dirs like `/tmp` are
  not code), `sed -i`, `perl -i`, `tee`, `apply_patch`. An apply_patch body
  counts its `+`/`-` lines; a quoted heredoc written to a file counts its body.
- **read** — `cat`, `nl`, `bat`, `head`, `tail`, `sed`, `git show rev:path`
  with a file operand. `sed -n 'A,Bp'` and `head -n N` keep their range; a
  read filtered downstream (`| head`) keeps its file but not its range.
- **search** — `grep`, `rg`, `ag`, `find`, `fd`, `ls`, `tree`, `git grep`,
  `git ls-files` — and a read piped into one (`cat big.go | grep x`): the
  file only fed the search.

A pipeline redirected into a file put nothing into context. One call records
every kind it did (`cat a.go; echo x > a.go` is a read, then an edit), and its
output lines go to a kind only when every piece of output is of that kind —
otherwise they are mixed. `echo`/`printf` separators are not output.

`cd` segments and the command's own `NAME=value` assignments are followed, so
`W=~/repo/.worktrees/x && cd $W && sed -n '1,40p' a.go` reads
`~/repo/.worktrees/x/a.go`. The range parser here is readeff's own: the
claude-guards dump rules size reads against a budget from disk, which is a
different question.

## Codex

A Codex `exec` call is a JS program. Its `tools.exec_command({cmd: …})` and
`tools.apply_patch(…)` string literals are read (`transcripts.ResponseItem.
Commands`); a command built at runtime from variables is invisible, never
guessed. One program returns one output for all its commands, so its lines
belong to the call as a whole, and emptiness and search hits are judged only
for single-command calls.
