# watch — one poller for PRs, CI, Eve, devbox, vitrinka and deployik

A week of Claude transcripts (2026-09-25 → 10-02) held ~25k hand-rolled
`sleep`/`until` wait loops and 1,630 per-session `gitkit pr-events` Monitors:
every session polled its own targets, often the same PR. `vybava watch` is the
one poller instead. A daemon (`watch serve`, the LaunchAgent `vybava.watchd`)
probes each **distinct** target once per interval however many sessions wait
on it, under one GitHub budget, and queues per-session events. Clients
subscribe and long-poll; `watch until` is a blocking CLI for Monitor, Codex
lanes and scripts.

```sh
vybava watch until pr:155 checks-settled          # blocks; a line per change; exit 0 when met
vybava watch until devbox-run:my-ws idle --timeout 30m
vybava watch add --session "$CLAUDE_CODE_SESSION_ID" --target pr:155 --until merged
vybava watch next --session "$CLAUDE_CODE_SESSION_ID" --timeout 25s --json
vybava watch ls                                    # subscriptions + each target's last reading
vybava watch status                                # is the daemon up
vybava watch agent install [--dry-run]             # the LaunchAgent
```

Every verb but `serve` and `until` answers the versioned runx envelope under
`--json` (`{v, ok, verb, data, diagnostics, next}`). Closed diagnostics:
`WATCH_DAEMON_DOWN` (fix: `vybava watch agent install`), `WATCH_REQUEST_INVALID`,
`WATCH_UNKNOWN_SUBSCRIPTION`, `WATCH_BINARY_UNSTABLE`; exit 2 with a diagnostic,
1 for infrastructure failures.

## Targets and conditions

A target is `<kind>:<ref>`. The probe canonicalizes the ref; the canonical
form is the dedupe key. Every kind also takes `changed` (any field differs
from the reading the subscription started with) and `<field>=<value>` over the
fields below.

| Kind | Ref | Read through | Fields | Conditions |
|---|---|---|---|---|
| `pr` | `155`, `#155`, `owner/name#155` or a PR URL | gitkit `merge-precheck` as a child of the binary (killed at the probe timeout), anchored at the subscriber's `--dir` | `state` `checks` `ci` (green/waived/red/pending/absent) `review` `bots` (`ok` or `pending: <bots>`) `mergeable` `draft` `failed` | `merged` `closed` `checks-settled` `checks-green` `checks-red` `eve-approved` `ready` |
| `devbox` | a box name (`a`, `b`) | `devbox boxes --json` | `state` `reachable` `health` | `up` `down` |
| `devbox-run` | a workspace name | `devbox status <ws> --json` (`data.runs`) | `runs` `running` `ids` | `idle` (its runs finished) `running` |
| `vitrinka` | `<id>` or `<workspace>/<id>` | `vitrinka task get <ref> --json` | `state` `status` | `done` `closed` |
| `deployik` | `<slug>[/<environment>]` (default `production`) | `deployik status <slug> --json` run in `--dir` | `rollup` (building/failed/live/none) `branch` | `live` `failed` `building` `settled` |

- **pr** reads the exact gates /prm merges on, so a watcher never disagrees
  with the merge loop: CI is gitkit's `ciOk`/`ciWaived`, `eve-approved` is
  `botApproval.ok` (every `REQUIRED_BOT_REVIEWERS` bot, Eve by default), and
  `ready` is every gate but the local worktree's cleanliness (that is the
  watcher's checkout, not the PR). A bare number is resolved to
  `owner/name#n` with `gh repo view` in `--dir`; the probe refuses an anchor
  that is a checkout of another repository. Use `checks-settled`, never
  `checks-green`, to wait for a run: `NONE` in a repo with workflows is
  `absent` (nothing ran yet), not green.
- **devbox-run** treats an answer without `data.runs` as an error, never as
  idle: a shape change must not release every waiter at once.
- **deployik** needs the subscriber's directory to hold `.deployik.json` (the
  server URL); the LaunchAgent has no `DEPLOYIK_URL`. `live` holds at once when
  the environment is already live — after `deployik deploy`, wait for
  `settled`, or subscribe `changed` first.
- Probes shell out to the user's own CLIs and never read or store a token.

## Events and delivery

A subscription's first reading is its baseline (no event). After that, each
reading that differs from the last one it was told about queues a `change`
event (`changed` lists the fields); a holding condition queues `met` and drops
the subscription. `error` is queued once per failure streak (3 failed probes in
a row); `expired` when the TTL (default 24 h, `--ttl`) runs out. A condition
that already holds on a reading younger than the kind's interval is met in the
`add` answer itself.

Delivery is at-least-once: `next --after <seq>` acknowledges (deletes) the
session's events up to `seq` and answers newer ones, waiting up to `--timeout`
(capped at 60 s) for the first. A caller that crashes before acknowledging gets
the same events again. Each session's queue keeps its newest 100 events;
`rm` drops the subscription's unacknowledged events with it (`watch until`
removes its own on exit), and an event nobody acknowledged for 7 days is
dropped.

## Scheduling: dedupe, backoff, budget

- **Dedupe** — one entry per canonical target. Two sessions on PR #155 cost
  one probe per interval (pr 60 s, devbox 60 s, devbox-run 30 s, vitrinka
  60 s, deployik 30 s). A target nobody waits on is never probed again. A PR is
  probed from its oldest subscriber's `--dir`.
- **Backoff** — a failed probe waits 2, 4, then 8 intervals (capped at 8); a
  rate-limit-looking error (`rate limit`, 403, 429) jumps straight to 8. One
  good reading resets it. Every failure is logged to the daemon's log.
- **Unsettled readings** — a probe answers `ErrUnsettled` while its source is
  mid-computation: an open PR whose mergeability GitHub is recomputing after a
  push to the base. The last reading stands and is asked again next interval —
  no change event, no backoff, no failure count — so every merge to `main`
  does not toast each session watching a PR.
- **GitHub budget** — a token bucket shared by every `pr` target: 300 units
  at once, refilled at 600 per hour (`serve --gh-burst`, `--gh-per-hour`). A
  PR probe costs 5 (merge-precheck spends a repo view, a pr view and one
  GraphQL round). A probe that cannot pay waits for the refill instead of
  polling, so many watched PRs stretch their intervals rather than spend the
  rate limit every session and `gh` call on the Mac shares.

## The daemon

`watch serve` keeps its state in `~/.local/state/vybava/watch/` (dir 0700):
`watchd.sock` (0600) and `state.json`, replaced atomically on every change.
The state holds the subscriptions (with their baselines) and the
unacknowledged events, versioned (`StateVersion`); a file of another version is
refused, never re-read. Readings, backoff and the budget are rebuilt on start:
a restart re-probes every target and neither replays nor swallows a change.
The daemon holds `watchd.sock.lock` (flock) for its lifetime, taken before it
reads `state.json` or judges the socket, so two daemons starting together can never both call it stale. A
socket file nobody answers on is then replaced; a live one refuses a second
daemon. SIGTERM stops accepting, lets in-flight probes finish and exits.

`Engine.Every(name, every, fn)` registers periodic work beside the probes (the
fleet summary publisher is the first); a failed task is logged, never
swallowed. A panic inside a probe or a
task is recovered into that reading's error and goes through the backoff; it
never takes the daemon down.

### LaunchAgent

```sh
vybava watch agent install --dry-run   # the plan, nothing touched
vybava watch agent install             # write the plist, (re)start the agent
vybava watch agent uninstall           # stop it, remove the plist; state stays
```

`~/Library/LaunchAgents/vybava.watchd.plist` runs `<vybava> watch serve` at
login, kept alive, `ProcessType Background`, logging to
`~/Library/Logs/vybava/watchd.log`. Its `PATH` is the installing shell's
(launchd's own lacks `gh`, `devbox`, `vitrinka`, `deployik`). The binary is
this one with symlinks resolved; a `go run` build is refused
(`WATCH_BINARY_UNSTABLE`, pass `--bin`). The plan is a `[]Step` like vpn's,
run as the user (no sudo): an installed agent is booted out after the new
plist is written, so a reinstall is a restart onto the new binary.

## Socket API

Plain HTTP/1.1 over the unix socket: a Claude Code mod reaches it with
`$.http.fetch({socketPath})`, a shell with `curl --unix-socket`. Errors are
`{"error": "..."}` with a 4xx/5xx status.

| Request | Answer |
|---|---|
| `GET /v1/health` | `{ok, pid, subscriptions, targets, githubBudget, tasks}` |
| `POST /v1/subscriptions` `{session, target, until, dir?, ttl?}` | 201 `{subscription, events}` (422 on a bad target or condition) |
| `GET /v1/subscriptions[?session=]` | `{subscriptions, targets: [{target, subscribers, summary, observedAt, nextAt, failures, lastError}]}` |
| `DELETE /v1/subscriptions/{id}` | `{removed}` (404 when unknown) |
| `GET /v1/events?session=&after=&timeout=` | `{events: [{seq, at, session, subscription, target, until, kind, summary, fields, changed, error}]}` |

`ttl` is nanoseconds on the wire (a Go duration); the CLI takes `--ttl 2h`.

## Monitor and Codex

`watch until` replaces a sleep loop under Claude's Monitor or in a Codex lane:
one line per state change on stdout (`--json`: one event object per line),
exit 0 when the condition holds, 124 on `--timeout`, 1 on failure. With no
daemon answering it says so on stderr and probes directly with the same probes
— nothing depends on the LaunchAgent being installed, it only stops being
shared.

```sh
# Claude Monitor: wake when the PR's checks settle
vybava watch until pr:155 checks-settled --timeout 2h
# Codex lane / script
vybava watch until vitrinka:fixit/4759 done && echo shipped
```
