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

// prm's shapes parse (ensure-pr.md passes --repo, round.md `[--pr <N>]` from
// the worktree, merge.md a bare call); an unknown flag or a PR given as a
// positional is refused, never dropped.
func TestBeforeReviewArgs(t *testing.T) {
	for _, argv := range [][]string{nil, {"--repo", "/r"}, {"--pr", "12"}, {"--pr=12", "--repo=/r"}} {
		if _, _, err := beforeReviewArgs.parse("before-review", argv); err != nil {
			t.Errorf("%q refused: %v", argv, err)
		}
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--repo", "/r", "--base", "main"}, "before-review: unknown argument --base"},
		{[]string{"--repo", "/r", "12"}, `before-review: unexpected argument "12"`},
		{[]string{"--pr", "abc"}, `before-review: --pr needs a PR number, got "abc"`},
	} {
		var stdout, stderr strings.Builder
		if code := runBeforeReview(tc.args, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), tc.want) || !strings.Contains(stderr.String(), beforeReviewArgs.usage) {
			t.Errorf("%v → %d %q, want %q + usage", tc.args, code, stderr.String(), tc.want)
		}
	}
}
