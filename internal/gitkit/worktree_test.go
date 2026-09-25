package gitkit

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const worktreePorcelain = "worktree /Users/me/repo\n" +
	"HEAD aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" +
	"branch refs/heads/main\n\n" +
	"worktree /Users/me/repo/.worktrees/pr-201\n" +
	"HEAD bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n" +
	"branch refs/heads/feat-offer-race\n\n" +
	"worktree /Users/me/repo/.worktrees/detached\n" +
	"HEAD cccccccccccccccccccccccccccccccccccccccc\n" +
	"detached\n"

func TestParseWorktreeList(t *testing.T) {
	wts := parseWorktreeList(worktreePorcelain)
	if len(wts) != 3 || wts[0].Path != "/Users/me/repo" || *wts[0].Head != strings.Repeat("a", 40) ||
		*wts[0].Branch != "refs/heads/main" || *wts[1].Branch != "refs/heads/feat-offer-race" || wts[2].Branch != nil {
		t.Fatalf("parseWorktreeList = %+v", wts)
	}
	if mainCloneOf(wts) != "/Users/me/repo" || mainCloneOf(nil) != "" {
		t.Fatal("mainCloneOf must return the first worktree")
	}
	if findWorktreeForBranch(wts, "feat-offer-race") != "/Users/me/repo/.worktrees/pr-201" || findWorktreeForBranch(wts, "nope") != "" {
		t.Fatal("findWorktreeForBranch must match refs/heads/<headRef>")
	}
}

func TestEnsurePlan(t *testing.T) {
	wts := parseWorktreeList(worktreePorcelain)
	reuse, _ := ensurePlan(wts, "feat-offer-race", 201)
	if reuse != (EnsurePlan{Action: "reuse", Path: "/Users/me/repo/.worktrees/pr-201", MainClone: "/Users/me/repo"}) {
		t.Errorf("reuse plan = %+v", reuse)
	}
	create, _ := ensurePlan(wts, "feat-new", 202)
	if create != (EnsurePlan{Action: "create", Path: "/Users/me/repo/.worktrees/pr-202", SelfCreated: true, MainClone: "/Users/me/repo"}) {
		t.Errorf("create plan = %+v", create)
	}
	if _, err := ensurePlan(nil, "feat-x", 1); err == nil || !strings.Contains(err.Error(), "main clone") {
		t.Errorf("empty list: %v", err)
	}
}

// prFixture is a bare "GitHub" whose PR 7 head is published at
// refs/pull/7/head, like the real one.
type prFixture struct {
	t                          *testing.T
	root, author, reviewer, wt string
	fork                       bool
}

func (f *prFixture) git(dir string, args ...string) string {
	f.t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *prFixture) publish() {
	if !f.fork {
		f.git(f.author, "push", "-q", "origin", "HEAD:feature")
	}
	f.git(f.author, "push", "-q", "-f", "origin", "HEAD:refs/pull/7/head")
}

func (f *prFixture) runner(dir string) gitRunner {
	return func(args ...string) (string, error) {
		return execFile(execOpts{dir: dir}, "git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
	}
}

func (f *prFixture) create(localExists bool) bool {
	f.t.Helper()
	diverged, err := createWorktree(f.runner(f.reviewer), f.wt, "feature", 7, localExists)
	if err != nil {
		f.t.Fatal(err)
	}
	return diverged
}

func newPRFixture(t *testing.T, fork bool) *prFixture {
	root := t.TempDir()
	f := &prFixture{t: t, root: root, author: filepath.Join(root, "author"), reviewer: filepath.Join(root, "reviewer"), wt: filepath.Join(root, "wt"), fork: fork}
	f.git(root, "init", "-q", "--bare", "-b", "main", "origin.git")
	f.git(root, "clone", "-q", "origin.git", "author")
	f.git(f.author, "commit", "-q", "--allow-empty", "-m", "c1")
	f.git(f.author, "push", "-q", "origin", "HEAD:main")
	f.publish()
	f.git(root, "clone", "-q", "origin.git", "reviewer")
	return f
}

func TestCreateWorktreeFastForwardsStaleLocalBranch(t *testing.T) {
	f := newPRFixture(t, false)
	f.git(f.reviewer, "branch", "-q", "feature", "origin/feature") // local copy, about to go stale
	f.git(f.author, "commit", "-q", "--allow-empty", "-m", "c2")
	f.publish()
	if f.create(true) || f.git(f.wt, "rev-parse", "HEAD") != f.git(f.author, "rev-parse", "HEAD") {
		t.Fatal("stale local branch was not fast-forwarded to the PR head")
	}
}

func TestCreateWorktreeLeavesAheadBranchDiverged(t *testing.T) {
	f := newPRFixture(t, false)
	scratch := filepath.Join(f.root, "scratch")
	f.git(f.reviewer, "branch", "-q", "feature", "origin/feature")
	f.git(f.reviewer, "worktree", "add", "-q", scratch, "feature")
	f.git(scratch, "commit", "-q", "--allow-empty", "-m", "unpushed")
	local := f.git(scratch, "rev-parse", "HEAD")
	f.git(f.reviewer, "worktree", "remove", scratch)
	if !f.create(true) || f.git(f.wt, "rev-parse", "HEAD") != local {
		t.Fatal("a local branch ahead of the PR head must be reported diverged and left untouched")
	}
}

func TestCreateWorktreeOpensForkPR(t *testing.T) {
	f := newPRFixture(t, true)
	if f.create(false) || f.git(f.wt, "rev-parse", "HEAD") != f.git(f.author, "rev-parse", "HEAD") {
		t.Fatal("fork PR worktree is not at the PR head")
	}
}

func TestRefreshWorktreeSeesLaterPush(t *testing.T) {
	f := newPRFixture(t, false)
	f.create(false)
	f.git(f.author, "commit", "-q", "--allow-empty", "-m", "teammate push")
	f.publish()
	diverged, err := refreshWorktree(f.runner(f.reviewer), f.wt, 7)
	if err != nil || diverged || f.git(f.wt, "rev-parse", "HEAD") != f.git(f.author, "rev-parse", "HEAD") {
		t.Fatalf("refresh = %v, %v; worktree not at the new head", diverged, err)
	}
}

func TestForkPRNeverTracksUnrelatedSameNameBranch(t *testing.T) {
	f := newPRFixture(t, true)
	f.git(f.root, "clone", "-q", "origin.git", "other")
	other := filepath.Join(f.root, "other")
	f.git(other, "commit", "-q", "--allow-empty", "-m", "unrelated")
	f.git(other, "push", "-q", "origin", "HEAD:feature")
	f.create(false)
	if _, err := execFile(execOpts{dir: f.wt}, "git", "rev-parse", "--abbrev-ref", "@{upstream}"); err == nil {
		t.Fatal("fork worktree tracks an unrelated origin branch")
	}
}

func TestWorktreeVerbArgv(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"ensure", "feat"}, "error: usage: worktree.ts ensure <headRef> <pr>\n"},
		{[]string{"list", "feat", "7"}, "error: usage: worktree.ts ensure <headRef> <pr>\n"},
		{[]string{"ensure", "feat", "7.5"}, "error: bad pr number: \"7.5\"\n"},
		{[]string{"ensure", "feat", "-3"}, "error: bad pr number: \"-3\"\n"},
	} {
		var stdout, stderr strings.Builder
		if code := runWorktree(tc.args, &stdout, &stderr); code != 1 || stderr.String() != tc.want {
			t.Errorf("%v → %d %q, want %q", tc.args, code, stderr.String(), tc.want)
		}
	}
}
