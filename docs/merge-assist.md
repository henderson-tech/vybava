# merge-assist

Merging `main` into a long-lived branch costs a session most of its time on
conflicts that need no judgment: locale catalogs, generated files, migration
timestamps. merge-assist settles those classes through git merge drivers
and prints what is left as one compact table, so a session reads only the
genuine code conflicts, and only their line ranges.

```text
merge-assist merge --dry-run         # preview with git merge-tree, touch nothing
merge-assist merge [ref]             # merge (default origin's default branch), print the table
merge-assist status                  # the table again, after resolving some rows
merge-assist regen                   # run the queued regen commands, stage their output
merge-assist migrations [--apply]    # renumber unmerged migrations the base overtook
merge-assist setup [--check]         # register the drivers in this clone (merge does it too)
```

## The table

```text
merge origin/main into work/x: 13 auto, 5 open
  apps/web/i18n/strings.cs.json                  catalog     auto
  apps/web/i18n/translation-keys.d.ts            generated   auto
  …/migrations/1796300000001-AddX.ts             migration   auto     renumbered from 1796100000001-AddX.ts
  bun run i18n:web:types                         regen       auto     apps/web/i18n/translation-keys.d.ts
  bun run api:generate                           regen       pending  rerun after the open rows: merge-assist regen
  apps/client/locales/cs.json                    catalog     open     2 clashing keys; settle with `lok merge … --prefer ours|theirs`
  apps/api/src/…/pdf.service.ts                  code        open     L71-75 L93-97
```

Settled rows come first, open rows last. A code row names hunk ranges,
never content; a `modify-delete` row says which side deleted. `--json`
carries the whole report. With nothing open, `merge` commits the merge
(`--no-commit` keeps it staged); otherwise it exits `CONFLICTS_OPEN` and the
loop is: resolve, `git add`, `merge-assist regen`, `merge-assist status`,
`git commit --no-edit`.

## The classes

**Catalogs**: every file `lok.catalogs` declares goes to `lok merge-driver`,
a merge by key ([lok.md](lok.md#merging-catalogs)). A key both sides worded
differently is a clash and stays open, even where git's line merge would
have produced a duplicate key without a word.

**Generated files**: each `merge.generated` group lists paths (`**` globs)
and the one command rebuilding them. The driver takes theirs outright and
queues the command; `merge` runs each queued command once after the merge
and stages exactly the files it rewrote (fingerprinted before and after, so
a session's own unstaged edit is never swept in). A command that fails
while code conflicts are open usually compiles the markers; it stays
`pending` and reruns with `merge-assist regen`. Catalog `afterWrite`
commands are queued the same way whenever a merged catalog differs from
theirs.

**Migrations**: `merge.migrations` names directories in the TypeORM shape
(`<ts>-<Name>.ts` holding `class <Name><ts>` and `name = '<Name><ts>'`).
When any migration only this branch has sorts at or below the merged base's
newest, all of the branch's unmerged migrations move, in order, to the first
free multiple of `step` (default 1e8) above that newest, then +1, +2, …
Filename, class name and `name` move together, and so does every other
tracked file naming the migration. A still-conflicted file gets the new name
inside its markers but stays unmerged. Then the repo's own `check` runs, and a
failing check is a `failed` row: the merge is not committed over it. A
migration the base already has is never renamed: its name is recorded in
every database that ran it. A file without the matching `class` is refused
before anything is written.

## Setup is local on purpose

`setup` renders the attribute lines into the clone's `info/attributes` and
registers `merge.lok.driver` / `merge.vybava-generated.driver` in its git
config. Both live in the common git dir, so every worktree shares them, and
nothing is tracked. A committed `.gitattributes` would not work: git reads
attributes from the checked-out branch, so every branch that predates the
block would merge main without the drivers. `merge` and `merge --dry-run`
run setup themselves, so the first merge in a fresh clone already has them.
A plain `git merge` uses the drivers once setup has run in the clone.

The drivers are `vybava lok merge-driver` and `vybava merge-assist driver`,
so `vybava` must be on the PATH git runs with. Each driver resolves the
repo's own config and falls back to git's text merge for any path its
config does not own, so a stale `info/attributes` entry is harmless.

`--dry-run` runs `git merge-tree --write-tree`, which invokes the drivers
too; they journal into a scratch file (`VYBAVA_MERGE_JOURNAL`) instead of
the merge's journal at `<git-dir>/vybava-merge.jsonl`.
