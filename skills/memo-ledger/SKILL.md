---
name: memo
description: "Use whenever a session learns a durable fact (a user preference, a correction, a trap, a project rule), acts on a remembered one, or needs to look one up - 'remember this', 'note for next time', 'what did we decide about X', a MEMORY.md row cited as #NN, or any write to a memory home's LEDGER.md / MEMORY.md / usage.jsonl."
---

# memo

`LEDGER.md` is the append-only truth of a memory home; `MEMORY.md` is
rendered from it by usage; `usage.jsonl` is the evidence. memo owns all
three. A session never edits them: it appends a row, cites a row, or looks a
row up.

## Protocol

```text
memo add <type>/<topic>[!] "<one sentence>." [--link '[[notes/<slug>]]'] [--supersedes N] [--retires N] --json
                                      # capture: user|feedback -> personal home, project|reference -> team home
                                      # a fact that changed -> --supersedes N; a fact that is simply gone -> --retires N
#NN / #tNN                            # act: when a row changes what you do, write its id in your reply or tool input;
                                      # bare #NN is a personal row, #tNN a team row; the Stop hook harvests both
memo show <ref> --json                # look up: the row plus its linked notes (45, #45, t12, #t12, fixit-team#12, [[LEDGER#^t12]])
memo find <words>... [--all] --json   # search the ledger, superseded rows marked
memo render --check --json            # CI door: exit 2 when MEMORY.md drifted from the ledger
memo migrate <home> > rows.md         # converting a v2 home: edit the template, then memo import rows.md --home <home>
```

Every verb emits `{v, ok, verb, data, diagnostics, next}` under `--json`.
**The envelope's `next` field IS the protocol** - run what it says, verbatim,
on success and failure alike; diagnostics carry a closed code and an exact
fix. A failure without an actionable diagnostic and `next` is a CLI bug: fix
it in `henderson-tech/vybava` (`internal/memo`, `docs/memo.md`), never
investigate around it.

## Hard laws

1. One row, one fact, one sentence, plain hyphens, ending in a period. The
   narrative behind it goes to `notes/<slug>.md` and the row links it.
2. Never edit `LEDGER.md`, `MEMORY.md` or `usage.jsonl` by hand (Edit, Write,
   heredoc, `sed -i`): the PreToolUse hook refuses and names the verb. A
   wrong row is superseded, never corrected in place.
3. A detail note is written only when the narrative is real (a runbook, a
   reproduction, a decision trail); otherwise the row is the whole memory.
4. Cite `#NN` (personal) or `#tNN` (team) only for a row that actually changed
   what you did; the score is the evidence the hot surface is built on.
5. The personal home's history is memo's local git; never add a remote,
   never push it.
