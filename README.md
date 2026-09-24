# Výbava

Výbava is FixIt Technologies' portable engineering environment: small tools,
agent skills, and workstation diagnostics, distributed as one catalog where
every item installs independently.

## Packages

| ID | Kind | What it does |
|---|---|---|
| `memorylint` | applet | Validate and maintain AI memory homes — schema, indexes, wikilinks, fixtures, write hooks. → [docs/memorylint.md](docs/memorylint.md) |
| `claude-guards` | applet | PreToolUse guard hooks for Claude Code — destructive git/docker, secret dumps, host-input automation, commit secrets, whole-disk walks, per-look Appium sessions, uncapped local test runners, and the context-budget rules (no shell-rewritten files, no whole-file dumps, no transcript reads); `doctor` re-wires hooks a settings.json rewrite dropped, `vybava setup mac` applies host settings. → [docs/claude-guards.md](docs/claude-guards.md) |
| `lok` | applet | Locale catalogs for AI sessions — configured in `vybava.config.ts`, queried by key, written by verb with every locale kept in sync, extracted from source, gated in CI. → [docs/lok.md](docs/lok.md) |
| `merge-assist` | applet | Merge main without the mechanical conflicts — catalogs merge by key, generated files take theirs and regenerate, unmerged migrations renumber past the base; one table of what is left. → [docs/merge-assist.md](docs/merge-assist.md) |
| `vybava config` | verb | The shared per-repo `vybava.config.ts` every applet reads — `init` scaffolds it with typed helpers, `check` gates CI, `show` prints the evaluated JSON. → [docs/config.md](docs/config.md) |
| `shrt` | applet | Terminal-safe short links on luko.to — offline repo rules, team-shared dynamic rules, minted codes; also the redirector server. → [docs/shrt.md](docs/shrt.md) |
| `posta` | applet | Drive a shared test mailbox end to end — mint a per-run plus-address, wait for the mail a journey triggered, take its links and attachments. → [docs/posta.md](docs/posta.md) |
| `fontfreeze` | applet | Freeze variable webfonts at rendered axis positions and subset per language. |
| `perfrig` | applet | Performance drills from a `testing/<project>/perf` manifest — ramp to first failure, percentile report. |
| `ingressgen` | applet | Render and drift-check complete default-deny Docker ingress policies from a manifest. |
| `reconcile` | applet | Pull-based GitOps for the infra boxes — converge a VPS to its infra repo's merged main from a per-box manifest: HELD hotfixes, transactional nginx hooks, commit rollback, textfile metrics, mesh-only status page + estate hub. → [docs/reconcile.md](docs/reconcile.md) |
| `readiness` / `release-readiness` | applet + skill | Release prep of a whole integration branch: the skill runs authority questions, an inventory with a completeness critic, one named lane agent per journey cluster (own story, QA task, usertest board), capacity governance, a merged-branch roll-up and a readiness board. The applet validates the `vybava.config.ts` `readiness` adapter, freezes production..integration ranges, and seeds and renders the run directory. → [docs/readiness.md](docs/readiness.md) |
| `prm` / `push-all` / `sync` / `push-back` | skills | The git family — `prm` is the one PR verb (create → review rounds → gated merge → teardown), `push-all` the commit doctrine, `sync` the pull/merge-up flow, `push-back` the claim verifier. Install with `vybava install ai-git`. |
| `gitkit` | applet | The deterministic layer those skills execute — PR selectors, review triage, merge gates, worktrees, path classification — as `vybava gitkit <script>`. → [docs/gitkit.md](docs/gitkit.md) |
| `codexsync` | applet | Render `~/.claude` skills and commands into `~/.agents/skills`, the structure Codex discovers — nesting preserved, each command a `source-command` skill, duplicate discovery suppressed. → [docs/codexsync.md](docs/codexsync.md) |
| `codexusage` | applet | Explain where the Codex plan limit went — per-thread spend from `~/.codex` rollouts, the derived allowance, and the runway left at the measured burn rate. → [docs/codexusage.md](docs/codexusage.md) |
| `tokentime` | applet | Where your AI tokens went — Claude Code transcripts and Codex rollouts indexed incrementally into permanent hour × project × model buckets (worktrees fold into their repo), rolled up by day, hour, project and model with an API-equivalent USD value. → [docs/tokentime.md](docs/tokentime.md) |
| `repolicy` | applet | Hold GitHub repository settings to a declared policy across whole owners — GitHub inherits no organization default, so `audit` reports the drift (exit 1) and `apply` converges it, touching only the settings the policy names. → [docs/repolicy.md](docs/repolicy.md) |
| `menubar-doctor` | applet | Find and fix macOS menu-bar items that run but never appear — since macOS 26 Control Center files a status item under the process that launched the app, so anything started from a terminal is filed under the terminal and stays invisible while its switch is off. → [docs/menubar-doctor.md](docs/menubar-doctor.md) |
| `reclaim` | applet | Emergency disk reclaim for a dev Mac — a fixed ladder of regenerating caches deleted biggest-first with live `df` after every step, an early-stop target, and by-hand notes for what it refuses to touch. → [docs/reclaim.md](docs/reclaim.md) |
| `macwatch` | applet | Mac load sampler with owner attribution - append load, memory and the heaviest processes to a TSV every interval, each tagged with project, directory and the claude/codex session that spawned it; `report` integrates the file into offenders, per-project and per-session totals and a spike ledger. → [docs/macwatch.md](docs/macwatch.md) |
| `plugin-gc` | applet | Garbage-collect the Claude Code plugin cache — every version ever installed is kept behind a PID refcount that abandoned sessions never release, and the payload is almost all `node_modules`; reports by default, deletes only on `--apply`. → [docs/plugin-gc.md](docs/plugin-gc.md) |
| `handoffs` | applet | Handoff ledger upkeep — `handoffs reconcile` judges every open handoff by whether its branches and PRs are still alive and archives the dead ones; unknown is never touched. → [docs/handoffs.md](docs/handoffs.md) |
| `press` | applet | Deterministic state for the document family — project resolution, `~/Exports/<project>/` config and index, ARES lookups, shared doctrine. → [docs/press.md](docs/press.md) |
| `press-pdf` / `press-logo` / `press-offer` / `press-email` | skills | Offer, documentation and legal PDFs; brand marks; Czech commercial DOCX; Outlook-paste client emails. Issuer identity stays machine-local. → [docs/press.md](docs/press.md) |

Groups (`recommended`, `experimental`, `ai-git`, `press-family`, `everything`)
are composable presets in the catalog — never code.

## Install

Homebrew (everything is public — no authentication needed):

```sh
brew install --cask FixIt-Technologies/tap/vybava
```

CI images, workflows and provisioning scripts use the release installer in
[`ci/`](ci/README.md) — never a checkout of this repository:

```sh
curl -fsSL -o /tmp/vybava-install.sh \
  https://raw.githubusercontent.com/FixIt-Technologies/vybava/v0.3.3/ci/install.sh \
  && bash /tmp/vybava-install.sh --version 0.3.3 --bin-dir /usr/local/bin --install memorylint,hotfix
```

From source:

```sh
go build -o ./bin/vybava ./cmd/vybava
./bin/vybava catalog list
./bin/vybava install recommended
```

`install` takes item or group selectors (default: the `recommended` group)
and supports `--agent claude|codex|all`,
`--scope user|project`, `--dry-run`, and `--json`. Installed applets are links
to the `vybava` binary, so `vybava memory lint .` and `memorylint .` are
equivalent.

## Layout

```text
catalog/catalog.yaml   package and group source of truth
cmd/vybava/            multicall entrypoint
internal/<id>/         one focused Go package per capability
skills/<id>/           canonical cross-agent skill payloads (SKILL.md plus any
                       references/ and assets/ the skill ships)
docs/                  per-tool references, release flow, decisions
Dockerfile             the luko.to redirector image (deployik app "luko")
```

## Extending

One payload + one catalog entry = one package; presets are one more catalog
line. Contract: [docs/decisions/0001-modular-catalog.md](docs/decisions/0001-modular-catalog.md)
· checklist: [CONTRIBUTING.md](CONTRIBUTING.md) · releases: [docs/homebrew.md](docs/homebrew.md).
