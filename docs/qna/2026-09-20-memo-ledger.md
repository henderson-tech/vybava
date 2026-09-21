# memo - the append-only memory ledger (qna log, 2026-09-20)

Settled with Lukáš through /qna. This file is the spec both the applet and
the migration follow; `docs/memo.md` is the user-facing reference once built.

## Problem

Memory notes were paragraphs grouped into "contract" files: 60 to 90 words per
fact, grab-bag grouping (a Fastify quirk under "working style"), no stable id
to cite, and no signal about which facts ever changed a session's behavior.
The harness loads `MEMORY.md` whole every session and silently drops content
past 200 lines, so the loaded surface has to stay small and has to earn its
lines.

## Decisions

| Fork | Call | Why |
|---|---|---|
| Ledger home | `LEDGER.md` is append-only truth; `MEMORY.md` is rendered from it, capped at 100 lines | nothing is ever deleted, the loaded surface self-organizes |
| Row syntax | Obsidian block ids: every row ends `^m<id>`; `[[LEDGER#^m45]]` is a real link, `#45` the short form | rows become addressable from any note, Obsidian previews them |
| Ordering | pinned first, then decayed usage score, then newest | the hot surface holds what actually gets used |
| Usage signal | Stop hook scans the session transcript for citations and note reads; `memo touch` for explicit use | deterministic, no self-reporting; last-used bumping never happened in practice |
| Applet | new `memo` applet; `memorylint` stays the linter and learns the row grammar | the daily capture door carries no lint baggage |
| Cross-home links | home aliases (`fixit`, `fixit-team`, `vybava`, ...) plus an umbrella Obsidian vault of symlinks at `~/Memory` | links resolve in the CLI and in Obsidian alike |
| Snapshots | the personal home becomes a local git repo owned by memo (never pushed); team homes are already versioned | diffs and rollback for free |
| Scope of this pass | both FixIt homes, doctrine, hooks, CI | prove it on the surfaces used daily |

Assumptions (not asked): the four note types stay the closed enum
(`user`, `feedback`, `project`, `reference`); detail notes keep the v2
frontmatter so existing lint keeps working; session ids appear only in
`usage.jsonl`, never in rows.

## The files in a home

```text
<home>/
  LEDGER.md     append-only truth, every row ever written
  MEMORY.md     rendered projection: pinned, then top rows by score, active only
  usage.jsonl   one event per line: {"row":45,"kind":"cite","at":"RFC3339","session":"<uuid>"}
  notes/        optional detail files, v2 frontmatter, linked from rows
```

Homes: personal `~/.claude/projects/<slug>/memory/` (types `user`, `feedback`)
and team `<repo>/.claude/memory/` (types `project`, `reference`). Routing is
unchanged from the memory doctrine.

## Row grammar

```text
- #<id> <YYYY-MM-DD> <type>/<topic>[!] <sentence> [-> <link> ...] ^m<id>
```

- `id`: positive integer, strictly increasing per home, never reused. The
  block id is always `^m<id>` and always the last token.
- `type`: one of `user | feedback | project | reference`. `topic`: a
  kebab-case tag, one word or hyphenated (`git`, `api-data`, `sim`).
- `!` directly after the topic pins the row: rendered first, never ages out.
- `sentence`: one line, one fact, plain hyphens only (no `—`/`–`), ends with a
  period, at most 200 characters (warning above 160).
- A sentence starting `supersedes #N: ` marks row N superseded; `retires #N.`
  (optionally followed by a reason) marks it retired with no replacement.
  Both are rows like any other; the old row is never edited.
- Links after `->`, space separated, any mix of: `[[notes/<slug>]]` (a detail
  note in this home), `[[LEDGER#^m<id>]]` (a row in this home),
  `[[<alias>/LEDGER#^m<id>]]` or `[[<alias>/notes/<slug>]]` (another home),
  `https://...`.

Example:

```text
- #45 2026-09-20 feedback/git Never `git stash`; parallel sessions share the tree. -> [[notes/git-stash-race]] ^m45
- #46 2026-09-20 reference/macos! AX exposes only the current Space; an off-Space frame() hangs until timeout. ^m46
- #61 2026-09-21 feedback/git supersedes #45: stash is fine inside `.worktrees/`. ^m61
```

`LEDGER.md` starts with a two-line frontmatter (`memo: 1`, `alias: <alias>`)
and a one-line comment naming the grammar; everything else is rows.

## Rendered MEMORY.md

Deterministic output of `memo render`. Header: for a personal home the
routing line `Team memory: <repo>/.claude/memory/MEMORY.md` plus a two-line
legend ("cite a row as #NN when you act on it; `memo show NN` for detail;
`memo find <words>` searches the whole ledger"). Then `## Pinned` and
`## Hot` sections; rows are printed in the exact ledger grammar so a citation
copied from either file is identical. Hard cap 100 lines including header;
rows past the cap stay in the ledger only.

Score per row: sum over its usage events of `weight * 0.5^(age_days/90)`,
weights `cite 1`, `show 1`, `read 1` (a Read of a linked note), `touch 2`.
Order: pinned, then score descending, then newest id first. A row created
more than 180 days ago whose newest event is more than 180 days old is not
rendered. Superseded and retired rows are never rendered.

## Verbs (cli-craft envelope on every path)

All verbs take `--home <alias|path>` (default: resolved from cwd -> repo ->
personal slug + team home; a verb that needs one home picks personal for
`user`/`feedback` rows and team for the rest) and `--json`.

- `memo add <type>/<topic>[!] "<sentence>" [--link <l>]... [--supersedes N] [--retires N]`
  appends the row, renders, snapshots the personal home. Refuses a long dash,
  a second sentence, a missing period, a type that does not belong in that
  home, an unknown supersede target; every refusal names the fix.
- `memo show <ref>` prints the row and the content of its linked notes; `ref`
  is `45`, `#45`, `fixit-team#12`, or a wikilink. Records a `show` event.
- `memo find <words>...` searches sentences and topics across the resolved
  home (`--all` for every registered home), superseded rows included with a
  marker.
- `memo touch <ref>` records a `touch` event.
- `memo render [--check]` writes `MEMORY.md`; `--check` exits 2 when the
  file on disk differs (CI door).
- `memo import <file>` reads rows without ids (same grammar minus `#id` and
  `^mid`), assigns ids in order, appends, renders. Migration door.
- `memo homes [--register <alias> <path>]` lists homes: auto-discovered
  (every `~/.claude/projects/*/memory` and every `<repo>/.claude/memory`
  with a `LEDGER.md`), alias = repo basename lowercased, team home =
  `<alias>-team`; overrides in `~/.config/vybava/memo/homes.json`.
- `memo vault [--path ~/Memory]` maintains an Obsidian vault of symlinks
  `<alias> -> <home>` plus a minimal `.obsidian/app.json`, idempotent.
- `memo snapshot [-m <msg>]`, `memo log [-n N]`, `memo restore <rev> <file>`:
  the personal home is a git repo (`git init` on first snapshot, never a
  remote). In a team home these say the repo's git owns it and exit 0.
- `memo hook` reads a Claude Code / Codex hook payload on stdin:
  - PreToolUse: refuses Edit/Write/heredoc/`sed -i` on `LEDGER.md`,
    `MEMORY.md`, `usage.jsonl` in any home (exit 2, stderr names
    `memo add` / `memo render`); notes/ files stay under memorylint's rule.
  - Stop / SessionEnd: opens `transcript_path`, collects citations
    (`#NN` with word boundaries, `^mNN`, `[[...LEDGER#^mNN]]`) from assistant
    text and tool inputs, Read tool calls on `<home>/notes/*.md`, and
    `memo show` bash commands; appends events deduplicated per
    (row, kind, session); re-renders when anything changed. Only ids that
    exist in the home count.
- `memo migrate <home>` (helper, not the judgment): lists v2 notes, prints a
  template `import` file with one empty row per top-level bullet.

Exit codes: 0 ok, 1 infra, 2 diagnostics. Diagnostics are a closed enum
(`docs/memo.md` lists them). `next` carries the exact follow-up command.

## memorylint changes

`memorylint check <home>` on a home with `LEDGER.md`: row grammar, ids
strictly increasing and unique, block id equals `#id`, supersede/retire
targets exist and are not already superseded, links resolve (notes/, rows,
registered aliases), no long dashes, sentence length, `MEMORY.md` equals
`memo render` output (error), notes/ files not linked from any row
(warning), the personal note ceiling (15) applies to `notes/` only. Existing
v2 homes without a `LEDGER.md` lint exactly as before.

## Doctrine and hooks (Claudik, `~/.claude`)

- `skills/my/memory/SKILL.md`: rows replace the note template as the default
  capture; a detail note only when the narrative is real; cite `#NN` when a
  row changes what you do.
- `settings.json`: `memo hook` on PreToolUse (Edit/Write/Bash) and Stop.
- Global `CLAUDE.md` memory routing line points at `memo`.

## FixIt

- `.claude/memory` converted to a ledger (`memo import`), notes kept only
  where the narrative earns it; `memory-hygiene.yml` adds `memo render --check`.
- Personal home converted the same way, then `memo snapshot`.

## Amendment (2026-09-20, round 5)

- A TEAM ledger's ids carry a `t` prefix everywhere: `- #t12 <date> ... ^t12`,
  cited as `#t12` / `[[LEDGER#^t12]]`, referenced as `memo show t12`;
  `supersedes #t3:` / `retires #t3.` inside a team ledger. Personal ids stay
  bare `#12` / `^m12`. A session has two homes, so a bare id credits the
  personal ledger only and a `t` id the team ledger only; alias-scoped forms
  are unchanged. Lint: the block id must match the row id including prefix,
  and a ledger holds only its own kind's ids.
- From any cwd inside a repo (main checkout or linked worktree) the personal
  home is `~/.claude/projects/<slug of the MAIN worktree>/memory`; the team
  ledger read and written is `<this checkout>/.claude/memory` (the branch
  being edited), while `memo homes`, the registry, alias derivation and the
  vault always name the main worktree's `.claude/memory`.

## Amendment 2026-09-21: no row date

- Rows carry no date. The grammar is
  `- #<id> <type>/<topic>[!] <sentence> [-> <link> ...] ^m<id>` (team:
  `#t<id>` / `^t<id>`); the examples above predate this and read with the
  date removed.
- Creation time moves to `usage.jsonl`: `memo add` and `memo import` write
  one `{"row":N,"kind":"add","at":...,"session":...}` per new row (an import
  stamps every row with the same timestamp). `add` weighs 0 in the score.
- The 180-day aging rule reads the row's `add` event; a row without one (a
  ledger written before this amendment) is treated as created now, so legacy
  ledgers never vanish.
- `memo import` accepts a row with or without the leading date and drops it
  when present, so draft files written under the old grammar still import.
- The PreToolUse guard also reads `apply_patch` delivered through the shell
  tool (`apply_patch <<'PATCH' ...` or `bash -lc "apply_patch ..."`) and
  counts a `*** Move to:` destination as a target.
