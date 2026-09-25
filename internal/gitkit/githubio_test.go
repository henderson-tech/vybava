package gitkit

import (
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func TestBuildGitHubCommand(t *testing.T) {
	for _, tc := range []struct {
		sub  string
		o    flags
		want string
	}{
		{"find-run", flags{"sha": "abc123"}, "run list --commit abc123 --json databaseId,status,conclusion,workflowName,headSha --limit 20"},
		{"watch-run", flags{"runId": "99"}, "run watch 99 --exit-status"},
		{"failed-logs", flags{"runId": "99"}, "run view 99 --log-failed"},
		{"rerun-failed", flags{"runId": "99"}, "run rerun 99 --failed"},
		{"reply", flags{"owner": "o", "repo": "r", "pr": "5", "commentId": "11", "body": "Fixed in abc."}, "api --method POST repos/o/r/pulls/5/comments/11/replies -f body=Fixed in abc."},
		{"resolve-thread", flags{"threadId": "RT_1"}, "api graphql -f query=" + resolveThreadMutation + " -f threadId=RT_1"},
		{"comment", flags{"owner": "o", "repo": "r", "pr": "5", "body": "B"}, "api --method POST repos/o/r/issues/5/comments -f body=B"},
		{"react", flags{"owner": "o", "repo": "r", "commentId": "11"}, "api --method POST repos/o/r/issues/comments/11/reactions -f content=+1"},
		// ready-for-review by default; --draft only when passed
		{"create-pr", flags{"head": "feat/x", "base": "main"}, "pr create --head feat/x --base main --fill"},
		{"create-pr", flags{"head": "feat/x", "base": "main", "draft": ""}, "pr create --head feat/x --base main --fill --draft"},
		// an explicit title/body wins over --fill; empty ones emit no bare flag
		{"create-pr", flags{"head": "feat/x", "base": "main", "title": "Fix the thing", "body": "Why it broke."}, "pr create --head feat/x --base main --fill --title Fix the thing --body Why it broke."},
		{"create-pr", flags{"head": "feat/x", "base": "main", "title": "", "body": "", "label": "eve-ignore"}, "pr create --head feat/x --base main --fill --label eve-ignore"},
		{"review", flags{"owner": "o", "repo": "r", "pr": "5", "event": "request-changes", "body": "Breaks callers."}, "pr review 5 --request-changes --body Breaks callers. --repo o/r"},
		{"review", flags{"owner": "o", "repo": "r", "pr": "5", "event": "comment", "body": "Notes."}, "pr review 5 --comment --body Notes. --repo o/r"},
	} {
		argv, err := buildGitHubCommand(tc.sub, tc.o)
		if err != nil || strings.Join(argv, " ") != tc.want {
			t.Errorf("%s %v = %q, %v", tc.sub, tc.o, argv, err)
		}
	}
}

func TestBuildGitHubCommandRefusals(t *testing.T) {
	for _, tc := range []struct {
		sub  string
		o    flags
		want string
	}{
		// never self-approve is enforced in the builder
		{"review", flags{"owner": "o", "repo": "r", "pr": "5", "event": "approve", "body": "lgtm"}, "never approve"},
		{"watch-run", flags{}, "missing required field: runId"},
		{"reply", flags{"owner": "o", "pr": "5"}, "missing required field: repo"},
		{"nope", flags{}, "Unknown github-io subcommand: nope"},
	} {
		if _, err := buildGitHubCommand(tc.sub, tc.o); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.sub, err, tc.want)
		}
	}
}

func TestParseFlags(t *testing.T) {
	// a valueless flag is boolean wherever it sits
	for _, argv := range [][]string{{"--draft", "--title", "T"}, {"--title", "T", "--draft"}} {
		if o, err := parseFlags(argv); err != nil || !maps.Equal(o, flags{"draft": "", "title": "T"}) {
			t.Errorf("parseFlags(%q) = %v, %v", argv, o, err)
		}
	}
	// zsh does not split an unquoted $VAR: name that, not a missing field
	_, err := parseFlags([]string{"--owner acme --repo app --pr 1066", "--commentId", "11"})
	if err == nil || !regexp.MustCompile(`ONE argument with embedded spaces[\s\S]*does NOT word-split`).MatchString(err.Error()) {
		t.Errorf("glued flags: %v", err)
	}
}

func TestDetectWorkflows(t *testing.T) {
	root := t.TempDir()
	if got, err := detectWorkflows(root); err != nil || len(got) != 0 {
		t.Fatalf("no workflows dir = %v, %v", got, err)
	}
	dir := filepath.Join(root, ".github", "workflows")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "ci.yml"), []byte("name: \"CI\"\r\non: push\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "release.yaml"), []byte("on: push\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "notes.md"), []byte("name: no\n"), 0o644)
	got, err := detectWorkflows(root)
	want := []Workflow{{"CI", ".github/workflows/ci.yml"}, {"release.yaml", ".github/workflows/release.yaml"}}
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("detectWorkflows = %v, %v", got, err)
	}
}

func TestGitHubIOUsage(t *testing.T) {
	var stdout, stderr strings.Builder
	if code := runGitHubIO(nil, &stdout, &stderr); code != 1 || !strings.HasPrefix(stderr.String(), "github-io — ") {
		t.Errorf("bare: %d %q", code, stderr.String())
	}
	stderr.Reset()
	if code := runGitHubIO([]string{"help"}, &stdout, &stderr); code != 0 || stdout.String() != githubIOUsage {
		t.Errorf("help: %d", code)
	}
}
