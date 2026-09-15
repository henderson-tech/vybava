# plugin-gc — garbage-collect the Claude Code plugin cache

Claude Code keeps **every plugin version it has ever installed** under
`~/.claude/plugins/cache/<marketplace>/<plugin>/<version>/` and refcounts each
one with PID marker files in `<version>/.in_use/`. A version is reclaimable
only when no marker remains — and an abandoned `claude` process holds its
marker forever. Nothing prunes this. It only grows.

Measured on one dev Mac when this was written: **18 cached versions of a
single plugin, 8.4 GB**, with live markers reaching back six minor versions
and 172 `claude` processes on the machine. Of the ~480 MB each version held,
**478 MB was `node_modules`** — a bun workspace install, down to a
`hermes-compiler` carrying win64 DLLs. The surface the plugin loader actually
reads (`skills/`, `agents/`, `.claude-plugin/`) was **~520 KB**.

```sh
plugin-gc                         # what would be reclaimed, nothing touched
plugin-gc --plugin vitrinka       # narrow to one plugin
plugin-gc --apply                 # sweep dead markers, strip, remove
plugin-gc --apply --only strip    # the safe, high-yield move alone
plugin-gc --apply --skip remove   # never delete a version directory
plugin-gc --orphan-grace 720h     # keep uninstalled plugins' caches 30 days
plugin-gc --json                  # stable report for agents
plugin-gc --home /tmp/fixture     # operate on a cache that is not the live one
```

**Reporting is the default.** Without `--apply` nothing is deleted, ever.

## The three moves

Ordered by how little each can possibly break. `--apply` runs all three;
`--only` / `--skip` filter them by id, the way `reclaim` filters its steps.

| Move | What goes | Why it is safe |
|---|---|---|
| `sweep` | `.in_use/<pid>` markers whose PID is **proven** dead | the session behind them is gone; the marker is the only thing keeping a version alive |
| `strip` | `node_modules` trees under an **inactive** version | the loader reads `skills/`, `agents/`, `.claude-plugin/`; `node_modules` is build residue — ~99% of the bytes, zero markers touched, no running session disturbed |
| `remove` | an inactive version directory with **no live marker**, and an **orphaned** one | nothing references it, at any scope |

A run sweeps before it removes, so a version freed by the sweep is reclaimed
in the same pass. A version that `remove` takes is never also counted as a
strip — the totals never double-count.

**`sweep` is per-marker; `strip` and `remove` are per-version.** A proven-dead
marker is dead whatever plan its version carries, the active one included —
that is the refcount going honest, and it is what lets today's active version
be reclaimed the day it rolls over. Only a version's *content* is untouchable.

## The active version is read, never guessed

The live version comes from `installed_plugins.json`:

```json
"plugins": { "<plugin>@<marketplace>": [ { "installPath": "…/5.3.0", "version": "5.3.0" } ] }
```

Every listed `installPath` is active (a plugin installed at both user and
project scope has more than one) and is compared **by path**, never by version
string. Sorting version strings is exactly the bug that made this tool
necessary: lexically, `3.11.0` outranks `5.3.0`.

If `installed_plugins.json` cannot be read the run **refuses** rather than
guess. If it parses but names **no plugins while the cache holds versions**,
that is not a machine with no plugins — it is a record this build cannot read
(a renamed field, a schema bump, a truncated file), and taking it at face
value would mark the version in use `stale` and delete it. The report still
prints; `strip` and `remove` are disabled with a warning.

`~/.claude/plugins/marketplaces/` is never touched at all.

## When is a marker dead? (the correctness trap)

`kill(pid, 0)` succeeding proves **nothing** — it succeeds for any live
process, and macOS recycles PIDs freely. Getting this wrong un-refcounts a
running session. Three verdicts count as dead, and every one of them is proof:

- **gone** — no process holds the PID.
- **foreign** — a process holds it, but its executable could not be a Claude
  Code session. The test answers *generously*: anything whose name contains
  `claude`, plus the JS runtimes Claude Code has been hosted by (`node`,
  `bun`, `deno`, `npx`, `bunx`), is treated as possibly-a-session. A browser
  or a daemon on that PID is a recycled PID with certainty.
- **recycled** — a plausible process holds the PID but **started after the
  marker file was written** (2 minutes of grace for clock jitter). A marker
  cannot predate its own writer, so a later start proves this is not that
  process. The marker's own `procStart` field is reported but not trusted for
  the verdict — it is written in a different timezone than `ps` reports, while
  the file's mtime needs no timezone to be true.

Everything else — including a marker file whose name is not a PID at all — is
**held**, and holds its version. The tool under-claims on purpose: a missed
marker costs disk, a wrongly swept one breaks a live session.

A version with **no `.in_use` directory at all** simply has no markers — never
an error. A freshly installed version can look like that; a busy one carries
dozens (43 live on the active version when this was written).

If the process table cannot be read at all, `sweep` and `remove` are disabled
for the run and a warning goes in the report; the scan still prints.

## Orphans — uninstall does not delete

`claude plugin uninstall` **removes nothing**. It writes a `.orphaned_at` file
into the version directory — milliseconds since the epoch — and leaves the
whole tree in place. The cache outlives both the plugin and its marketplace,
so an uninstalled plugin keeps its bytes forever.

A version is `orphaned` when it carries that stamp **and its plugin is no
longer installed at any version**. `remove` takes the whole directory, and its
bytes are reported apart from ordinary removals so the dry run can say *"this
is not merely unreferenced — the plugin is gone"*.

Because an uninstall is often a mistake found the same day, an orphan is kept
for `--orphan-grace` (default **7 days**) and reported as `keep` with the
reason naming the grace. A stamp on a version of a plugin that *is* still
installed only means that version was superseded — that is `stale`, never an
orphan, and the stamp is still reported.

When a plugin's last version leaves, its now-empty directory goes too
(`os.Remove`, which refuses a non-empty directory).

## Stripping a held version

An inactive version a live session still holds keeps its directory but loses
`node_modules` — that is where nearly all the bytes are, and the loader never
reads it. One exception: if the version's `.claude-plugin/plugin.json` or
`.mcp.json` mentions `node_modules`, the version could execute an entry point
inside the tree, so it is reported as `keep` with the reason and left alone.

Nested `node_modules` (a bun workspace nests `node_modules/.bun/node_modules`)
are sized once — a found tree is measured whole and not descended into.

## Plans in the report

| Plan | Meaning |
|---|---|
| `active` | `installed_plugins.json` points here — content never touched |
| `held` | inactive, a live session holds it — `strip` only |
| `stale` | inactive, no live marker — `remove` takes the whole directory |
| `orphaned` | the plugin is uninstalled and the grace has passed — `remove` takes it |
| `keep` | the scan refused to judge it (incl. an orphan inside its grace); reported, never touched |

## JSON and exit codes

`--json` emits the whole report: home, dry-run flag, live session count, every
plugin with its active versions, every version with its plan, reason, size,
markers (PID, liveness, what holds it, recorded start), `node_modules` trees,
the strip/remove byte split, the dead-marker count, what actually left the
disk, per-path outcomes and warnings. Exit is 0 even when an individual
deletion fails — the failures are in the report.

Implementation: `internal/plugingc` (`plugingc.go` owns the scan, the plans
and the moves; `procs.go` owns the process table, the marker verdict and the
install record). CLI wiring in `internal/cli/plugingc.go`.
