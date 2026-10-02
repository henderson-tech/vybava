package installer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAFailedTypesCarryKeepsThePriorCopy(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only folder that makes the carry fail")
	}
	root := t.TempDir()
	prior := filepath.Join(root, "stage", "demo-1")
	destination := filepath.Join(root, "skills", "demo")
	if err := os.MkdirAll(filepath.Join(prior, engineTypes), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prior, engineTypes, "index.d.ts"), []byte("// prior"), 0o644); err != nil {
		t.Fatal(err)
	}
	plugin := filepath.Join(destination, ".claude-plugin")
	if err := os.MkdirAll(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(plugin, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(plugin, 0o755) })

	carryErr := carryEngineTypes(prior, destination)
	if carryErr == nil {
		t.Fatal("a carry into a read-only folder succeeded")
	}
	err := keepPrior(prior, carryErr)
	if err == nil || !strings.Contains(err.Error(), "the new mod is active") {
		t.Fatalf("keepPrior() = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(prior+".kept", engineTypes, "index.d.ts")); statErr != nil {
		t.Fatalf("the prior copy's types were not kept: %v", statErr)
	}
}
