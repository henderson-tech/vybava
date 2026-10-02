package installer

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

func TestAFailedTypesCarryKeepsThePriorCopy(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only folder that makes the carry fail")
	}
	root := t.TempDir()
	// Registered after TempDir, so it runs first: the kept copy's read-only
	// folder must be writable again for TempDir's own cleanup.
	t.Cleanup(func() {
		_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err == nil && entry.IsDir() {
				_ = os.Chmod(path, 0o755)
			}
			return nil
		})
	})
	destination := filepath.Join(root, "skills", "demo")
	for name, body := range map[string]string{
		".claude-plugin/plugin.json": `{"name":"demo","version":"0.1.0"}`,
		"hooks/register.ts":          "// prior",
		engineTypes + "/index.d.ts":  "// engine",
	} {
		path := filepath.Join(destination, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The types cannot leave a read-only parent, so the carry fails.
	if err := os.Chmod(filepath.Join(destination, ".claude-plugin"), 0o555); err != nil {
		t.Fatal(err)
	}
	i := Installer{Payload: fstest.MapFS{
		"mods/demo/.claude-plugin/plugin.json": {Data: []byte(`{"name":"demo","version":"0.2.0"}`)},
		"mods/demo/hooks/hooks.json":           {Data: []byte(`{"modules":["./register.ts"]}`)},
		"mods/demo/hooks/register.ts":          {Data: []byte("// new")},
	}}
	stage := filepath.Join(root, "stage")

	err := i.swapMod(stage, "demo", destination)
	if err == nil || !strings.Contains(err.Error(), "the new mod is active") {
		t.Fatalf("swapMod() = %v, want the carry failure reported", err)
	}
	if got, _ := os.ReadFile(filepath.Join(destination, "hooks", "register.ts")); string(got) != "// new" {
		t.Fatalf("active mod = %q, want the new payload", got)
	}
	kept, _ := filepath.Glob(filepath.Join(stage, "demo-*", "hooks", "register.ts"))
	if len(kept) != 1 {
		t.Fatalf("the prior copy was not kept in the stage: %v", kept)
	}
	if got, _ := os.ReadFile(kept[0]); string(got) != "// prior" {
		t.Fatalf("kept copy = %q, want the prior mod", got)
	}
}
