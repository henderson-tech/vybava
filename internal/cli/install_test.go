package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/henderson-tech/vybava/internal/catalog"
	"github.com/henderson-tech/vybava/internal/installer"
)

// A group reaching a mod under --agent codex skips it and says so; a mod
// named outright is refused, since Codex has no mods.
func TestCodexInstallSkipsGroupModsAndRefusesANamedMod(t *testing.T) {
	items := []catalog.Item{{ID: "peek", Kind: catalog.KindMod}, {ID: "prm", Kind: catalog.KindSkill}}

	var stderr bytes.Buffer
	rt := &runtime{stderr: &stderr}
	if err := rt.noteCodexMods([]string{"everything"}, items, installer.AgentCodex); err != nil {
		t.Fatalf("group install refused: %v", err)
	}
	if !strings.Contains(stderr.String(), "skipped for --agent codex: peek") {
		t.Fatalf("stderr = %q, want the skipped mod named", stderr.String())
	}

	if err := rt.noteCodexMods([]string{"peek"}, items[:1], installer.AgentCodex); err == nil {
		t.Fatal("a mod named outright was accepted for Codex")
	}
}
