package fleet

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/henderson-tech/vybava/internal/runx"
)

func TestTypeScriptCoversEveryContract(t *testing.T) {
	ts := TypeScript()
	if ts != TypeScript() {
		t.Fatal("TypeScript() is not deterministic")
	}
	for _, want := range []string{
		fmt.Sprintf("  v: %d\n", runx.EnvelopeVersion),
		"export interface FleetSnapshot {",
		"  sessions: FleetSession[]\n",
		"  state: FleetState\n",
		"  startedAt?: string\n",
		"export interface FleetRevive {",
		"  openJobs: LedgerJob[]\n",
		"export interface LedgerEvent {",
		"  owner?: LedgerOwner\n",
		`export type JobKind = "workflow" | "shell" | "monitor" | "agent" | "limit-wait" | "park"`,
		`export type FleetState = "waiting" | "busy" | "idle" | "shell" | "dead" | "ended" | "unknown"`,
	} {
		if !strings.Contains(ts, want) {
			t.Errorf("generated TypeScript lacks %q", want)
		}
	}
}

// TestModCopiesMatchTheGenerator holds every mod's copy of the contract to
// the Go types. A mod adds types/fleet.gen.d.ts from `vybava fleet schema
// --ts`; a changed Go type then fails here until the copies are regenerated.
func TestModCopiesMatchTheGenerator(t *testing.T) {
	copies, err := filepath.Glob(filepath.Join("..", "..", "mods", "*", "types", "fleet.gen.d.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if len(copies) == 0 {
		t.Skip("no mod carries types/fleet.gen.d.ts yet")
	}
	want := TypeScript()
	for _, path := range copies {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s drifted from the fleet contract; regenerate: go run ./cmd/vybava fleet schema --ts > %s", path, path)
		}
	}
}
