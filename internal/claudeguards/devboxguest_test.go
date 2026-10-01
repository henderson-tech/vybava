package claudeguards

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/henderson-tech/vybava/internal/devboxguest"
)

// onDevboxGuest runs the rest of the test as a portal session on a Devbox
// guest would: the guest's marker file exists.
func onDevboxGuest(t *testing.T) {
	t.Helper()
	marker := filepath.Join(t.TempDir(), "runtime.env")
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	save := devboxguest.Marker
	devboxguest.Marker = marker
	t.Cleanup(func() { devboxguest.Marker = save })
}

// In a portal session the Mac's machine rules stand down: the command already
// runs on the Devbox, and the process table is every workspace's (an Appium
// a workspace's run started has no claude ancestor either), so routing,
// capping or reaping by it would act on other workspaces.
func TestMacMachineRulesStandDownOnDevboxGuest(t *testing.T) {
	root := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := `{"guards":{"devboxOnly":["^bun run test(:|$)"],"devboxWhenWorkspace":["^bunx tsc(\\s|$)"]}}`
	if err := os.WriteFile(filepath.Join(root, "vybava.config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(home, ".devbox", "workspaces", "repo")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	doc := "name: repo\nport_base: 21706\napps:\n  api:\n    sync: " + root + "\n    run: bun run dev\n"
	if err := os.WriteFile(filepath.Join(ws, "workspace.yaml"), []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	hook := func(cmd string) *HookInput {
		in := &HookInput{CWD: root}
		in.ToolInput.Command = cmd
		return in
	}
	withProcTable(t, fakeTable) // at both default caps, with three orphans

	// The same fixtures refuse on the Mac, so a pass below is the guest's.
	if guardDevboxOnly(hook("bun run test")) == nil || guardDevboxWhenWorkspace(hook("bunx tsc")) == nil ||
		guardMachineCap(hookCmd("bun run dev:api")) == nil || len(selectReapVictims(fakeTable)) == 0 {
		t.Fatal("fixtures must trip every rule on the Mac")
	}
	onDevboxGuest(t)
	for name, d := range map[string]*Denial{
		"devbox-only":      guardDevboxOnly(hook("bun run test")),
		"devbox-workspace": guardDevboxWhenWorkspace(hook("bunx tsc")),
		"dev-server-cap":   guardMachineCap(hookCmd("bun run dev:api")),
		"sim-cap":          guardMachineCap(hookCmd("xcrun simctl boot ABC")),
	} {
		if d != nil {
			t.Errorf("%s refused on a Devbox guest:\n%s", name, d.Text())
		}
	}
	if v := selectReapVictims(fakeTable); len(v) != 0 {
		t.Errorf("reap picked %d victims on a Devbox guest", len(v))
	}
	// An uncapped suite still sizes its workers to the box's vCPUs, so the cap
	// stays — answered with the cap alone, since the agent cannot devbox run.
	if d := guardTestWorkerCap(hookCmd("bunx playwright test")); d == nil || strings.Contains(d.Message, "devbox run") {
		t.Errorf("test-worker-cap must still refuse on a Devbox guest, without routing to devbox run: %+v", d)
	}
}
