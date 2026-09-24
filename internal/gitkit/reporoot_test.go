package gitkit

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestExplicitRepoArg(t *testing.T) {
	t.Setenv("GIT_SKILL_REPO", "")
	for _, tc := range []struct {
		argv []string
		want string
		ok   bool
	}{
		{[]string{"--repo", "/tmp/x"}, "/tmp/x", true},
		{[]string{"--repo=/tmp/y"}, "/tmp/y", true},
		{[]string{"--pr", "7"}, "", false},
	} {
		if got, ok := explicitRepoArg(tc.argv); got != tc.want || ok != tc.ok {
			t.Errorf("explicitRepoArg(%q) = %q, %v", tc.argv, got, ok)
		}
	}
	t.Setenv("GIT_SKILL_REPO", "/tmp/z")
	if got, ok := explicitRepoArg(nil); got != "/tmp/z" || !ok {
		t.Errorf("GIT_SKILL_REPO fallback = %q, %v", got, ok)
	}
}

func TestRepoRootRejectsMissingAnchor(t *testing.T) {
	if _, err := repoRoot([]string{"--repo", "/definitely/not/here"}); err == nil || !strings.Contains(err.Error(), "not an existing directory") {
		t.Fatalf("err = %v", err)
	}
}

// An explicit anchor wins over the process cwd, and a path inside the repo
// resolves up to its toplevel — the drift case.
func TestRepoRootResolvesAnchorToplevel(t *testing.T) {
	dir := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	expected, err := runIn(dir, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "apps", "web")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, anchor := range []string{dir, sub} {
		if got, err := repoRoot([]string{"--repo", anchor}); err != nil || got != strings.TrimSpace(expected) {
			t.Errorf("repoRoot(%s) = %q, %v; want %q", anchor, got, err, expected)
		}
	}
}
