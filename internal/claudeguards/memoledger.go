package claudeguards

import (
	"fmt"

	"github.com/henderson-tech/vybava/internal/memo"
)

// memo:ledger-hand-write - a shell write (redirect, tee, sed -i, cp/mv, rm)
// to a memo home's LEDGER.md, MEMORY.md or usage.jsonl. The decision is
// memo's own RefuseHandWrite; it runs here so memo's hook no longer spawns on
// every Bash call. memo's Edit/Write hook keeps the tool-write half, and its
// Codex hook still reads the shell tool itself.
func guardMemoLedger(in *HookInput) *Denial {
	p := memo.HookPayload{ToolName: "Bash", Cwd: in.CWD}
	p.ToolInput.Command = in.ToolInput.Command
	d := memo.RefuseHandWrite(p)
	if d == nil {
		return nil
	}
	return deny("memo:ledger-hand-write", fmt.Sprintf("%s\n\nInstead:  %s", d.Detail, d.Fix), "")
}
