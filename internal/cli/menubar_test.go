package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/menubar"
	"howett.net/plist"
)

// menubarRegistry builds the nested Control Center table: a Swift dictionary
// keyed by a non-string type, so it encodes as [key, value, key, value…].
func menubarRegistry(t *testing.T, owner string, allowed bool, items ...string) []byte {
	t.Helper()
	location := func(id string) map[string]any {
		return map[string]any{"bundle": map[string]any{"_0": id}}
	}
	locations := make([]any, 0, len(items))
	for _, item := range items {
		locations = append(locations, location(item))
	}
	inner, err := plist.Marshal([]any{location(owner), map[string]any{
		"isAllowed": allowed, "location": location(owner), "menuItemLocations": locations,
	}}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := plist.Marshal(map[string]any{menubar.RegistryKey: inner}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	return outer
}

func menubarEnvReading(registry []byte) func() (menubar.Env, error) {
	return func() (menubar.Env, error) {
		return menubar.Env{
			Home: "/home", GOOS: "darwin", Now: time.Now(),
			ReadFile: func(name string) ([]byte, error) {
				if name != menubar.RegistryPath("/home") {
					return nil, fs.ErrNotExist
				}
				return registry, nil
			},
			WriteFile: func(string, []byte, fs.FileMode) error { return nil },
			MkdirAll:  func(string, fs.FileMode) error { return nil },
			Exec:      func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
		}, nil
	}
}

func TestMenubarDoctorDispatchesAsAnApplet(t *testing.T) {
	var out, errOut bytes.Buffer

	command, err := (App{Stdout: &out, Stderr: &errOut}).Command("menubar-doctor")
	if err != nil {
		t.Fatal(err)
	}
	if command.Use != "menubar-doctor" {
		t.Fatalf("argv[0] must select the applet, got %q", command.Use)
	}
	var verbs []string
	for _, sub := range command.Commands() {
		verbs = append(verbs, sub.Name())
	}
	if !strings.Contains(strings.Join(verbs, " "), "fix") || !strings.Contains(strings.Join(verbs, " "), "launch") {
		t.Fatalf("applet is missing its verbs: %v", verbs)
	}
}

func TestMenubarScanReportsInvisibleItemsAndExitsOne(t *testing.T) {
	// The shape of the bug: a terminal switched off in the Menu Bar pane,
	// carrying another app's status item.
	registry := menubarRegistry(t, "dev.terminal.app", false, "dev.terminal.app", "com.example.bar")

	for _, asJSON := range []bool{false, true} {
		var out, errOut bytes.Buffer
		rt := runtime{stdout: &out, stderr: &errOut, json: asJSON}
		command := rt.menubarCommandWithEnv("menubar-doctor", menubarEnvReading(registry))
		command.SetArgs(nil)

		err := command.Execute()
		if !errors.Is(err, ErrFindings) || ExitCode(err) != 1 {
			t.Fatalf("an invisible item must exit 1 with no error text: err=%v exit=%d", err, ExitCode(err))
		}
		if ErrorText(err) != "" {
			t.Errorf("findings are not an error message: %q", ErrorText(err))
		}
		if asJSON {
			var report menubar.Report
			if err := json.Unmarshal(out.Bytes(), &report); err != nil {
				t.Fatal(err)
			}
			if len(report.Blocked()) != 1 || report.Blocked()[0].Item != "com.example.bar" {
				t.Errorf("JSON envelope missing the finding: %+v", report)
			}
		} else if !strings.Contains(out.String(), "com.example.bar") || !strings.Contains(out.String(), "INVISIBLE") {
			t.Errorf("human output must name the item: %s", out.String())
		}
	}
}

func TestMenubarScanIsQuietAndExitsZeroOnAHealthyMachine(t *testing.T) {
	var out, errOut bytes.Buffer
	rt := runtime{stdout: &out, stderr: &errOut}
	command := rt.menubarCommandWithEnv("menubar-doctor", menubarEnvReading(
		menubarRegistry(t, "com.example.fine", true, "com.example.fine")))
	command.SetArgs(nil)

	if err := command.Execute(); err != nil {
		t.Fatalf("a healthy machine exits 0: %v", err)
	}
	if !strings.Contains(out.String(), "clean") {
		t.Errorf("stdout: %s", out.String())
	}
}

func TestMenubarRefusesOffMacOS(t *testing.T) {
	var out, errOut bytes.Buffer
	rt := runtime{stdout: &out, stderr: &errOut}
	command := rt.menubarCommandWithEnv("menubar-doctor", func() (menubar.Env, error) {
		return menubar.Env{Home: "/home", GOOS: "linux"}, nil
	})
	command.SetArgs(nil)

	err := command.Execute()
	if !errors.Is(err, menubar.ErrUnsupported) || ExitCode(err) != 2 {
		t.Fatalf("off macOS this is an error, not a finding: err=%v exit=%d", err, ExitCode(err))
	}
}
