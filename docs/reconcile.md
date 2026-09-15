# reconcile

`vybava reconcile` is the pull-based GitOps engine for the infra boxes: a cron
tick pulls the infra repo's `origin/main` into a clone on the box and converges
the mapped files onto the live filesystem. It is the Go port of the bash
`scripts/infra-reconcile/reconcile.sh` engine (webulinka-infra's hardened copy
is the reference; its 23-case `test-reconcile.sh` is mirrored 1:1 in
`internal/reconcile/parity_test.go`), driven by a per-box YAML manifest that
carries the `map-paths.sh` semantics verbatim. Decision log:
`devulinka-infra/docs/specs/2026-09-04-reconcile-go-decisions.md`.

```sh
vybava reconcile run                       # cron entry point (report or converge per the mode file)
vybava reconcile status [--json]           # read-only drift summary: no fetch, no state, no alerts
vybava reconcile force <repo-path>         # stamp the repo version of ONE held file (backed up first)
vybava reconcile rollback [<sha>] [--unpin]# re-converge to last-good (or <sha>) and pin the box there
vybava reconcile serve [--listen ip:port]  # read-only status page + /status.json on the WireGuard address
vybava reconcile serve --hub               # poll serve.hosts and render one estate page
```

Every verb takes `--manifest <path>` (default `./reconcile.yaml`, the clone
root). Unknown subcommands are rejected, never run.

## Safety model (unchanged from bash)

| Situation | What happens |
|---|---|
| live file == repo | adopted silently (recorded as applied) |
| repo changed, live untouched since last apply | **converge**: repo → live, hooks run |
| live file hand-edited (incident hotfix) | **HELD** — never overwritten, never rolled back; alerts until backported or `force`d |
| file only on the box (e.g. `deployik-*.conf`) | ignored — never a directory mirror |
| repo app dir with no `<apps_root>/<app>` on the box | skipped — never materializes a new stack |
| committed symlink, symlinked destination component, write escaping the app dir | refused with a classified error |
| nginx conf converged | `nginx -t` via the manifest hook; reload only on pass; **transactional** — every nginx file the tick touched is restored (and its applied record re-pointed) when the test or any copy fails |
| compose file converged | file only + `ROLL MANUALLY: <app>`; `auto_roll_apps` opt-in runs `docker compose up -d` |
| write refused (EACCES) | `permission` error naming the destination owner, the running user and the mapping's `owner` hint |
| existing live file converged | rewritten **in place on the same inode** — a container bind-mounting that single file (`./pgbouncer/pgbouncer.ini:/etc/pgbouncer/pgbouncer.ini`) sees the new content; a temp + rename swap would leave the mount on the old inode. New files land via temp + rename |
| mode = `report` (default) | computes + alerts everything, changes **nothing** on disk |

`run`, `force` and `rollback` serialize behind one flock-style lock
(`lock_file`, the same path the bash cron wraps with `flock -n`, so both
engines exclude each other during a parity window). A tick that produced
errors exits 1.

## Manifest (`reconcile.yaml`, schema_version 1)

Mappings are ordered `case` arms — first match wins, `*` and `?` match across
`/` exactly like bash `case`. `skip` globs are evaluated before every mapping;
an *ordered* exception to a skip (produlinka's reservine `.env*.example`
contract files) is expressed as a mapping arm placed before a `skip: true` arm.

```yaml
schema_version: 1
repo: devulinka-infra            # REPO_NAME — alert titles, page header
host_label: devops-vps           # eve monitor host label
# clone: defaults to the manifest's directory
mode_file: ~/.config/infra-reconcile/mode
state_dir: ~/.local/state/infra-reconcile
lock_file: /tmp/infra-reconcile.lock
apps_root: /opt/apps             # compose containment root
metrics_file: /opt/monitoring/textfile/infra-reconcile.prom   # optional
vybava_version: v1.4.0           # optional pin; a mismatch is reported, never fatal
auto_roll_apps: []               # apps whose compose converge may `docker compose up -d`
skip: ["*.age", ".env*", "*/.env*", "*.md"]
mappings:
  - match: ["apps/eve-ai-layer/*"]      # its own reconciler owns the dir
    skip: true
  - match: ["nginx/*.conf"]
    strip: nginx/                       # {rest} = path without this prefix
    dest: /opt/nginx-proxy/conf.d/{rest}
    hook: nginx                         # nginx | compose | none
    owner: root                         # classification hint for refused writes
  - match: ["apps/*/*"]
    strip: apps/
    dest: /opt/apps/{rest}              # {app} = first component of {rest}
    hook: compose                       # app derived from {app} (or `app:`)
    require_live_dir: true              # default for compose: skip absent, contain writes
hooks:
  nginx:
    workdir: /opt/nginx-proxy
    test:   [docker, compose, exec, -T, nginx, nginx, -t]
    reload: [docker, compose, exec, -T, nginx, nginx, -s, reload]
  compose: [docker, compose, up, -d]    # auto-roll command, run inside <apps_root>/<app>
alerts:
  - type: telegram                      # sources `lib` and calls notify_telegram
    lib: /opt/scripts/lib/telegram-notify.sh
    channel: lovinka_monitoring
  - type: eve-monitor                   # POST to EVE_MONITOR_URL with EVE_MONITOR_TOKEN
    config: ~/.config/infra-reconcile/eve-webhook
serve:
  listen: 10.8.1.1:9470
  hosts:                                # `serve --hub` polls these
    - { name: devulinka, url: "http://10.8.1.1:9470" }
```

Hook commands are argument arrays, never shell strings. Alerts keep the bash
per-channel dedup: a channel's `last-alert.<channel>` marker is written only
after that channel's own successful delivery; a clean tick clears both. Absent
telegram library / eve config means "channel off", not "failure".
`EVE_MONITOR_CURL_OPTS` from the bash config is not supported and is logged as
ignored.

The three real box manifests (ported from each repo's `map-paths.sh`) live in
`internal/reconcile/testdata/manifests/{produlinka,devulinka,webulinka}.yaml`
with a contract test each; the infra repos adopt them in their own PRs.

## State directory

```
~/.local/state/infra-reconcile/
  applied.tsv      <repo path>\t<sha256 last applied>   — the hotfix detector
  pending-hooks    hooks that failed and retry next tick (nginx, compose:<app>)
  last-good        commit that last converged fully with every hook passing
  pin              commit `rollback` pinned the box to (cleared by --unpin)
  history.jsonl    one entry per run/force/rollback — the page and metrics read it
  last-alert.*     per-channel digest dedup markers
  backups/         live files `force` overwrote (<ts>-<name>.<random>, mode 600)
```

`status` never creates or touches any of it.

## Rollback and last-good

The git commit is the unit of history. After a converge tick with zero errors
and zero failed hooks the tick's commit becomes `last-good`. `reconcile
rollback` resets the clone to `last-good` (or the given `<sha>`), converges
under the normal rules (HELD files stay HELD) and **pins** the box: every
following `run` reconciles to the pin instead of `origin/main` and says so in
its log, until `rollback --unpin`. Without the pin the next 2-minute tick would
simply re-apply `origin/main` and the rollback would be a no-op — so fix
forward in git, merge, then unpin.

Within a tick, nginx keeps the bash engine's transactional last-good: touched
conf files are snapshotted before overwrite and restored when `nginx -t` (or
any copy in the batch) fails.

## Metrics

Written atomically to `metrics_file` on every `run`/`rollback` tick:

```
infra_reconcile_last_tick_timestamp <unix>
infra_reconcile_pending <n>
infra_reconcile_held <n>
infra_reconcile_errors <n>              # errors + failed hooks
infra_reconcile_mode_info{mode="report"} 1
infra_reconcile_last_good_commit_info{sha="…"} 1
```

A staleness rule on `last_tick_timestamp` (lives in devulinka's alert rules)
closes the silent-death gap: a stuck lock or dead cron stops the timestamp.

## serve / hub

`serve` binds **only** to a concrete private address assigned to a local
interface (`0.0.0.0`, loopback, hostnames and public IPs are refused) and
answers GET only: `/` (sync state, last-good, pin, history, per-file diff
links), `/status.json` (the `status --json` shape) and `/diff?path=<repo
path>` (unified diff live → repo, mapped + tracked paths only). Nothing on it
mutates a box; `force` and `rollback` stay CLI.

`serve --hub` polls each `serve.hosts[].url/status.json` on `--interval`
(default 30s) and renders the estate as one table; unreachable boxes show as
such instead of vanishing.

```sh
# per box (systemd unit, deploy user)
vybava reconcile serve --manifest /home/deploy/infra/devulinka-infra/reconcile.yaml
# devulinka only
vybava reconcile serve --hub --manifest /home/deploy/infra/devulinka-infra/reconcile.yaml
```

## Devulinka pilot: red drills

Run in **report** mode beside the converging bash engine first — same lock
file, so the two never overlap; compare `status --json` against the bash log
line per tick (pending / HELD / errors must classify identically). Then the
three drills, each proving one safety property before the Go engine takes
converge:

1. **Broken nginx conf → rollback.** Merge a vhost with a syntax error. Expect:
   the tick logs `nginx -t FAILED — rolled back 1 conf file(s)`, the previous
   conf is live again, `applied.tsv` points at the restored content, the page
   shows `errors` and the Telegram/eve digest carries the error once. Fix in
   git; the next tick converges and reloads once. Alternatively
   `vybava reconcile rollback` to pin the box on last-good while the fix lands.
2. **Hand-edited live file → HELD.** Edit a mapped file on the box. Expect:
   `HELD (hand-edited live, backport or force)`, the file untouched across
   ticks and across a `rollback`, `infra_reconcile_held 1`, the digest deduped
   after the first alert. Resolve by backporting (clears on its own) or
   `vybava reconcile force <path>` (live copy lands in `backups/`).
3. **Root-owned destination → classified error.** As `deploy`, map a file into
   a root-owned `/opt/apps/<app>`. Expect: one `permission` error naming
   `destination owned by root, running as deploy; manifest owner hint: root`,
   the rest of the sweep unaffected, `infra_reconcile_errors 1`, exit 1. Fix by
   chowning the tree to the deploy user or moving the mapping.

Only after all three: flip the mode file to `converge`, retire the bash cron
entry, keep the eve app lanes (`apps/eve-*/reconcile.sh`) on their own cron.
