package gitkit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanAdminLabelsAddsOnlyMissingAndCancelsOnlyLive(t *testing.T) {
	runs := []ghRun{
		{DatabaseID: 1, Status: "completed", WorkflowName: "CI"},
		{DatabaseID: 2, Status: "in_progress", WorkflowName: "CI"},
		{DatabaseID: 3, Status: "queued", WorkflowName: "Lint"},
		{DatabaseID: 4, Status: "waiting", WorkflowName: "Deploy"},
	}
	toAdd, toCancel := planAdminLabels([]string{"eve-ignore", "bug"}, runs)
	if strings.Join(toAdd, ",") != "skip-ci" {
		t.Errorf("toAdd = %v, want only skip-ci", toAdd)
	}
	var ids []int64
	for _, r := range toCancel {
		ids = append(ids, r.DatabaseID)
	}
	if len(ids) != 3 || ids[0] != 2 || ids[1] != 3 || ids[2] != 4 {
		t.Errorf("toCancel = %v, want runs 2, 3, 4", ids)
	}
	if add, cancel := planAdminLabels([]string{"skip-ci", "eve-ignore"}, nil); len(add) != 0 || len(cancel) != 0 {
		t.Errorf("fully labelled PR with no runs must be a no-op: %v %v", add, cancel)
	}
}

func TestAdminLabelsRefusesBadArgv(t *testing.T) {
	// A row that unexpectedly passes validation must never reach the real gh:
	// this verb labels PRs and cancels runs (a stray row once labelled a
	// merged vybava PR). The shim fails loudly instead.
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte("#!/bin/sh\necho 'test reached gh' >&2\nexit 97\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, argv := range [][]string{{}, {"--repo", "."}, {"abc"}, {"12", "13"}, {"12", "--bogus"}, {"12", "--repo", ""}, {"12", "--repo", ".", "--repo=."}} {
		var stderr bytes.Buffer
		if code := runAdminLabels(argv, &bytes.Buffer{}, &stderr); code == 0 {
			t.Errorf("%v: exit 0, want a usage error", argv)
		} else if !strings.Contains(stderr.String(), adminLabelsUsage) {
			t.Errorf("%v: stderr %q lacks the usage line", argv, stderr.String())
		}
	}
}
