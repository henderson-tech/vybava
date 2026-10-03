package watch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func commands(steps []Step) string {
	var lines []string
	for _, s := range steps {
		lines = append(lines, s.Command())
	}
	return strings.Join(lines, "\n")
}

func TestAgentInstallPlanRendersWithoutTouchingLaunchd(t *testing.T) {
	steps := AgentInstallPlan("/Users/me", 501, "/Users/me/.local/bin/vybava", "/opt/homebrew/bin:/usr/bin", true)
	got := commands(steps)
	for _, want := range []string{
		"/bin/mkdir -p -m 0700 /Users/me/.local/state/vybava/watch",
		"/bin/mkdir -p /Users/me/Library/Logs/vybava",
		"write /Users/me/Library/LaunchAgents/vybava.watchd.plist (mode 0644)",
		"/bin/launchctl bootout gui/501/vybava.watchd",
		"/bin/launchctl enable gui/501/vybava.watchd",
		"/bin/launchctl bootstrap gui/501 /Users/me/Library/LaunchAgents/vybava.watchd.plist",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("plan lacks %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "bootout") > strings.Index(got, "bootstrap") {
		t.Error("the old job must be booted out before the new one bootstraps")
	}
	if fresh := commands(AgentInstallPlan("/Users/me", 501, "/b", "/usr/bin", false)); strings.Contains(fresh, "bootout") {
		t.Errorf("a fresh install boots out nothing:\n%s", fresh)
	}
	if un := commands(AgentUninstallPlan("/Users/me", 501, true)); !strings.Contains(un, "bootout gui/501/vybava.watchd") || !strings.Contains(un, "rm -f /Users/me/Library/LaunchAgents/vybava.watchd.plist") {
		t.Errorf("uninstall plan:\n%s", un)
	}
}

func TestAgentPlistRunsServeWithTheInstallersPath(t *testing.T) {
	plist := AgentPlist("/Apps/V & Co/vybava", DefaultPaths("/Users/me"), "/opt/homebrew/bin:/Users/me/.local/bin")
	for _, want := range []string{
		"<string>vybava.watchd</string>",
		"<string>/Apps/V &amp; Co/vybava</string>\n\t\t<string>watch</string>\n\t\t<string>serve</string>",
		"<string>/opt/homebrew/bin:/Users/me/.local/bin</string>",
		"<string>/Users/me/Library/Logs/vybava/watchd.log</string>",
		"<key>KeepAlive</key>\n\t<true/>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
}

func TestRunPlanWritesFilesAtomically(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "x.plist")
	steps := []Step{
		{Why: "make dir", Argv: []string{"/bin/mkdir", "-p", filepath.Join(dir, "logs")}},
		{Why: "write", Write: target, Mode: "0644", content: "<plist/>"},
	}
	if err := RunPlan(t.Context(), steps, os.Stderr); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	info, _ := os.Stat(target)
	if err != nil || string(data) != "<plist/>" || info.Mode().Perm() != 0o644 {
		t.Fatalf("%q %v %v", data, err, info.Mode())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("leftover temp files: %v", entries)
	}
	err = RunPlan(t.Context(), []Step{{Why: "fail", Argv: []string{"/bin/sh", "-c", "exit 4"}}}, os.Stderr)
	if err == nil || !strings.Contains(err.Error(), "fail failed") {
		t.Fatalf("a failing step answered %v", err)
	}
}
