---
name: find-session
description: Use when the user asks which session a pasted conversation, reply or ending came from, wants an earlier Claude Code session's id, or wants to reopen/resume a session found by its text, title or id prefix — 'find session', 'which session was this', 'resume the session that said …'.
---

Query: `$ARGUMENTS`, else the user's pasted text, fed on stdin through a quoted heredoc:

```sh
find-session <<'FIND_SESSION_QUERY'
<text>
FIND_SESSION_QUERY
```

No query: bare `find-session`.

Relay the top session's id, title, directory and time, then its reopen line alone in a fenced block, nothing after it, never inline. On `PARTIAL_SCAN` when the session doesn't fit the user's description, run its `fix`; on `QUOTED_ONLY`/`AMBIGUOUS`, show the candidates and what tells them apart.

`find-session` missing: `vybava install find-session-cli`.
