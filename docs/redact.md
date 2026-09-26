# redact — secrets out of agent conversation history

Every tool call an agent makes is persisted verbatim: Claude Code writes it to
`~/.claude/projects/…` JSONL, Codex to `~/.codex/sessions/…`. Later sessions
re-read those files, and archives upload them (vitrinka `--transcript`, Eve
distill). A secret an agent printed once — or a "harmless" fragment of one —
lives there until someone removes it.

The incident this was built from (2026-09-26): a read-only readiness subagent
checked which production env vars were set by printing
`NAME=<set len 44 prefix abcdefg…>` for 11 variables. 22 fragments landed in
the transcript; the agent's own report named two. **A redactor must never
trust an agent's account of what it leaked** — it scans.

```sh
redact                                     # this session ($CLAUDE_CODE_SESSION_ID), report only
redact --session 2bbb9875-…                # a Claude session and everything under it
redact --session 019c…                     # or a Codex thread's rollout
redact --project ~/Work/app --since week   # a repo's Claude (worktrees too) + Codex history
redact --all                               # every Claude and Codex file, prompt history included
redact --all --json                        # the stable report for agents
redact --session 2bbb9875-… --apply        # overwrite what it found, in place
```

**Reporting is the default.** Without `--apply` nothing is written. A scan
with findings exits 1 (so an agent or a hook can gate on it); a failed file
exits 1 too, with the reason on that file.

## What a session is, on disk

`--session <id>` takes every file that belongs to the session — not only its
transcript:

| Path | What |
|---|---|
| `~/.claude/projects/<slug>/<id>.jsonl` | the main transcript |
| `<slug>/<id>/subagents/agent-*.jsonl` | subagents |
| `<slug>/<id>/subagents/workflows/wf_*/{journal,agent-*}.jsonl` | workflow runs |
| `<slug>/<id>/workflows/*.json`, `scripts/*.js` | workflow state and scripts |
| `<slug>/<id>/tool-results/*.txt` | large tool outputs, persisted as plain text |
| `/tmp/claude-<uid>/<slug>/<id>/tasks/b*.output` | background-task output (plain text) |
| `/tmp/claude-<uid>/<slug>/<id>/tasks/a*.output` | **symlinks** to the subagent transcripts |

Symlinks are resolved and every file is scanned and written once, however
many names reach it (the report lists the others under `via`). `--all` adds
`~/.claude/history.jsonl` and `~/.codex/history.jsonl` — every typed prompt,
pastes included.

## What it finds

Detection is `internal/secretscan`, shared with claude-guards' commit scan and
memorylint — one catalogue, one place to add a shape.

| Detector | Example (value shown as ‹…›) |
|---|---|
| provider tokens | `ghp_‹…›`, `AKIA‹…›`, `sk-ant-‹…›`, `sk_live_‹…›`, `xoxb-‹…›`, `AIza‹…›`, `npm_‹…›`, JWTs |
| `private-key` | a whole `-----BEGIN … PRIVATE KEY-----` block — a header with no key body is prose, left alone |
| `env-assignment` | `MAIL_PASSWORD=‹…›`, `"TWILIO_TOKEN": "‹…›"`, `APP_KEY: ‹…›` — secret-named, never `PUBLIC_KEY` |
| `credential-assignment` | `password: ‹…›`, `"api_key":"‹…›"` |
| `url-credential` | `postgres://app:‹…›@db` |
| `auth-header` | `Authorization: Bearer ‹…›` |
| `cli-flag` | `--password=‹…›` |
| `fragment` | `len 44 prefix ‹…›…`, `head=‹…›`, `ghp_‹…›…` |
| `known` | an exact value supplied out of band (below) |

Names, references and placeholders are not values: `$MAIL_PASSWORD`,
`${{ secrets.X }}`, `onyx://…`, `process.env.X`, `changeme`, `<redacted>`, a
bare `SORT_KEY=createdAt`, a code path. JSON-escaped text is decoded before
it is scanned, so a secret inside a tool result's escaped string is found
exactly as it would read.

## Values no pattern would catch

A random 32-character password under an innocent name matches no shape. Give
the redactor the value itself — it is matched verbatim and never leaves the
process:

```sh
redact --all --known-dotenv ~/Work/app/.env            # every secret-named value of that .env
redact --all --known-env LEAKED_KEY                     # a value in the environment
```

To check real vault items, let onyx inject them — `run_command` with
`env_refs` puts each value into the child's environment under a name you
choose, and nothing but counts comes back:

```text
run_command argv: ["vybava", "redact", "--all", "--known-env", "K1", "--known-env", "K2"]
            env_refs: {"K1": "onyx://Reservine/SK/mail-password", "K2": "onyx://Reservine/SK/twilio-token"}
```

The item's `allowed_commands` binding has to admit `vybava redact`. onyx's
`secret_identify` is **not** used: it asks for Touch ID on every call and is
budgeted per item per hour — an oracle, not a scanner.

## How `--apply` writes

Each span is overwritten **in place with the same number of bytes**:
`[REDACTED:<detector>]` padded with `*`, or all `*` when the marker does not
fit. That is load-bearing:

- **A live session keeps writing.** A temp file + rename would drop whatever
  the session appends meanwhile, and a writer holding the old inode would
  keep appending to the unlinked file. An in-place overwrite of bytes already
  written races with nothing.
- **Offsets hold.** `internal/transcripts` cursors (tokentime, operator)
  resume by byte offset. A same-length edit past the first 256 bytes is
  invisible to them; a length change would shift every record after it.
- **JSONL stays valid at every instant.** Spans are mapped back from the
  decoded string to whole escape sequences and replaced with plain ASCII
  inside the string.
- **Nothing keeps the original.** No temp copy, no backup. The span is written
  only while the file still holds the exact bytes the scan saw there;
  anything else counts as `changed` and a rerun picks it up.

The modification time is restored when nothing else wrote meanwhile, so an old
session keeps its place in `claude --resume`. Each touched file appends one
line to `~/.config/vybava/redact-audit.jsonl`: path, count, detectors.

A second run over the same files finds nothing — a `Fill` is a placeholder
to every detector.

## Every session, at its end

`claude-guards redact-session` is a SessionEnd hook (in the manifest, so
`doctor --fix` wires it): when a session ends it runs the `--apply` pass over
that session's files — subagents, workflows, tool results, task output — and
appends to the audit log. It never fails the exit; a problem is one line on
stderr. By then nothing appends to the transcript any more, so this is the
cheapest moment to scrub it — before an archive uploads it or a later session
re-reads it. History from before the hook existed is untouched until someone
runs `redact --all --apply`.

## Limits

- **Thinking blocks are signed.** Redacting inside one invalidates its
  signature; resuming that session may then be refused by the API. The secret
  going away is worth it; the session is usually done.
- **Copies elsewhere stay.** Time Machine, APFS snapshots, a vitrinka archive
  or an Eve distill upload made before the redaction still hold the original.
  Rotate what matters.
- **Files over 256 MiB** (non-JSONL) and **records over 256 MiB** are reported
  as not scanned, never skipped silently.
- An unfinished last JSONL record is left to its writer and read next run.

## Prevention

`claude-guards`' `secrets:*` family blocks the commands that leak in the first
place — `printenv` of a secret-named variable, a length or prefix taken of a
secret (`${#x}`, `cut -c`, `.slice(`), `echo "$API_TOKEN"` not piped into its
consumer. Printing a `.env` is deliberately not blocked (it would stop ~20
ordinary commands a day); the SessionEnd pass below is what catches it.
`claude-guards ctx` reports a session's leaks with the command that removes
them. Rules: `claude-guards list secrets`.
