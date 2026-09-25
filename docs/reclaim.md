# reclaim — emergency disk reclaim

When a dev Mac hits a full disk, processes start crashing within minutes.
`reclaim` exists for exactly that moment: it frees the most space in the
fewest seconds, visibly, without asking anything and without scanning first.

```sh
reclaim                  # everything reversible, biggest-first (tiers 1–3)
reclaim --until 100G     # stop the ladder as soon as 100G is free
reclaim --tier 1         # only the huge build/package caches
reclaim --dry-run        # size every step, delete nothing
reclaim --list           # print the ladder
reclaim --only go-build,docker-builder
reclaim --skip trash --keep-days 30
reclaim --json           # stable report for agents
reclaim bun-prune <checkout>           # report one checkout's unreachable .bun dirs
reclaim bun-prune <checkout> --apply   # delete them (background priority)
```

## How it works

A **fixed ladder** of steps, each deleting something that regenerates on its
own. No `du`, no classification pass — the first delete starts immediately.

- Steps in a tier run **concurrently**; each prints the moment it finishes
  with the volume's free space at that instant, so partial wins land while the
  slow ones (Docker prune) are still working.
- **`--until`** checks free space after every finished step and cancels the
  rest of the ladder once the target is met.
- "Freed" in the summary is the **df delta**, never a sum of `du` figures —
  APFS clones and hardlinks make per-tree sizes lie. Per-step bytes are the
  logical size seen during deletion (rm walks the tree anyway) or what the
  tool itself reports (Docker's "Total reclaimed space").
- Aged steps (`messages-tmp`, `sandbox-tmp`) delete only files older than
  `--keep-days` (60) and never the tree — a warm media cache is not garbage,
  purging it just costs an iCloud re-fetch and a slow app.
- A missing tool (`docker`, `pnpm`, `xcrun`, `brew`, `pwmcp`) skips its step;
  a failing one reports and the ladder continues.

## The ladder

| Tier | Step | What goes | Comes back via |
|---|---|---|---|
| 1 | `go-build` | `~/Library/Caches/go-build`, `~/.cache/go-build` | next `go build` |
| 1 | `docker-builder` | `docker builder prune -af` | next build |
| 1 | `docker-images` | `docker image prune -af` (unreferenced only) | re-pull / rebuild |
| 1 | `bun` | `~/.bun/install/cache` tarballs, index dirs, `*.npm` manifests; never `links/` (skips while a bun install runs) | next install re-downloads what it newly materializes |
| 1 | `npm` | `~/.npm/_cacache`, `~/.npm/_npx` | next install / npx |
| 1 | `derived-data` | `~/Library/Developer/Xcode/DerivedData/*` | next Xcode build |
| 1 | `gradle` | `~/.gradle/caches` | next gradle build |
| 1 | `pnpm` | `~/Library/Caches/pnpm` + `pnpm store prune` | next `pnpm install` |
| 1 | `cargo` | registry cache, git checkouts | next `cargo build` |
| 1 | `py` | pip / uv caches | next install |
| 2 | `tool-caches` | playwright-mcp, CocoaPods, dotslash, claude, codex, copilot, composer, yarn, turbo, nx, JetBrains | next use |
| 2 | `browser-caches` | Brave, Chrome, Spotify caches | next launch |
| 2 | `playwright` | `pwmcp prune` — unpinned browser revisions only | nothing |
| 2 | `brew` | `brew cleanup -s` + `~/Library/Caches/Homebrew` | next `brew install` |
| 2 | `maven` | `~/.m2/repository` | next `mvn` build |
| 2 | `logs` | JetBrains, CreativeCloud, rotated `*.log.old.*` | nothing |
| 2 | `xcode-caches` | Xcode + CoreSimulator caches | next run |
| 2 | `sim-unavailable` | `simctl delete unavailable` | nothing |
| 3 | `device-support` | iOS / watchOS / tvOS DeviceSupport symbols | re-sync from a plugged device |
| 3 | `sim-runtimes` | simulator runtimes no device uses | re-download via Xcode |
| 3 | `sim-logs` | per-sim diagnostics logs (shuts sims down; apps + data survive) | nothing |
| 3 | `messages-tmp` | Messages sandbox tmp, files older than keep-days (quits Messages) | iCloud re-fetch |
| 3 | `sandbox-tmp` | every app sandbox tmp, aged slice only | app re-creates |
| 3 | `trash` | `~/.Trash` | nothing |

Measured on a working Mac when this was written: go-build 77G, Docker build
cache 24G, bun 17G, DeviceSupport 17G, npm 8G, gradle 5G — none of them in a
named-bucket cache list until someone ranked by size.

Adding a step is one entry in `internal/reclaim/ladder.go` (ID, tier, what
regenerates it, paths or a run func) plus a row here.

## bun: the global store is never touched

With `globalStore = true` in a bunfig (FixIt sets it), every checkout's
`node_modules/.bun/<pkg>` is an absolute symlink into
`~/.bun/install/cache/links/<pkg>-<hash>`. Until 2026-09-25 the `bun` step
deleted `~/.bun/install/cache` whole, which dangles every such checkout at
once; the repair is one `bun install` per checkout, the install storm a full
disk can least afford. The step now deletes only the cache root's other
entries: extracted tarballs (`<pkg>@<ver>@@@N`), the per-name index dirs of
version symlinks beside them (also under `@scope/`) and the `*.npm`
manifests. Dot-entries (bun's staging) and `links/` stay.

Why deleting the extracted dirs is safe, measured read-only on 2026-09-25:

- all 8,337 dependency symlinks inside `links/` resolve to a `links/` entry
  (7,619 to a sibling, 718 within their own entry, none outside it), so
  nothing in the store points into an extracted dir;
- sampled `links/` files have their own inode and a link count of 1, the
  same size as the extracted file: APFS clones, not hardlinks or symlinks;
- in a throwaway store (`cache = "/tmp/…"` in a probe bunfig), deleting the
  extracted dirs left the checkout resolving, a frozen install reported
  "no changes", a reinstall into an empty `node_modules` reused `links/`
  without re-extracting, and a `links/` entry deleted on purpose was
  downloaded again rather than left broken.

The cost is re-downloads, not breakage: the next install that materializes a
NEW `links/` entry, or a project-local package (patched and trusted ones are
cloned into the checkout, 27-28 per FixIt create), fetches that tarball again.

Two guards against an install running beside the step. It skips (status
`skipped`, with the pid) while any bun install-family process or `bunx` is
alive; rerun `reclaim --only bun` when it finishes. And every target is
renamed into `~/.bun/install/.reclaim-trash-<pid>/` before it is deleted, so an
install that starts mid-step sees a whole tarball or none, never a
half-deleted one it would clone into a `links/` entry every checkout shares. A
trash dir an interrupted run left is deleted by the next one.

## reclaim bun-prune: one checkout's stale .bun entries

bun never deletes a `node_modules/.bun` entry the lockfile stopped naming, and
a linker switch leaves `node_modules/.old_modules-<hash>` behind: FixIt's main
checkout held 3,939 `.bun` entries against ~130 real dirs in a fresh worktree,
plus a 5.2 GB `.old_modules`. The ladder never scans; this verb does, because
an entry is garbage only when nothing in the checkout resolves to it.

```sh
reclaim bun-prune ~/Work/app              # report: unreachable dirs + leftovers, sizes
reclaim bun-prune ~/Work/app --apply      # delete them
reclaim bun-prune ~/Work/app --json       # stable report
```

The walk starts at the roots (the checkout's `node_modules`, each workspace
package's `node_modules` from the root `package.json` `workspaces`, and
`node_modules/.bun/node_modules`, bun's fallback for undeclared imports). A
symlink there that lands in `.bun/<entry>` marks the entry; a marked entry that
is a real directory (a project-local package) adds its own `node_modules`
links, transitively. A marked entry that is a symlink into `links/` ends the
walk, since `links/` only points at `links/`. An unreadable root, or a
workspace match that exists but cannot be stat'ed, is an error, never a skip,
and a `**` workspace pattern refuses rather than risk a missed root.

Only unreachable real directories (project-local packages, where the bytes
are) and leftovers are deleted. Unreachable symlinks into `links/` are
counted, never deleted: bun links every lockfile package into `.bun`, so a
worktree installed minutes earlier already held 569 of them (of 3,245
entries) next to zero unreachable directories. Each frees nothing but a
directory entry, and the next install would relink it. That fresh-worktree
run is also the walk's check: every project-local directory bun had just
made was reached.

A native project is a root of its own. An iOS Pods project compiles every
development pod from the `.bun` entry its `ios/Podfile.lock` named at
`pod install` time, and nothing but the next `pod install` re-points it. So
after the node_modules walk, each package directory's `ios/Podfile.lock` is
read and every entry it names, plus what those entries link to, is kept
(`kept_native`, sized apart in a dry run) and never deleted. FixIt's main
checkout on 2026-09-25: a Podfile.lock from 2026-08-03 named 59 entries,
7.7 GB, none of them reachable from node_modules any more. Run `pod install`
(or delete a prebuild-generated `ios/`), then prune again to release them.
Android keeps no such file: `settings.gradle` resolves the modules with
`node --print require.resolve(...)` at every Gradle configure.

Sizes count hardlinks once; APFS clones still share blocks, so the `df`
delta `--apply` prints is the truth. `--apply`:

- refuses while a bun install-family process has its cwd inside the
  checkout, or names a directory inside it with `--cwd` (`bun
  --cwd=~/Work/app install` started in `$HOME` writes the checkout while lsof
  reports `$HOME`; a relative value resolves against the process cwd, a
  symlinked one is also compared resolved, and since ps joins argv with
  spaces, a longer space-joined run that names an existing directory counts
  too), checked before the walk and again right before the first delete. One
  `lsof -c bun -d cwd` finds the cwds, compared case-folded
  (APFS keeps a typed `~/work/app` spelling that lsof reports as
  `~/Work/app`), and one `ps -axo pid=,args=` reads their command lines
  through the same parser as the bun step's guard. Only a writer counts:
  `install`/`i`/`ci`, `add`/`a`, `remove`/`rm`/`uninstall`, `update`,
  `upgrade`, `link`, `unlink`, `pm`, `patch`, `patch-commit`, `init` and
  `create`/`c`, after any global flags, and a bun ps no longer lists, whose
  command line is unknown. bun takes the first word without a dash as its
  verb (`bun --cwd=x install` installs in x, `bun --cwd x install` runs bunx
  on a package named `install`); the parser also lets each flag take one
  value, so it can over-count a writer, never miss one. Any other bun there (a
  dev server, `bunx`, a script such as the `switcheroo start` session
  launchers that live for days with their cwd in FixIt's main checkout) only
  resolves modules, and resolution from the checkout reaches only reachable
  entries, which are never pruned. A long-lived process that loaded from an
  entry before an install made it unreachable can still fail a lazy
  `require` into it once it is deleted; a restart picks up the current
  tree. A cwd inside a checkout
  nested in it (a directory with its own `.git` file or dir, like a worktree
  at `.worktrees/<slug>`) does not count when bun treats it as its own
  project: the nearest `package.json` at or above the cwd lies inside the
  nested checkout and is not one of the checkout's workspace packages. Such a
  process installs into its own `node_modules`, and one resolving modules by
  walking up into the checkout's `node_modules` (a worktree not yet installed)
  only lands on reachable entries, which are never pruned. A nested checkout
  bun would resolve to the pruned root (no `package.json` of its own, or a
  submodule the root's `workspaces` list) still refuses;
- refuses when anything the plan read changed before the first rename: the
  `.bun` listing, the root `package.json`, each workspace pattern's literal
  parent directory (`apps/` for `apps/*`), every `node_modules` directory
  walked and every `Podfile.lock`. An install that started and finished
  during a throttled walk passes both busy checks, but adding or replacing a
  store entry, relinking a root, editing the workspace list or adding a
  package directory each moves an mtime there;
- keeps every candidate modified within `--min-age` (24 h), where an install
  in flight writes;
- renames each target into `node_modules/.bun-prune-trash-<pid>/` before
  deleting it, so a later install never finds a half-deleted package under a
  name it trusts; an interrupted run's trash shows up as a leftover next time;
- runs in the Darwin background band (what `taskpolicy -b` does), so deleting
  hundreds of thousands of files yields to interactive work. The dry run does
  not: on a loaded Mac (load ~250 on 14 cores) the band's I/O throttle kept a
  read-only walk of a fresh worktree under half a second of CPU in two minutes.
  Expect `--apply` itself to crawl on such a machine; run it in a quiet window.

The global store is never touched: a `links/` entry no checkout references
stays until bun itself drops it.

## What it refuses to touch

- **Docker volumes and containers.** A dangling volume may be a paused
  worktree's database; the owner is a compose project *name*, not a path.
  The run prints the reclaimable volume size as a by-hand note pointing at
  the audit flow that classifies ORPHAN vs MOVED and deletes by name.
- **Unsaved screen recordings** — user data; listed with sizes, opened by
  hand.
- **`~/Library/Caches/ms-playwright`** — every parallel session would
  re-download and serialize on a silent lock; only `pwmcp prune` goes near it.
- **`Messages/Attachments`** — that *is* the conversation media.
- **`~/.bun/install/cache/links`**: the global virtual store every
  `globalStore` checkout symlinks into (see above).
- Anything not on the ladder. Big and unclassified means user data until a
  human says otherwise.

## Exit codes and JSON

`--json` emits the full report: volume, free before/after, per-step status
(`done` / `dry-run` / `skipped` / `failed` / `stopped`), bytes, seconds, free
space after each step, notes. Exit is 0 even when a step fails — a half-freed
disk is still the goal met; failures are in the report.
