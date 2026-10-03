# The thin-wrapper skill template

If you are writing a troubleshooting section, stop: that knowledge belongs
in the CLI as a diagnostic + fix.

```markdown
---
name: <tool>
description: "<TRIGGER: the situations and concrete terms that should fire
  it — third person, when-to-invoke only; what it does belongs in the body>"
---

# <Tool>

<One or two sentences: the mental model — what owns what.>

## Protocol

<The ordered verbs a session runs, as a fenced block, each with a one-line
comment. End with:>

Every verb emits `{v, ok, verb, data, diagnostics, next}` under `--json`.
**The envelope's `next` field IS the protocol** — run what it says,
verbatim, on success and failure alike; diagnostics carry a closed code and
an exact fix. A failure without an actionable diagnostic and `next` is a
CLI bug: fix it in <repo> per AGENTS.md "CLI contract" — never investigate
around it.

## Hard laws

<3–6 numbered laws: the constraints the CLI cannot enforce itself —
state-authority, isolation, what never to bypass or delete.>

Per-project notes: `memo find <tool>` (the memo ledger).
```

Command stub (`commands/<tool>.md`), when the skill dir can't register
top-level:

```markdown
---
name: <tool>
description: <one line>
argument-hint: "[...]"
---

Read `<abs path>/SKILL.md` completely and follow it. Pass `$ARGUMENTS`
through: <what arguments mean>.
```
