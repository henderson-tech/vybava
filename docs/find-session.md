# find-session — which session wrote this?

You copy the end of a conversation out of a terminal and want that session
back. `find-session` matches the text against every Claude Code transcript and
prints the session that wrote it, plus the one line that resumes it from its
launch directory under the switcheroo preset it started with.

```sh
vybava install find-session-cli find-session   # the applet and its /find-session skill
find-session                       # whatever is on the clipboard
pbpaste | find-session --json      # one runx envelope
find-session 'Trap for that deploy'
find-session e3f88062              # an id or id prefix; a trailing "." is forgiven
```

```
find-session · clipboard · 162/3900 sessions in 188ms

e3f88062-ca30-4f57-a535-fd8d3a73c723  Hash suffix on kmgym PIN templates
  ~/Work/Projects/Reservine/reservine (devlp) · 2026-10-01 10:25–14:44 · opus-5-5 high · 6/8 matched, its ending
  cd ~/Work/Projects/Reservine/reservine && cco -- --resume e3f88062-ca30-4f57-a535-fd8d3a73c723
```

The text comes from the arguments, else stdin, else the clipboard (macOS).
The caller's own session (`CLAUDE_CODE_SESSION_ID`) is left out, because it
holds the paste, unless you pass `--include-current`. `--limit N` (default 3), `--days N`,
`--full` and `--root` (default `$CLAUDE_CONFIG_DIR/projects`, else
`~/.claude/projects`) scope the search.

## Matching

The terminal renders markdown away (inline code, bold, headings), wraps
lines and adds its own chrome, so a paste never matches its transcript byte
for byte. The query becomes up to 8 phrases of plain words (16–32 bytes, no
code punctuation, spread across the paste), and a transcript matches when it
holds half of them. A short query (no such phrase) needs every word of 3+
characters. Matching is literal and case-sensitive.

Each matching transcript is then read line by line. Hits in the text of its own
assistant replies, or in its title, make it the **author**. A tool call carrying the
text (a find-session query, a file write) only quotes it. Hits anywhere else
(a human paste, a tool result) only **quote** it. Authors rank first, then the
most phrases matched, then the text nearest the session's end, then the
newest.

Only main sessions (`<project>/<id>.jsonl`) are searched, because subagent
transcripts can't be resumed.

## Speed

Transcripts are scanned newest first in tiers (2 days, 14 days, everything),
stopping at the first tier where some session authored the text. Each
phrase is searched by its rarest byte. Each scan worker streams files through one
fixed 8 MB buffer. A file is dropped once it can no
longer reach half the phrases. On a 13 GB, 3,900-session history a
recent match takes about 0.2 s warm and 1 s cold; a full scan takes 3–6 s.
Nothing is stored or indexed. Titles, prompts and echoed phrases pass through
`internal/secretscan` first, so a credential in a transcript never reaches the output. `PARTIAL_SCAN` (info) says a search stopped early; its fix is the same
query rerun with `--full`.

## Reopen line

The preset comes from the transcript: the model and `effort` of the first
reply, plus `ultracode` when an `ultra_effort_enter` attachment comes before
it (a `ccoo` launch; a later one is the prompt keyword, not the preset).

| started as | line |
| --- | --- |
| Opus ultracode | `ccoo` |
| Opus high | `cco` |
| Opus low | `ccol` |
| Fable high | `cch` |
| Fable low | `ccl` |
| other Opus/Fable | `cc --model <opus\|fable> --effort <effort>` |
| other models | `cc` |

`claude --resume` finds a transcript only from the directory the session was launched in, so
the line starts with `cd` into it. A removed worktree reports `CWD_MISSING`,
and its fix recreates the directory first.

## Diagnostics

`EMPTY_QUERY`, `NO_NEEDLES`, `NO_MATCH` (exit 2), `ROOT_MISSING`, `BAD_FLAG` are errors.
`QUOTED_ONLY` (the best match only quotes the text; the author may be a
subagent or deleted), `AMBIGUOUS` (the top two tie), `MANY_MATCHES` (more than
100 sessions hold the query; only the 100 with the most phrases are ranked),
`CWD_MISSING` and `CWD_UNKNOWN` (no launch directory recorded: the line is the
resume alone, to run from that directory) are warnings. `PARTIAL_SCAN` is info. Under `--json`, `next` carries the fixes and
then the top session's reopen line.
