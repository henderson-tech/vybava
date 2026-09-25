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
	// a declared boolean is boolean wherever it sits
	for _, argv := range [][]string{{"--draft", "--title", "T"}, {"--title", "T", "--draft"}} {
		if o, err := parseFlags("create-pr", argv); err != nil || !maps.Equal(o, flags{"draft": "", "title": "T"}) {
			t.Errorf("parseFlags(%q) = %v, %v", argv, o, err)
		}
	}
	// zsh does not split an unquoted $VAR: name that, not a missing field
	_, err := parseFlags("reply", []string{"--owner acme --repo app --pr 1066", "--commentId", "11"})
	if err == nil || !regexp.MustCompile(`ONE argument with embedded spaces[\s\S]*does NOT word-split`).MatchString(err.Error()) {
		t.Errorf("glued flags: %v", err)
	}
	// Refused, never dropped: each of these once did the wrong thing quietly.
	for _, tc := range []struct {
		sub  string
		argv []string
		want string
	}{
		{"create-pr", []string{"--repo", "/some/checkout", "--head", "b", "--base", "main"}, "unknown argument --repo"},
		{"create-pr", []string{"--head", "b", "--base", "main", "--draft", "stray"}, `unexpected argument "stray"`},
		{"create-pr", []string{"--head", "b", "--base", "main", "--fill"}, "unknown argument --fill"},
		{"reply", []string{"--owner", "o", "--repo", "r", "--pr", "5", "--commentId", "1", "--bdy", "x"}, "unknown argument --bdy"},
		{"resolve-thread", []string{"--threadId", "RT", "--pr", "5"}, "unknown argument --pr"},
	} {
		_, err := parseFlags(tc.sub, tc.argv)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "usage: github-io "+tc.sub) {
			t.Errorf("%s %q: err = %v, want %q + the usage line", tc.sub, tc.argv, err, tc.want)
		}
	}
}

func TestResolveBodyFile(t *testing.T) {
	dir := t.TempDir()
	body := filepath.Join(dir, "body.md")
	if err := os.WriteFile(body, []byte("## Why this exists\nBecause.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	o := flags{"head": "b", "base": "main", "body-file": body}
	if err := resolveBodyFile(o); err != nil || o["body"] != "## Why this exists\nBecause.\n" || o["body-file"] != "" {
		t.Fatalf("resolve: %v %v", o, err)
	}
	if argv, err := buildGitHubCommand("create-pr", o); err != nil || !slices.Contains(argv, "## Why this exists\nBecause.\n") {
		t.Fatalf("create-pr did not carry the file's body: %q %v", argv, err)
	}
	empty := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(empty, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		o    flags
		want string
	}{
		{flags{"body-file": empty}, "is empty"},
		{flags{"body-file": filepath.Join(dir, "missing.md")}, "--body-file"},
		{flags{"body-file": body, "body": "inline"}, "not both"},
	} {
		if err := resolveBodyFile(tc.o); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.o, err, tc.want)
		}
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
