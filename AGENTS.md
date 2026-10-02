# Výbava Agent Entry

Read `CLAUDE.md` before changing this repository. Treat its guidance as shared
agent instructions; it is not Claude-specific despite the filename.

The package catalog in `catalog/catalog.yaml` is the source of truth. Keep
Codex and Claude payloads canonical under `skills/`; never maintain separate
copies per agent runtime.

## CLI contract

Agent-facing applets (perflab, polish-kit, blip, readiness, …) follow the
`cli-craft` skill (`skills/cli-craft/`). A thin-wrapper skill that hits a
failure without an actionable diagnostic and `next` reports a bug here; fix
the applet, never document around it.

- One envelope per invocation under `--json`, success and failure:
  `{v, ok, verb, data, diagnostics, next}` through `internal/runx`
  (`Session.Emit`, one `Finish` path). Verbs return `runx.DiagError` or a
  plain error; they never print errors or call `os.Exit`.
- Diagnostic codes are a closed enum in the applet's `diag.go`, each with a
  doc comment saying when it fires and what fixes it, and a row in the
  applet's `docs/<applet>.md`; a test holds the doc and the enum together.
- `next` carries exact commands (ids and tokens filled in) on success and
  failure. Misuse is a `USAGE` diagnostic with the corrected invocation,
  never a usage dump.
- Exit codes: `0` ok, `1` `INFRA_ERROR`, `2` diagnostics. A wrapped
  command's exit code is data, not the applet's exit.
- Long verbs print `<applet>[<tag>] phase=…` progress lines on stderr for
  Monitor; stdout carries only the envelope.
- Sanitize before emitting: no credential-bearing URLs, tokens, headers or
  env values in any envelope field.
- Re-running a verb is its recovery path (idempotent, checkpointed).
