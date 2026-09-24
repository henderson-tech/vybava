| L001 | error | a line is not in the row grammar (long dash, no period, two sentences, over 200 chars, bad link form, type not of this home, block id not matching the row id incl. the `t` prefix, a personal-form id in a team ledger), or `usage.jsonl` has a bad line |# memo

memo is the append-only memory ledger for AI sessions. A memory home holds one
`LEDGER.md` where every fact ever captured is a one-line row with a stable id;
`MEMORY.md` (the file the harness loads whole) is rendered from it, pinned rows
first, then by decayed usage score, capped at 100 lines; `usage.jsonl` records
which rows a session actually cited. Nothing is edited or deleted: a fact
changes by a new row that supersedes or retires the old one. Spec and decision
log: `docs/qna/2026-09-20-memo-ledger.md`.

## Files in a home

```text
<home>/
  LEDGER.md     append-only truth, every row ever written (memo add / import)
  MEMORY.md     rendered projection (memo render / ensure), never edited by hand
  usage.jsonl   one event per line: {"row":45,"kind":"cite","at":"RFC3339","session":"<id>"}
                kinds: add (the row's creation, written by memo add / import), cite, show, read, touch
  notes/        optional detail notes, v2 frontmatter, linked from rows
  .gitignore    team homes only: MEMORY.md and usage.jsonl (written by memo, never by hand)
```

Which files are shared and which stay on the machine depends on the home
kind (decision 2026-09-21: the team hot surface is local):

| File | Personal home | Team home |
|---|---|---|
| `LEDGER.md` | snapshotted by memo's local git | committed, shared through the repo |
| `notes/` | snapshotted | committed, shared |
| `MEMORY.md` | snapshotted | LOCAL: gitignored, rendered per machine (`memo ensure` at SessionStart) |
| `usage.jsonl` | snapshotted | LOCAL: gitignored, one machine's citations |

In a team home every writer (`memo add`, `memo import`, `memo render`,
`memo touch`, the Stop harvest) first makes sure `<home>/.gitignore` lists
`MEMORY.md` and `usage.jsonl`: the file is created when missing, the two
lines are added when absent, any other line is kept. Idempotent; a personal
home never gets one, nor does a team home outside a git work tree.

A team `MEMORY.md` that git still tracks (`git ls-files --error-unmatch`) is
never written and never gets an ignore line: git ignores nothing it tracks,
and rewriting it dirtied every such checkout until deploy scripts refused
the tree. The file stays as committed, and `add`, `import`, `render` and
`ensure` warn `SURFACE_TRACKED`. For a tracked render (a home committed
before 2026-09-21) the fix is `git -C <home> rm --cached -q -- MEMORY.md &&
memo render --home <home>`, run in a worktree, with the removal committed
alongside `.gitignore`. For a tracked hand-written v2 index the fix is
`memo migrate <home>`: fold the index into rows first, then untrack it, so
nobody drops an index that was never migrated.

Homes: personal `~/.claude/projects/<slug>/memory/` (types `user`, `feedback`)
and team `<repo>/.claude/memory/` (types `project`, `reference`). From a
linked worktree the personal home is the MAIN checkout's (Claude Code slugs
the cwd, but the repo's memory is the one that counts); the team ledger read
and written is the worktree's own `.claude/memory` (the branch being edited),
while `memo homes`, the registry and the vault always name the main
checkout's path, so a vault symlink never dies with a worktree. The personal
home is a local git repository owned by memo; the team home's ledger and
notes are versioned by its repository, its rendered surface is not.

`LEDGER.md` opens with a frontmatter block and the grammar comment:

```text
---
memo: 1
alias: fixit
kind: personal
repo: /Users/me/Work/FixIt        # personal homes only: the repo the slug encodes
---
<!-- - #<id> <type>/<topic>[!] <sentence> [-> <link> ...] ^m<id> -->
```

## Row grammar

```text
- #<id> <type>/<topic>[!] <sentence> [-> <link> ...] ^m<id>
```

A row carries no date (amendment 2026-09-21): its creation time is the `add`
event `memo add` / `memo import` write to `usage.jsonl`, so the ledger line
holds only what a reader acts on. A ledger written before the amendment
still leads each row with `YYYY-MM-DD`; the parser drops it, so a legacy
ledger keeps reading and taking `memo add` — nothing rewrites its rows.

- `id`: positive integer, strictly increasing per home, never reused. In a
  personal ledger the row is `#45 ... ^m45` and is cited `#45`; in a TEAM
  ledger every id carries a `t` prefix, `#t45 ... ^t45`, cited `#t45`, so a
  session holding both homes never confuses the two id spaces. The Obsidian
  block id (`^m45` / `^t45`) is always the last token, so `[[LEDGER#^m45]]`
  / `[[LEDGER#^t45]]` are real links.
- `type`: `user | feedback | project | reference`; `topic`: kebab-case.
- `!` after the topic pins the row: rendered first, never ages out.
- `sentence`: one line, one fact, plain hyphens only (no em or en dash, U+2014 / U+2013), ends with a
  period, at most 200 characters (a warning above 160).
- `supersedes #N: <sentence>` (`#tN` in a team ledger) marks row N superseded; `retires #N.` (with an
  optional reason after it) marks it retired. Both are ordinary rows; the old
  row is never touched and never rendered again.
- Links after `->`: `[[notes/<slug>]]`, `[[LEDGER#^m<id>]]`,
  `[[<alias>/LEDGER#^m<id>]]`, `[[<alias>/notes/<slug>]]`, `https://...`.

```text
- #45 feedback/git Never `git stash`; parallel sessions share the tree. -> [[notes/git-stash-race]] ^m45
- #46 reference/macos! AX exposes only the current Space; an off-Space frame() hangs until timeout. ^m46
- #61 feedback/git supersedes #45: stash is fine inside `.worktrees/`. ^m61
```

## Rendered MEMORY.md

Deterministic output of `memo render`: a header (for a personal home the
routing line `Team memory: <repo>/.claude/memory/MEMORY.md`, then the
two-line legend), `## Pinned`, `## Hot`. Rows print in the exact ledger
grammar, so a citation copied from either file is identical. Hard cap 100
lines including the header; rows past it stay in the ledger only.

Score per row: sum over its usage events of `weight * 0.5^(age_days/90)`,
weights `cite 1`, `show 1`, `read 1` (a Read of a linked note), `touch 2`;
`add` weighs 0, so a row is never hot merely for being new.
Order: pinned, then score descending, then newest id first. A row created
more than 180 days ago (its `add` event) whose newest usage event is more
than 180 days old (or that never had one) is not rendered; a row without an
`add` event (a ledger written before 2026-09-21) counts as created now, so a
legacy ledger never vanishes. Superseded and retired rows are never rendered.

## Verbs

Every verb takes `--home <alias|path>` (default: resolved from the cwd; a
verb that needs one home picks personal for `user`/`feedback` rows and team
for the rest) and `--json`, and answers with one envelope
`{v, ok, verb, data, diagnostics, next}`.

```text
memo add <type>/<topic>[!] "<sentence>." [--link <l>]... [--supersedes N] [--retires N]
                                          # append, render, snapshot the personal home
memo show <ref>                           # row + linked notes; records a show event
memo find <words>... [--all]              # sentences and topics; superseded rows marked
memo touch <ref>                          # explicit use, weight 2
memo render [--check]                     # write MEMORY.md; --check exits 2 on drift (compares the file on disk)
memo ensure                               # write MEMORY.md only when missing or older than LEDGER.md / usage.jsonl
memo import <file>                        # id-less rows, ids assigned in order; all-or-nothing
memo migrate <home>                       # v2 notes -> import template (helper, not the judgment)
memo homes [register <alias> <path>]
memo vault [--path ~/Memory]              # Obsidian vault of <alias> -> <home> symlinks
memo snapshot [-m msg] · memo log [-n N] · memo restore <rev> <file>
memo hook                                 # Claude Code / Codex hook payload on stdin
```

A `ref` is `45`, `#45`, `^m45` (personal), `t12`, `#t12`, `^t12` (team),
`fixit-team#12` or a `[[...LEDGER#^m45]]` / `[[...LEDGER#^t12]]` wikilink. A
bare id is looked up in the session's personal home, a `t` id in its team
home; `--home` or an alias overrides that.

`memo add` refuses `LEGACY_HOME` in a v2 home, meaning a hand-written
`MEMORY.md` and no `LEDGER.md`: a first render would replace that index, and
nothing snapshots a personal home before its ledger exists. `memo migrate`
+ `memo import` convert the home first. Elsewhere,
`memo add` creates the ledger on first use: the row's type decides the kind
(`user`/`feedback` personal, else team), the alias is the repo basename
lowercased (`-team` suffix for the team home), and a personal ledger records
the repo path. `memo add` records one `add` event for the new row and `memo
import` one per imported row (all at the same timestamp); an import file
may still carry the pre-amendment leading `YYYY-MM-DD`, which is dropped.
`memo show` and `memo touch` stamp their event with
`CLAUDE_CODE_SESSION_ID` when set, so the Stop hook's harvest of the same
session deduplicates against them.

`memo ensure` is the SessionStart verb: per session home it renders
`MEMORY.md` when the file is missing or its mtime is older than `LEDGER.md`
or `usage.jsonl` (`reason: missing | stale`), and exits 0 without touching
anything when it is current. A stale file whose render comes out identical
gets its mtime bumped, so the next start is the fast path. This is what
keeps a freshly cloned team home usable: the clone carries no `MEMORY.md`,
the first session renders it. `memo render --check` keeps comparing the
file on disk, so it still works locally in a team home whose `MEMORY.md` is
gitignored; in CI, where the file is absent, it would only report drift,
so CI runs `memorylint check` for team homes and nothing else.

## Homes, aliases, registry

`memo homes` lists every home it can see: every `~/.claude/projects/*/memory`
holding a `LEDGER.md`, the team home each personal ledger's `repo:` points
at, the cwd's own session homes, and `~/.config/vybava/memo/homes.json`:

```json
{"version": 1, "homes": [{"alias": "fixit-team", "path": "/abs/repo/.claude/memory"}]}
```

Unknown fields in the registry are rejected (`REGISTRY_INVALID`). A
registered alias wins over the alias in a ledger's frontmatter.

## Hooks

`memo hook` reads one hook payload on stdin and dispatches on
`hook_event_name`:

- **PreToolUse**: refuses Edit/Write/MultiEdit/NotebookEdit on `LEDGER.md`,
  `MEMORY.md` or `usage.jsonl` in any home, and Bash commands that write
  them (`>`/`>>` redirects, heredocs, `tee`, `sed -i`/`perl -i`, `cp`/`mv`
  destinations, `rm`). Exit 2; stderr names the memo verb that owns the file
  (`memo add`, `memo render`, `memo touch`). Files under `notes/` stay under
  memorylint's hook. Shell segmentation is claudeguards' one definition
  (`claudeguards.Segments`), so quoted mentions never trip it.
- **Stop / SessionEnd**: opens `transcript_path`, collects citations from
  assistant text and tool inputs (`#NN` / `^mNN` / `[[LEDGER#^mNN]]` credit
  the personal home, `#tNN` / `^tNN` / `[[LEDGER#^tNN]]` the team home,
  alias-scoped forms that alias only; word boundaries throughout), Read
  calls on `<home>/notes/*.md` (a `read` event for every row linking the
  note) and `memo show <ref>` Bash commands; appends events deduplicated per
  (row, kind, session) and re-renders when anything landed. Only ids that
  exist in a home count, so a PR number in prose is harmless.
- **SessionStart**: runs `memo ensure` over every session home (personal and
  team) that exists, so a team home just cloned or pulled has its
  `MEMORY.md` before the harness loads it. Never blocks: a home that cannot
  be rendered is one `memo hook: not rendered: <home>: <why>` line on
  stderr (a tracked team `MEMORY.md` is one, carrying its `SURFACE_TRACKED`
  fix), the other homes still render, exit 0.

Claude Code `settings.json`:

```json
{"hooks": {
  "SessionStart": [{"hooks": [{"type": "command", "command": "memo hook"}]}],
  "PreToolUse": [{"matcher": "Edit|Write|MultiEdit|NotebookEdit|Bash", "hooks": [{"type": "command", "command": "memo hook"}]}],
  "Stop": [{"hooks": [{"type": "command", "command": "memo hook"}]}]
}}
```

## Vault

`memo vault` maintains `~/Memory` (or `--path`): one symlink per home,
`<alias> -> <home>`, plus a minimal `.obsidian/app.json`. Idempotent: a
correct link is kept, a wrong one replaced, nothing else is touched. Open
the directory as an Obsidian vault and `[[fixit-team/LEDGER#^m12]]` resolves
there exactly as memo resolves it.

## Snapshots

The personal home's history is git. When the home already sits inside a
work tree (`~/.claude` is itself a repo), memo never `git init`s: it stages
only the home's own files (`git add -- <home>`), commits them path-scoped
(`git commit -m "memo: snapshot <alias>" -- <home>`) and leaves every other
dirty file in that repo untouched; `memo log` is `git log -- <home>` and
`memo restore <rev> <file>` is `git checkout <rev> -- <home>/<file>`. Only a
home outside any work tree gets its own repository (`git init` on the first
snapshot, never a remote). `memo add` snapshots after every row. In a team
home the three verbs say the repository owns the history
(`SNAPSHOT_TEAM_OWNED`, exit 0).

## memorylint

`memorylint check <home>` on a home with a `LEDGER.md` adds the ledger rules
and keeps the v2 rules for `notes/`; a v2 home without a ledger lints
exactly as before.

| Rule | Severity | Fires when |
|---|---|---|
| L001 | error | a line is not in the row grammar (long dash, no period, two sentences, over 200 chars, bad link form, type not of this home), or `usage.jsonl` has a bad line |
| L002 | error | ids are not strictly increasing (reported through L001 with the line) |
| L003 | error | a supersede/retire target does not exist, is later, or was already closed by another row |
| L004 | error | a link points at a missing note, row, or an alias no home carries |
| L005 | warning | a sentence is over 160 characters |
| L006 | error | `MEMORY.md` differs from `memo render` output; in a TEAM home a missing `MEMORY.md` is clean (it is a local projection) |
| L007 | warning | a `notes/` file is linked from no row |
| L008 | warning | a team home's `MEMORY.md` or `usage.jsonl` exists and is not gitignored (`git check-ignore`); fix: add both to `<home>/.gitignore`, which `memo render --home <home>` writes; a TRACKED file names its untracking instead (`SURFACE_TRACKED`'s fix), and a tracked `MEMORY.md` skips the L006 drift check |

CI for a team home runs `memorylint check <repo>/.claude/memory` and nothing
else (since 2026-09-21): `memo render --check` needs the machine-local
`MEMORY.md`, which a checkout does not carry.

In ledger mode `notes/<slug>.md` names are plain kebab-case (M002 is not
applied), M009 (not linked from the index) is replaced by L007, cross-home
wikilinks inside notes are left to memo, and the personal note ceiling (15)
counts `notes/` only.

## Diagnostics

Closed enum; every failure carries the exact `fix` and it lands in `next`.

| Code | Exit | When |
|---|---|---|
| `USAGE` | 2 | malformed invocation; fix is the corrected command |
| `HOME_NOT_FOUND` | 2 | no ledger for the cwd, alias or path |
| `LEDGER_INVALID` | 2 | `LEDGER.md` or `usage.jsonl` does not parse; detail names the line |
| `ROW_SYNTAX` | 2 | head or sentence outside the grammar (unknown type, bad topic, reserved token) |
| `ROW_LONG_DASH` | 2 | em/en dash in the sentence |
| `ROW_TWO_SENTENCES` | 2 | more than one sentence |
| `ROW_NO_PERIOD` | 2 | sentence does not end with a period |
| `ROW_TOO_LONG` | 2 | over 200 characters |
| `ROW_LONG` | 0 | warning, over 160 characters |
| `ROW_TYPE_HOME` | 2 | type does not belong in the resolved home |
| `ROW_LINK_INVALID` | 2 | link not in an accepted form, or pointing at a missing local note/row |
| `TARGET_UNKNOWN` | 2 | supersede/retire target not in the home |
| `TARGET_CLOSED` | 2 | target already superseded or retired; fix names the closing row |
| `REF_SYNTAX` | 2 | reference not `45`, `#45`, `alias#45`, `^m45` or a wikilink |
| `REF_UNKNOWN` | 2 | referenced row missing |
| `REF_AMBIGUOUS` | 2 | bare id exists in more than one session home |
| `RENDER_DRIFT` | 2 | `render --check`: MEMORY.md differs |
| `LEGACY_HOME` | 2 | `add` into a v2 home (hand-written `MEMORY.md`, no `LEDGER.md`); the row is not written; fix `memo migrate <home>`, then `memo import` |
| `SURFACE_TRACKED` | 0 | warning, a team `MEMORY.md` is tracked by git and was left as committed; fix untracks a render or migrates a hand-written index |
| `IMPORT_INVALID` | 2 | import file line outside the id-less grammar |
| `REGISTRY_INVALID` | 2 | homes.json malformed or with unknown fields |
| `HOOK_REFUSED` | 2 | PreToolUse would rewrite a ledger file by hand |
| `SNAPSHOT_TEAM_OWNED` | 0 | snapshot/log/restore in a team home |
| `SNAPSHOT_CLEAN` | 0 | nothing to snapshot |
| `INFRA_ERROR` | 1 | unstructured I/O failure |
