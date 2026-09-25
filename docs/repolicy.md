# repolicy — GitHub repository settings, held to a declared policy

GitHub inherits exactly one thing from an organization: the default branch
*name*. Everything else — "automatically delete head branches", which merge
buttons exist, whether auto-merge is offered — is per repository. So every new
repository starts off whatever policy the org actually works by, and nobody
notices until merged branches pile up in a hundred checkouts.

`repolicy` declares the desired state once and converges whole owners to it.

```sh
repolicy audit henderson-tech LEFTEQ      # report drift, exit 1 when any exists
repolicy apply henderson-tech             # converge the drifting repositories
repolicy audit --policy repos.yaml        # owners and settings from a file
repolicy audit henderson-tech --json      # stable output for agents and CI
```

## What it will and will not touch

**Only settings the policy names.** A policy that declares
`deleteBranchOnMerge` reads that field and writes that field; merge buttons,
visibility and everything else are never read, never sent, never logged.

**Never archived repositories** unless `--include-archived` is passed — they
refuse writes anyway. **Never a repository in `exclude`.** A repository the
token cannot administer is reported as a failed row; the rest of the sweep
still lands, and the command exits non-zero naming the refusals.

## Policy file

Without `--policy`, the built-in policy is `deleteBranchOnMerge: true` and the
owners come from the command line.

```yaml
owners:
  - henderson-tech
  - LEFTEQ
exclude:
  - henderson-tech/vendored-mirror   # exact owner/repo
settings:
  deleteBranchOnMerge: true
  allowMergeCommit: true
  allowSquashMerge: false
```

Owners passed as arguments override the file's `owners`. An unknown setting
key is a hard error naming the vocabulary — a typo must never read as "no
drift".

| Policy key | GitHub setting |
|---|---|
| `deleteBranchOnMerge` | Automatically delete head branches |
| `allowMergeCommit` | Allow merge commits |
| `allowSquashMerge` | Allow squash merging |
| `allowRebaseMerge` | Allow rebase merging |

The three names for one knob (policy key, the `gh repo list --json` field, the
REST field the PATCH carries) are mapped in `Vocabulary`; adding a knob is one
line there plus a row here.

## Labels

```yaml
labels:
  - name: skip-ci
    color: ededed
    description: skip CI on this PR — every pull_request job guards on it; merge is --admin
  - name: eve-ignore
    color: ededed
    description: skip eve's automatic PR review
```

**Presence is the policy.** A repository drifts when a declared label is
absent (`label:<name>` in the drift rows); `apply` creates it with the declared
colour and description through `gh label create`, one call per missing label
(never `--force`). A label that already exists keeps whatever colour and text a human gave
it — repolicy never rewrites one. The default policy (no `--policy` file)
carries the two labels above: they are the org skip standard `prm --admin`
applies and every pull_request workflow guards on (`docs/skip-ci.md`). A
policy may declare labels alone, with no `settings:`.

## The henderson-tech policy

`policies/henderson-tech.yaml` holds the org's own stance — squash is the only
button, merged branches delete themselves:

```bash
vybava repolicy audit henderson-tech --policy policies/henderson-tech.yaml
vybava repolicy apply henderson-tech --policy policies/henderson-tech.yaml
```

It is the half of the stance GitHub cannot inherit. The org ruleset "org main:
PR required, no direct pushes" carries `required_linear_history`, so a merge
commit is refused server-side on every default branch — but the merge BUTTONS
are per-repository with no org default, so a repository created tomorrow offers
all three until `apply` runs. Audit is the alarm; apply is the fix.

## Exit codes

| Code | Meaning |
|---|---|
| 0 | on policy — `apply` converged everything it found |
| 1 | `audit` found drift (the CI gate) |
| 2 | the sweep could not run, or `apply` hit a repository it may not administer |

## In CI

`repolicy audit <owner> --json` on a schedule is the drift alarm for new
repositories: it reads only what the policy declares, needs `gh` authenticated
as someone who can see the owner's repositories, and exits 1 the day a fresh
repository lands off-policy.

## How it talks to GitHub

Through `gh` — one `gh repo list` per owner asking for exactly the declared
fields, then at most one `gh api -X PATCH` per drifting repository carrying all
of its off-policy settings at once. Authentication is `gh`'s; `repolicy` never
reads a token.
