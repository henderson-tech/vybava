---
name: cli-craft
description: "Use when building or changing an agent-facing CLI (Go/Charm) or the thin-wrapper skill in front of it — its output envelope, diagnostics or exit contract."
---

# cli-craft — AI-first CLI tools and their thin-wrapper skills

The operator is an AI agent. The intelligence lives in the binary; the
Claude skill on top is a thin usage wrapper. A skill that needs
troubleshooting sections means the CLI is underbuilt — move that knowledge
into diagnostics and `next`.

## Division of labor

| Layer | Owns |
|---|---|
| CLI binary | All hard work: state, validation, feedback, recovery. Every failure names the cause and the exact next command. |
| Skill (SKILL.md) | The protocol (ordered verbs), hard laws, per-project memory pointer. Nothing the envelope already teaches. |
| Owning repo's AGENTS.md | The contribution doctrine — a "CLI contract" section (devbox's is the reference). |

## The contract

Every verb, success and failure, emits one versioned envelope under
`--json`: `{v, ok, verb, data, diagnostics, next}`. Read
`references/envelope.md` before defining a new verb or diagnostic.

- `next` IS the protocol: exact commands, on success AND failure.
- Diagnostics use a CLOSED code enum; each carries severity, plain-language
  detail, and the exact fix command when one exists.
- Never a bare error, panic, stack trace, or usage dump. Misuse (unknown
  verb, missing flag, wrong context) answers with the corrected invocation.
- Real exit codes: 0 ok, 1 infra, 2 diagnostics; JSON parseable on every path.
- A failure that leaves the operator guessing is a CLI bug — fix the CLI,
  never document around it.

## Stack

Go, compiled, fast startup. TUI only where a human takes the keyboard:
Charm — bubbletea/lipgloss. Every verb stays fully drivable
non-interactively via `--json`; the TUI is a view, never the only path.

## Building

- New CLI or verb: `references/go-patterns.md` (envelope emitter, DiagError,
  Finish, closed enum — lifted from devbox `cli/internal/runx/`).
- Any TUI surface: `references/tui-patterns.md` (Charm structure, no hangs,
  no dead ends, resumable flows).
- The wrapper skill: `references/skill-template.md`.
