package gitkit

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The hook is read from the MAIN clone's config and resolved for the
// linked worktree the command runs in.
func TestBeforeReviewResolvesHookForLinkedWorktree(t *testing.T) {
	main := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git(main, "init", "-q", "-b", "main")
	git(main, "commit", "-q", "--allow-empty", "-m", "c1")
	os.Mkdir(filepath.Join(main, ".claude"), 0o755)
	os.WriteFile(filepath.Join(main, ".claude/.claude.git.config"), []byte("BEFORE_REVIEW_CMD=stop {slug} on {branch} for {pr}\n"), 0o644)
	wt := filepath.Join(main, ".worktrees", "pr-12")
	git(main, "worktree", "add", "-q", "-b", "feat/x", wt)

	var stdout, stderr strings.Builder
	if code := runBeforeReview([]string{"--pr", "12", "--repo", wt}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var got BeforeReview
	if err := json.Unmarshal([]byte(stdout.String()), &got); err != nil {
		t.Fatal(err)
	}
	if !got.ConfigFound || got.ResolvedBeforeReviewCmd == nil || *got.ResolvedBeforeReviewCmd != "stop pr-12 on feat/x for 12" || !got.IsWorktree {
		t.Fatalf("before-review = %+v", got)
	}
}

func TestJSParseInt(t *testing.T) {
	for s, want := range map[string]int{"12": 12, " 7x": 7, "-3": -3} {
		if got, ok := jsParseInt(s); !ok || got != want {
			t.Errorf("jsParseInt(%q) = %d, %v", s, got, ok)
		}
	}
	if _, ok := jsParseInt("x7"); ok {
		t.Error("jsParseInt(x7) must be NaN")
	}
}
