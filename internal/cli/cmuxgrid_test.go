package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCmuxGridPlanFormats(t *testing.T) {
	for _, machine := range []bool{false, true} {
		var out bytes.Buffer
		rt := &runtime{stdout: &out, stderr: &out, json: machine}
		cmd := rt.cmuxGridCommand()
		cmd.SetArgs([]string{"plan", "--width", "1620", "--height", "2880"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if machine {
			var result struct {
				Shape struct {
					Columns int `json:"columns"`
					Rows    int `json:"rows"`
				} `json:"shape"`
			}
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Shape.Columns != 3 || result.Shape.Rows != 4 {
				t.Fatalf("unexpected plan: %s", out.String())
			}
		} else if !strings.Contains(out.String(), "3 columns × 4 rows (12 terminals)") {
			t.Fatalf("not a readable plan: %s", out.String())
		}
		for _, sub := range cmd.Commands() {
			if sub.Name() != "help" && sub.Short == "" {
				t.Errorf("%s has no help description", sub.Name())
			}
		}
	}
}
