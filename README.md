| `handoffs` | applet | Handoff ledger upkeep — `handoffs reconcile` judges every open handoff by whether its branches and PRs are still alive and archives the dead ones; unknown is never touched. → [docs/handoffs.md](docs/handoffs.md) |
# Výbava

Výbava is FixIt Technologies' portable engineering environment: small tools,
agent skills, and workstation diagnostics, distributed as one catalog where
every item installs independently.

## Packages

| ID | Kind | What it does |
|---|---|---|
| `memorylint` | applet | Validate and maintain AI memory homes — schema, indexes, wikilinks, fixtures, write hooks. → [docs/memorylint.md](docs/memorylint.md) |
| `claude-guards` | applet | PreToolUse guard hooks for Claude Code — destructive git/docker, secret dumps, host-input automation, commit secrets, whole-disk walks, per-look Appium sessions, and the context-budget rules (no shell-rewritten files, no whole-file dumps, no transcript reads). → [docs/claude-guards.md](docs/claude-guards.md) |
| `lok` | applet | Locale catalogs for AI sessions — configured in `vybava.config.ts`, queried by key, written by verb with every locale kept in sync, extracted from source, gated in CI. → [docs/lok.md](docs/lok.md) |
| `vybava config` | verb | The shared per-repo `vybava.config.ts` every applet reads — `init` scaffolds it with typed helpers, `check` gates CI, `show` prints the evaluated JSON. → [docs/config.md](docs/config.md) |
| `shrt` | applet | Terminal-safe short links on luko.to — offline repo rules, team-shared dynamic rules, minted codes; also the redirector server. → [docs/shrt.md](docs/shrt.md) |
| `posta` | applet | Drive a shared test mailbox end to end — mint a per-run plus-address, wait for the mail a journey triggered, take its links and attachments. → [docs/posta.md](docs/posta.md) |
| `fontfreeze` | applet | Freeze variable webfonts at rendered axis positions and subset per language. |
| `perfrig` | applet | Performance drills from a `testing/<project>/perf` manifest — ramp to first failure, percentile report. |
| `ingressgen` | applet | Render and drift-check complete default-deny Docker ingress policies from a manifest. |
| `reconcile` | applet | Pull-based GitOps for the infra boxes — converge a VPS to its infra repo's merged main from a per-box manifest: HELD hotfixes, transactional nginx hooks, commit rollback, textfile metrics, mesh-only status page + estate hub. → [docs/reconcile.md](docs/reconcile.md) |
| `prm` / `prc` / `merge` | skills | PR create → review-resolve → gated merge workflows for Claude Code and Codex. |
| `codexsync` | applet | Render `~/.claude` skills and commands into `~/.agents/skills`, the structure Codex discovers — nesting preserved, each command a `source-command` skill, duplicate discovery suppressed. → [docs/codexsync.md](docs/codexsync.md) |
| `codexusage` | applet | Explain where the Codex plan limit went — per-thread spend from `~/.codex` rollouts, the derived allowance, and the runway left at the measured burn rate. → [docs/codexusage.md](docs/codexusage.md) |
| `repolicy` | applet | Hold GitHub repository settings to a declared policy across whole owners — GitHub inherits no organization default, so `audit` reports the drift (exit 1) and `apply` converges it, touching only the settings the policy names. → [docs/repolicy.md](docs/repolicy.md) |
| `menubar-doctor` | applet | Find and fix macOS menu-bar items that run but never appear — since macOS 26 Control Center files a status item under the process that launched the app, so anything started from a terminal is filed under the terminal and stays invisible while its switch is off. → [docs/menubar-doctor.md](docs/menubar-doctor.md) |
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
