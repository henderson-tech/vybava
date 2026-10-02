package installer_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/henderson-tech/vybava/internal/catalog"
	"github.com/henderson-tech/vybava/internal/installer"
	"github.com/henderson-tech/vybava/internal/state"
)

func modPayload(module string) fstest.MapFS {
	return fstest.MapFS{
		"mods/demo/.claude-plugin/plugin.json":                  {Data: []byte(`{"name":"demo","version":"0.1.0"}`)},
		"mods/demo/hooks/hooks.json":                            {Data: []byte(`{"modules":["./register.ts"]}`)},
		"mods/demo/hooks/register.ts":                           {Data: []byte(module)},
		"mods/demo/.claude-plugin/types/claude-code/index.d.ts": {Data: []byte("// engine-written, never shipped")},
		"mods/demo/.DS_Store":                                   {Data: []byte("finder")},
	}
}

func modInstaller(t *testing.T, module string) (installer.Installer, string) {
	t.Helper()
	root := t.TempDir()
	return installer.Installer{
		Payload:  modPayload(module),
		Store:    state.Store{Path: filepath.Join(root, "state.json")},
		StageDir: filepath.Join(root, "stage"),
	}, root
}

func planMod(t *testing.T, i installer.Installer, root string) []installer.Operation {
	t.Helper()
	operations, err := i.Plan([]catalog.Item{{ID: "demo", Kind: catalog.KindMod}}, installer.Options{
		Scope: installer.ScopeProject, RootDir: root,
	})
	if err != nil {
		t.Fatalf("Plan() error = %v", err)
	}
	if len(operations) != 1 || operations[0].Agent != "claude" {
		t.Fatalf("Plan() = %+v, want one Claude operation", operations)
	}
	return operations
}

func TestModInstallsIntoClaudeSkillsWithoutEngineFilesOrStaging(t *testing.T) {
	t.Parallel()

	i, root := modInstaller(t, "export const register = () => {}")
	if err := i.Apply(planMod(t, i, root), false); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	skills := filepath.Join(root, ".claude", "skills")
	installed := filepath.Join(skills, "demo")
	for _, want := range []string{".claude-plugin/plugin.json", "hooks/hooks.json", "hooks/register.ts", ".vybava-package.json"} {
		if _, err := os.Stat(filepath.Join(installed, want)); err != nil {
			t.Errorf("installed mod lacks %s: %v", want, err)
		}
	}
	for _, unwanted := range []string{".claude-plugin/types", ".DS_Store"} {
		if _, err := os.Stat(filepath.Join(installed, unwanted)); err == nil {
			t.Errorf("installed mod carries %s from the payload", unwanted)
		}
	}
	assertOnlyEntry(t, skills, "demo")
}

func TestModUpgradeSwapsInPlaceAndKeepsEngineTypes(t *testing.T) {
	t.Parallel()

	i, root := modInstaller(t, "export const register = () => {} // v1")
	operations := planMod(t, i, root)
	if err := i.Apply(operations, false); err != nil {
		t.Fatalf("first Apply() error = %v", err)
	}
	installed := operations[0].Destination
	laid := filepath.Join(installed, ".claude-plugin", "types", "claude-code", "index.d.ts")
	if err := os.MkdirAll(filepath.Dir(laid), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(laid, []byte("// laid by the engine at load"), 0o644); err != nil {
		t.Fatal(err)
	}

	i.Payload = modPayload("export const register = () => {} // v2")
	if err := i.Apply(operations, false); err != nil {
		t.Fatalf("upgrade Apply() error = %v", err)
	}
	module, err := os.ReadFile(filepath.Join(installed, "hooks", "register.ts"))
	if err != nil || !strings.Contains(string(module), "v2") {
		t.Fatalf("upgraded module = %q, %v; want v2", module, err)
	}
	if data, err := os.ReadFile(laid); err != nil || string(data) != "// laid by the engine at load" {
		t.Fatalf("engine types after upgrade = %q, %v; want them carried over", data, err)
	}
	assertOnlyEntry(t, filepath.Join(root, ".claude", "skills"), "demo")
	entries, err := os.ReadDir(i.StageDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("stage directory keeps %d entries after the swap", len(entries))
	}
}

func TestModSkipsCodexAndRefusesUnmanagedFolders(t *testing.T) {
	t.Parallel()

	i, root := modInstaller(t, "export const register = () => {}")
	codex, err := i.Plan([]catalog.Item{{ID: "demo", Kind: catalog.KindMod}}, installer.Options{
		Agent: installer.AgentCodex, Scope: installer.ScopeProject, RootDir: root,
	})
	if err != nil || len(codex) != 0 {
		t.Fatalf("Plan(codex) = %v, %v; want a mod skipped for Codex", codex, err)
	}

	operations := planMod(t, i, root)
	if err := os.MkdirAll(operations[0].Destination, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := i.Apply(operations, false); err == nil {
		t.Fatal("Apply() replaced an unmanaged folder")
	}
}

func TestModRemoveTakesTheFolderOutWhole(t *testing.T) {
	t.Parallel()

	i, root := modInstaller(t, "export const register = () => {}")
	operations := planMod(t, i, root)
	if err := i.Apply(operations, false); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}
	if err := i.Remove(operations, false); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := os.Stat(operations[0].Destination); !os.IsNotExist(err) {
		t.Fatalf("mod still installed after Remove(): %v", err)
	}
	if err := i.Remove(operations, false); err != nil {
		t.Fatalf("second Remove() error = %v", err)
	}
	entries, err := os.ReadDir(i.StageDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("stage directory keeps %d entries after Remove()", len(entries))
	}
}

func assertOnlyEntry(t *testing.T, dir, want string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 1 || names[0] != want {
		t.Fatalf("%s holds %v, want only %s", dir, names, want)
	}
}
