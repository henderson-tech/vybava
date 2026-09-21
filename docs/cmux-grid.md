# cmux-grid

Open a fresh native cmux workspace with one terminal in each equally sized cell:

| Display (macOS points) | Columns × rows |
| --- | --- |
| Portrait: height exceeds width | 3 × 4 |
| Landscape Pro Display XDR, or width ≥ 3000 | 5 × 2 |
| Other landscape | 4 × 2 |

The grid is selected at creation. Moving the window between monitors does not
rearrange running terminals. Existing workspaces are never modified or closed.
Closing a terminal uses normal cmux behavior, which can collapse its empty pane.

Requires cmux **0.64.25 or newer**, restarted after updating, and Settings →
Socket Control → **Automation mode** for Hammerspoon's same-user socket client.
Password and Full open access are not required. The helper never reads credentials.

```sh
vybava install cmux-grid
cmux-grid plan --width 2056 --height 1290
cmux-grid new --width 3008 --height 1692 --screen 'Pro Display XDR'
```

`plan` prints the shape and native layout JSON without contacting cmux. `new`
supports `--json`, `--socket` and `--cmux`. It resolves the foreground cmux window
through the socket, ignoring the caller's workspace environment. Failed or
incomplete requests leave the new workspace available for inspection, never
delete terminals or silently retry creation.

Pultík's `hammerspoon/cmux.lua` integrates the applet, when installed at
`~/.local/bin/cmux-grid`: **Cmd+N** creates a monitor-sized grid inside cmux;
**Ctrl+Option+Shift+Enter** forwards native **Cmd+Shift+Enter** pane zoom.
The same chord restores the original grid. **Ctrl+Option+Enter** remains the
global macOS-window maximize shortcut. App-specific hotkeys disable outside cmux.

The native layout uses balanced binary splits weighted by column/row count.
This gives five equal columns instead of repeated halving. No shell command is
injected into any terminal. Focus starts at the top-left terminal.
