# Intake

Target shape per finding: `id · title · where (file:line) · claim · suggested fix ·
by · url`. Missing fields stay empty; never invent a location.

## PR URL or number

`vybava gitkit resolve-fetch <pr> --repo <path> [--include-resolved]` — findings
across `inline`, `review-thread`, `review-summary` and `conversation`;
`skipped.informational` (bot walkthroughs, bodiless approvals) is not a finding.
Read PR head files from the local mirror (`git show refs/pr/<N>:<path>`), never
`gh api .../contents`. Standalone (not under prm): `vybava gitkit worktree ensure <headRef> <pr>` → run
every git, test and lint command inside its `path`, never the main checkout;
`diverged: true` → stop and report, never reset.

## Markdown file

Markers: `### C1`, `### H1`, `### Hyg1`, `### U1`, `## Finding N`, a table row, or a
list item with a path. Capture the doc's own severity and dispositions; anything
under "DECLINED", "dropped" or "deferred" is pre-declined. `git log --since=<doc
date> --stat` on the cited paths flags findings the code may have outrun.

## Pasted text

Split on numbered or bulleted items, headings, or "file:line" anchors; one claim per
finding. Severity absent → order by what the claim says breaks.
