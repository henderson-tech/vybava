package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestPluginGCDispatchesAsAnApplet(t *testing.T) {
	var out, errOut bytes.Buffer

	command, err := (App{Stdout: &out, Stderr: &errOut}).Command("plugin-gc")
	if err != nil {
		t.Fatal(err)
	}
	if command.Use != "plugin-gc" {
		t.Fatalf("argv[0] must select the applet, got %q", command.Use)
	}
	if command.Flags().Lookup("apply") == nil {
		t.Fatal("destruction must be behind an explicit --apply flag")
	}
	if command.Flags().Lookup("apply").DefValue != "false" {
		t.Fatal("reporting is the default; --apply must default to false")
	}
	for _, flag := range []string{"only", "skip", "plugin", "home"} {
		if command.Flags().Lookup(flag) == nil {
			t.Fatalf("missing --%s", flag)
		}
	}
	if !strings.Contains(command.Long, "installed_plugins.json") {
		t.Fatal("help must say where the active version comes from")
	}
}
