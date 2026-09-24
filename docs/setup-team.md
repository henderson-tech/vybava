# vybava setup team — one-click henderson-tech Mac

```sh
brew install --cask henderson-tech/tap/vybava
vybava setup team                 # checklist: tick, enter — guided sign-ins run inline
vybava setup team --yes --json    # defaults, no prompts; human steps come back as `next`
vybava setup team --with devbox   # opt-in extras
vybava setup team --only vitrinka-cli,onyx
vybava upgrade && vybava setup team --update   # later: new Výbava, then newer tools
```

The `henderson` catalog group is the preset: the Onyx vault stack in Helium,
the vitrinka CLI and app, SwitcherooBar, Pultík, and the git family (`prm`,
`push-all`, `sync`, `push-back` on `gitkit`). `devbox` is optional and starts
unticked.

## The `tool` kind

A catalog item with `kind: tool` is an external app or CLI. Výbava never
re-implements its installer — the recipe names the product's own published
channel, and the item is detected live, never recorded in Výbava state (a tool
installed by hand counts as installed).

```yaml
- id: vitrinka-cli
  kind: tool
  status: experimental
  description: …
  tool:
    probe: {command: vitrinka}                    # exactly one: app | command | path
    install: {pultik: "vitrinka-cli-darwin-{arch}"} # exactly one: brew_cask | brew | pultik | bun | run
    setup: [[vitrinka, setup]]                    # guided commands after a fresh install, in order
    needs: [bun]                                  # tool ids installed first
    optional: true                                # unticked in the checklist
    interactive: true                             # installer needs a human terminal
```

Adding a tool is one catalog entry plus one line in the `henderson` group (and
`everything`, whose test enforces it).

| Channel | Install | `--update` |
|---|---|---|
| `brew_cask` / `brew` | `brew install [--cask]` | `brew upgrade` |
| `pultik` | latest shelf artifact from `https://apps.fixit.app/api/apps`, sha256-verified; `.zip`/`.dmg` → the named `.app` into `/Applications` (old copy moved aside first), raw binary → `~/.local/bin` | only when the app's `CFBundleShortVersionString` or the binary's sha256 differs from the shelf |
| `bun` | `bun add -g <pkg>` | `bun add -g <pkg>@latest` |
| `run` | the product's idempotent installer | re-run |

A checksum mismatch is refused (`TOOL_CHECKSUM_MISMATCH`) before anything is
placed. An install whose probe still fails afterwards is a recipe bug
(`TOOL_PROBE_FAILED_AFTER_INSTALL`).

## Humans stay in the loop

Credentials never pass through Výbava. Sign-ins (`vitrinka setup`, the onyx
MCP onboarding, loading the unpacked extension) run with the terminal attached
in checklist mode; under `--yes`/`--json` they are skipped with
`TOOL_NEEDS_HUMAN` and listed in `next`, and anything that needs them is
skipped with `TOOL_NEEDS_MISSING` and the command that fixes it.
