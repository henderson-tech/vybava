package catalog

import "testing"

func TestToolRejectsEmptySetupCommand(t *testing.T) {
	tool := &Tool{Probe: Probe{Command: "x"}, Install: Install{Brew: "x"}, Setup: [][]string{{"x", "setup"}, {}}}
	if err := tool.validate(); err == nil {
		t.Fatal("an empty setup command was accepted")
	}
}
