package gitkit

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestAdminLabelsOrchestration drives runAdminLabels against a scripted `gh`
// on PATH: it records every argv and answers each subcommand, so the verb's
// real call sequence and JSON shape are asserted, not just its plan.
func TestAdminLabelsOrchestration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"commit", "-q", "--allow-empty", "-m", "x"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "calls.log")
	// The PR already carries eve-ignore; the repo already has skip-ci (so no
	// label create at all), two runs are live and one cancel fails.
	shim := `#!/bin/sh
echo "$*" >> "` + log + `"
case "$*" in
  "repo view"*) echo "acme/widgets" ;;
  "pr view"*) echo '{"number":7,"url":"https://github.com/acme/widgets/pull/7","headRefOid":"abc123","labels":[{"name":"eve-ignore"},{"name":"bug"}]}' ;;
  "run list"*) echo '[{"databaseId":1,"status":"completed","workflowName":"CI"},{"databaseId":2,"status":"in_progress","workflowName":"CI"},{"databaseId":3,"status":"queued","workflowName":"Lint"}]' ;;
  "label list"*) echo '[{"name":"good first issue"},{"name":"skip-ci"}]' ;;
  "run cancel 3"*) echo "already completed" >&2; exit 1 ;;
  *) : ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var stdout, stderr bytes.Buffer
	if code := runAdminLabels([]string{"7", "--repo", repo}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr.String())
	}
	raw, _ := os.ReadFile(log)
	calls := strings.Split(strings.TrimSpace(string(raw)), "\n")
	want := []string{
		"repo view --json nameWithOwner --jq .nameWithOwner",
		"pr view 7 --repo acme/widgets --json number,url,headRefOid,labels",
		"run list --repo acme/widgets --commit abc123 --limit 200 --json databaseId,status,workflowName",
		"label list --repo acme/widgets --limit 1000 --json name",
		"pr edit 7 --repo acme/widgets --add-label skip-ci",
		"run cancel 2 --repo acme/widgets",
		"run cancel 3 --repo acme/widgets",
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("gh calls:\n%s\nwant:\n%s", strings.Join(calls, "\n"), strings.Join(want, "\n"))
	}
	var out AdminLabels
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if out.PR != 7 || out.HeadSha != "abc123" || strings.Join(out.Labels, ",") != "eve-ignore,bug,skip-ci" || strings.Join(out.LabelsAdded, ",") != "skip-ci" {
		t.Errorf("labels: %+v", out)
	}
	if len(out.RunsCancelled) != 1 || out.RunsCancelled[0].ID != 2 || len(out.RunsLeft) != 1 || out.RunsLeft[0].ID != 3 || out.RunsLeft[0].Status != "queued" {
		t.Errorf("runs: cancelled=%+v left=%+v", out.RunsCancelled, out.RunsLeft)
	}
	if !strings.Contains(stderr.String(), "run 3 (Lint) not cancelled") {
		t.Errorf("a failed cancel must be said on stderr: %q", stderr.String())
	}
	// Wire shape: arrays are never null.
	if !strings.Contains(stdout.String(), `"runsLeft": [`) || strings.Contains(stdout.String(), "null") {
		t.Errorf("JSON shape: %s", stdout.String())
	}
}
