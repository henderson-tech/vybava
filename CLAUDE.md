# Výbava

FixIt Technologies' modular distribution hub for small engineering utilities,
reusable agent skills, and workstation diagnostics. Per-tool references live
in `docs/`; lessons in `.claude/memory/MEMORY.md`.

## Architecture laws

- `catalog/catalog.yaml` is the package and group source of truth. One item
  owns one capability; groups only compose item IDs — group behavior is never
  hard-coded into the CLI. Adding a package = its payload + one catalog entry;
  a preset membership is one more catalog line.
- The `vybava` binary is multicall: installed applets are links dispatching by
  `argv[0]`. Implementations stay in focused `internal/<id>/` packages; CLI
  wiring carries no domain logic.
- Agent skills have ONE canonical `SKILL.md` under `skills/<id>/`; installer
  adapters copy it into Claude Code or Codex homes — never per-agent forks.
- Human-readable output is the default; every automation-facing command must
  support stable `--json` and non-interactive execution.
- Tagged releases publish a Homebrew cask via the tap deploy key — read
  `docs/homebrew.md` before touching release distribution.
- `ci/` is the ONLY pipeline-facing surface: CI images, workflows and
  provisioning scripts install a tagged release through `ci/install.sh` and
  never check this repository out (`scripts/`, `skills/`, `docs/` and the Go
  sources are internal). Contract + consumers: `ci/README.md`; tests in
  `internal/ciinstall`. Pins move only after a release is cut.
- The repo-root `Dockerfile` is the luko.to redirector image (deployik app
  `luko`, build context = Dockerfile's directory, so it must stay at the
  root). `internal/shrt/rules.go` ships in BOTH the CLI and the server —
  changing it means redeploying luko. Ops details: `docs/shrt.md`.

## Commands

```sh
go test ./...  &&  go vet ./...
go run ./cmd/vybava catalog list
go run ./cmd/vybava doctor
```

Run `go fmt ./...` after Go edits. Utilities are Go — never Python helpers.

Run verification remotely with `devbox run verify` using `devbox.yaml`. Its
`repo` app holds the CLI test workspace open; its reserved port serves no UI.
Exception: Lukáš authorized local verification for the operator trial on
2026-09-06; Devbox capacity must not block that trial.
`internal/codexsync/storage.go` owns codexsync's destination validation,
atomic file writes, and empty-directory cleanup; rendering stays in
`internal/codexsync/codexsync.go`. See `docs/codexsync.md` for ownership rules.

`internal/operator` owns the local Codex operator trial: incremental Claude/Codex
observations, durable delivery receipts, revision-bound proposals and actual
human scores. `docs/operator.md` documents its CLI and the human-only send rule.
`internal/operator/companion.go` owns the native companion snapshot and free-text
feedback contract; source scan time is separate from snapshot read time.
`history.go` owns stable observation pagination and immutable, revision-bound
human review decisions. Approving a draft records review only; it never executes.
`sqlite.go` owns explicit JSON-to-SQLite migration, indexed event/history queries
and content revisions independent of heartbeat metadata. The v5 JSON marker blocks
legacy writers after migration; preserve the private pre-migration archive.
`outcome.go` records execution/verification evidence separately from approval.
`messages_reader.go` owns read-only SQLite metadata and pinned imsg content reads;
`messages.go` owns baseline, changes, removal, retry and per-source coverage.
Agent scan time is independent of Messages check time. All trial writers must be
upgraded together before a live v4 write; see `docs/operator.md`.
Production operator reads/writes use scoped `Store` APIs (event, queue, metadata,
history, snapshot, revision). `View`/`With` retain full-archive compatibility for
explicit export/tests; do not use them in polling paths.
`Store.Scan` serializes source scans separately and merges observations/cursors
under the writer lock, preserving concurrent feedback. `attention.go` owns the
conservative, freshness-gated Claude attention selection; the CLI only wires it.

`internal/codexusage` reads `~/.codex` rollouts read-only and attributes plan-limit
spend per Codex process. Two accounting rules are load-bearing and documented in
`docs/codexusage.md`: `cached_input_tokens` is a SUBSET of `input_tokens` (never sum
them), and a `token_count` event repeating an unchanged `total_token_usage` is a
rate-limit refresh, not a call — recorded unbilled so its percentage survives without
double-billing. `rollout.go` owns parsing and the mtime prefilter; `live.go` owns
`ps`/`lsof` enrichment and must always degrade to a warning.

`internal/transcripts` owns reading agent logs: the incremental cursor (offset, size,
mtime, prefix digest; never a partial last record), Claude transcript and Codex
rollout decoding, the projects-tree walk and git-root resolution. operator and
codexusage read through it; never add a fourth parser. `internal/tokentime`
builds on it: buckets are permanent (transcripts are deleted, totals must not
shrink), every response is counted once through the `seen` identities committed
in the same transaction as buckets and cursors, and the rollup JSON is a contract
with claude-switcheroo (`src/arcade/contract.ts`), the beats JSON with its
timesheet (`src/timesheet/contract.ts`). Beats (per-minute human/ai presence)
backfill through a beats-only backlog read that never charges. Rules: `docs/tokentime.md`.

`internal/plaud` reads the Plaud account directly (PKCE login, vault-injected
refresh token, cached access token only); the manual-only skill is
`skills/plaud/`. `docs/plaud.md` has the auth model and the API map.

`internal/vconfig` loads the shared per-repo `vybava.config.ts` (bun-evaluated, cached by mtime beside the git dir) or `vybava.config.json`; applets read their section through `Config.Section` with unknown fields rejected. The TypeScript helpers are embedded (`config-helpers.ts`) and drift-checked by `vybava config check`. `internal/lok` owns locale catalogs (order-preserving JSON, alphabetical inserts, per-locale parity); `internal/claudeguards` refuses raw reads of configured catalogs. `internal/configdiscover` proposes `guards.noRead` and `lok.catalogs` from the tracked tree and backs `config check`'s drift warnings; it lives outside `vconfig` to keep a `vconfig → lok → vconfig` cycle from forming, only ever fills sections absent from the config, and is advisory — tracked files only, so a gitignored generated tree is invisible to it. Docs: `docs/config.md`, `docs/lok.md`.

`internal/claudeguards/input.go` owns the ONE definition of "what commands does
this string run" — `splitShell` (quote-aware), `trimAssignments`,
`runnerPayloads`, reached through `segments()`. Every rule family goes through
it; never re-derive segmentation locally. The 2026-09-14 field audit found five
rule families each doing their own, which let quoted text be scanned as
commands in 18 of 25 rules and let a bare `FOO=1` prefix disarm 9 — hard bans
included. Quoting asymmetry is load-bearing: single quotes suppress everything,
double quotes suppress control operators but NOT `$(…)`/backticks. Findings and
the settled "do not re-litigate" list: `docs/decisions/0004-guard-field-audit.md`.

`internal/plugingc` garbage-collects the Claude Code plugin cache. Three rules
are load-bearing and documented in `docs/plugin-gc.md`: the active version
comes from `installed_plugins.json` compared BY PATH — never by sorting version
strings, which puts `3.11.0` above `5.3.0` — a `.in_use` marker is dead only
when proven so (PID gone, PID held by something that cannot be a session, or a
process that started AFTER the marker's own mtime), and `claude plugin
uninstall` DELETES NOTHING: it writes `.orphaned_at` and leaves the tree, so an
uninstalled plugin's cache outlives both it and its marketplace.
`kill(pid, 0)` succeeding proves nothing; PIDs are recycled. Everything
undecidable is held, and destruction only ever happens behind `--apply`.
`internal/claudeguards/plugincache.go` is the other half: it blocks package
installs into that tree at all, and is deliberately escape-hatch-free.

`internal/mergeassist` settles mechanical merge conflicts; the catalog driver is
lok's (`internal/lok/merge.go`). `mergeassist/gitmerge` is the dependency-free
leaf both drivers share (journal, text-merge fallback) so lok never imports
mergeassist. Load-bearing: drivers are registered in the clone's
`info/attributes` + git config, never a tracked `.gitattributes` (git reads
attributes from the checked-out branch); a catalog key clash is a conflict even
when git's line merge is clean (it keeps duplicate keys); a migration the base
has is never renamed. Docs: `docs/merge-assist.md`.

`internal/gitkit` is the git family's deterministic layer: skills call
`vybava gitkit <script>`, never a file path; a verb's argv, stdout (JSON key
order), stderr and exit code are the contract, byte-identical to the Node
scripts it replaced. Shell out, emit JSON and parse numbers only through
`native.go`'s Node-compatible helpers (`execFile`, `writeJSON`, `jsString`,
`jsNumber`) — `docs/gitkit.md`. The git-family skills are canonical HERE; a
personal `~/.claude` copy is a symlink, never a fork.

`internal/readiness` is the `release-readiness` skill's deterministic layer. It renders
`skills/release-readiness/templates/` from the embedded payload, so a rule change
is a template edit and never a Go string. The run directory is seeded with COPIES
of the skill's scripts: skills never reference their own install path, which
differs between Claude and Codex. `init` never overwrites run.json, a ledger or
a copied script. `slot` and `uniq-shots.sh` are workarounds with named retirement
conditions (`docs/readiness.md`).

`internal/toolsetup` owns catalog `tool` items: probes are live (never Výbava
state), install goes through the product's own channel, and credentials never
pass through Výbava — guided steps run with a terminal or come back as `next`.
A pultik artifact is placed only after its sha256 matches the shelf.

`internal/envbridge` provides bounded, memory-only environment transfer over a
private Unix socket. It never fetches vault values or executes shell exports;
the injecting wrapper and consuming process own those boundaries. See
`docs/envbridge.md` before using its sensitive read output.
