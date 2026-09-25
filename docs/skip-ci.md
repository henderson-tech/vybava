# skip-ci — the org standard for a PR that skips CI and Eve

Two labels and one job guard, the same in every henderson-tech and Reservine
repository. `prm --admin` applies them; `repolicy` holds the labels across
whole owners; `skipci` holds the workflows to the guard.

| Label | Honoured by | Effect |
|---|---|---|
| `skip-ci` | every job of every `pull_request` workflow (the guard below) | the PR's runs skip; GitHub counts a skipped required check as passing |
| `eve-ignore` | Eve itself (`review-webhook.ts`, fixed name, case-sensitive) | Eve never reviews the PR; removing the label re-arms a review of the current head |

Eve is a GitHub App webhook, so no workflow ever tests `eve-ignore` — the
label only has to exist in the repo. `skip-ci` is a workflow contract:

```yaml
jobs:
  verify:
    if: github.event_name != 'pull_request' || !contains(github.event.pull_request.labels.*.name, 'skip-ci')
```

## Why the guard looks like this

- **Job-level, never workflow-level.** A `paths`/`branches` filter or a
  `[skip ci]` commit trailer skips the whole run, and GitHub then leaves every
  required check *pending* forever. A job skipped by `if:` reports a `skipped`
  conclusion, which required-status-check rules treat as passing.
- **`github.event_name != 'pull_request' ||`** keeps `push`, `schedule` and
  `workflow_dispatch` runs of the same workflow untouched — the label only
  ever speaks for a PR.
- **No `labeled`/`unlabeled` trigger.** Adding them re-runs CI on *every*
  label change (FixIt's run-testing learned this on PR #1441: a 77-minute
  suite restarted because `eve-ignore` landed on an already-tested SHA). The
  label must sit on the PR before the push it covers; what already started is
  cancelled by the caller, not by the workflow.
- **Job-level, on every job.** A gating job that others `needs:` would also
  work, but a workflow's job graph is not the standard's business; one line
  per job is greppable, driftable and survives a refactor.

## The three tools

**`vybava skipci check|apply [repo]`** — the drift check and the generator.
`check` lists each job of each `pull_request` (or `pull_request_target`)
workflow as `guarded`, `missing` (no `if:`), `wrap` (a single-line `if:` the
guard can be AND-ed onto) or `manual` (a block or multi-line condition a human
edits), exit 1 when any is not guarded. `apply` inserts and wraps by line
edits — comments, ordering and quoting elsewhere survive — and never rewrites
a `manual` job or an unparsable file. Existing conditions are wrapped
`(existing) && (guard)`, inside `${{ }}` when the original used it. A job
whose condition already contains the `!contains(... 'skip-ci')` test, or is
exactly `github.event_name != 'pull_request'`, is `guarded` as it stands.

**`vybava repolicy audit|apply <owner…>`** — the labels. The default policy
carries both labels (`docs/repolicy.md`); a repo without one drifts and
`apply` creates it with `gh label create --force`.

**`vybava gitkit admin-labels <pr> --repo <path>`** — `prm --admin`'s
deterministic half. Creates both labels in the repo (idempotent), adds the
missing ones to the PR, then cancels every queued or running workflow run on
the head SHA — the push that opened the PR queued its runs with an event
payload that predates the label. Output: `labels`, `labelsAdded`,
`runsCancelled`, `runsLeft` (a run gh could not cancel — reported, never
fatal). `merge-precheck` then reports `gates.ciWaived: true` and `ciOk` for a
PR carrying `skip-ci`: the cancelled runs read as red, and are not a gate.
Landing such a PR is an `--admin` merge — branch protection still sees no
green required check — which is exactly what `prm --admin` does.

## Sweeping an org

```sh
vybava repolicy apply henderson-tech Reservine        # labels everywhere
for r in ~/Work/Projects/FixIt-Technologies/*/; do    # guards, one PR per repo
  (cd "$r" && vybava skipci check --json | jq -r 'select(.missing+.wrap+.manual>0) | .repo')
done
```

The 2026-09-25 sweep (henderson-tech + Reservine) put the guard on every
`pull_request` workflow in both orgs; repos outside them (`LEFTEQ/*`) were left
alone. A new workflow in any repo picks the guard up from `skipci check`, which
belongs in the same place a repo lints its own tree.
