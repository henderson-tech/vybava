# memorylint — AI memory home linting and maintenance

Understands Obsidian-style Markdown memory homes. `check` verifies frontmatter
schema and allowed types, kebab-case filenames, duplicate names, index/entry
line caps, missing index targets, dangling `[[wikilinks]]`, memories absent
from `MEMORY.md`, and unallowlisted email/IP fixture values.

## Lifecycle

Memory that never changed behaviour is context tax, so `check` also enforces
how notes age:

- a `provisional` note must carry `expires: YYYY-MM-DD`; promoting it to
  `active` must drop the field (error either way);
- an expired provisional is reported as deletable on sight;
- a note whose newest `last-used` / `last-verified` signal is over 90 days old
  is an eviction candidate (warning; undated notes are skipped, since no date
  is not evidence of disuse);
- a home over its note ceiling — 15 for personal (`user`/`feedback` only), 30
  otherwise — is flagged to consolidate or evict, never to raise the cap.

## Commands

```sh
memorylint check <home>                 # the rules above
memorylint check <home> --base <ref>    # + L009: a team ledger id naming another row on <ref> (default auto, none = off)
memorylint check <home>/<project> | <note.md>  # same rules, findings scoped to that project or note
memorylint fix [--dry-run] <home>       # normalize notes onto the flat v2 schema
memorylint new --home <home> --type project --name project-topic --description "Use when …"
memorylint new --provisional …          # status: provisional + expires 60 days out
memorylint reindex [--write] <home>     # render MEMORY.md deterministically
memorylint refs [--bare] <home> <file>… # references to notes that no longer exist
memorylint graph [--similar] <home>…    # wikilink graph, or likely-duplicate pairs
memorylint hook                         # Claude Code / Codex write hook
```

All commands honor `--json`. `reindex --team-index <path>` adds the routing
line pointing at the companion team home. `check` only walks the home; `refs`
covers pointers *into* the home from code and docs (`--bare` widens matching
to unqualified `<note>.md` names).

## Write hooks

`memorylint hook` reads a Claude Code / Codex hook payload on stdin, exits 0
outside memory homes, exits 2 to refuse a write. It refuses two things:

- **secrets**, before they are written;
- **the Edit/Write tools inside the agent-managed home**
  (`~/.claude/projects/<slug>/memory/`) — Claude Code normalizes frontmatter
  there *after* the post-write hook (flat v2 → nested `metadata:` envelope),
  which no in-session check can detect. The only reliable defence is refusing
  the tool and sending the caller to Bash or `memorylint new`. Team homes are
  unaffected; `memorylint fix` repairs already-drifted notes.

## Handoff homes

`~/.claude/handoffs/` is linted with its own schema instead of the memory one —
the same `check` and `hook` entry points, dispatched by path. A handoff is
`<project>/<slug>.md` or `<project>/<slug>/handoff.md`, live or under
`<project>/archive/`; any other `.md` is a context file and gets only the
secret scan (M011 — handoffs are machine-local and may name servers and people).

```yaml
name: <slug>              # H001 — must equal the slug
description: <one line>
status: open              # open · in-progress · done · abandoned
created: YYYY-MM-DD
feature: <project>/<taskId>  # vitrinka epic (or `none`); required from 2026-09-05, legacy exempt
created-by: <session id>  # uuid, or `unknown` for pre-schema handoffs
sessions: [<session id>]  # every session that worked it, created-by first
```

H002: `done`/`abandoned` belong under `archive/`, `open`/`in-progress` outside
it. H003 (warning, never a write block): a handoff is at most 200 lines.

The `feature` key is the ledger link: `<project>/<taskId>` (a vitrinka epic)
or `none`; required for handoffs created from 2026-09-05 on, legacy ones
exempt. Which open handoffs are still live is `handoffs reconcile` (`docs/handoffs.md`).

## Configuration

`.memorylint.yaml` at the lint root:

```yaml
version: 1
max_index_lines: 100
max_entry_lines: 150
allowed_emails: [qa-*@example.test]
allowed_ips: [192.0.2.*]
allowed_values: [fixture-token-value]
ignore: [archive/**]
```
