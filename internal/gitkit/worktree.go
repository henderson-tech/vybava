package gitkit

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// worktree — discover-or-create an isolated worktree for a PR head branch.
// `worktree ensure <headRef> <pr> [--repo <abs>]` prints
// {action, path, selfCreated, mainClone[, diverged]}.

// Worktree is one block of `git worktree list --porcelain`; Head and Branch
// are nil when absent (Branch is nil for a detached worktree).
type Worktree struct {
	Path   string  `json:"path"`
	Head   *string `json:"head"`
	Branch *string `json:"branch"`
}

func parseWorktreeList(porcelain string) []Worktree {
	out := []Worktree{}
	var cur *Worktree
	flush := func() {
		if cur != nil {
			out = append(out, *cur)
		}
		cur = nil
	}
	for raw := range strings.SplitSeq(porcelain, "\n") {
		line := strings.TrimRightFunc(raw, unicode.IsSpace)
		if path, ok := strings.CutPrefix(line, "worktree "); ok {
			flush()
			cur = &Worktree{Path: path}
		} else if head, ok := strings.CutPrefix(line, "HEAD "); ok && cur != nil {
			cur.Head = &head
		} else if branch, ok := strings.CutPrefix(line, "branch "); ok && cur != nil {
			cur.Branch = &branch
		}
	}
	flush()
	return out
}

// mainCloneOf is the first (primary) worktree's path, "" when none.
func mainCloneOf(worktrees []Worktree) string {
	if len(worktrees) == 0 {
		return ""
	}
	return worktrees[0].Path
}

func findWorktreeForBranch(worktrees []Worktree, headRef string) string {
	want := "refs/heads/" + headRef
	for _, w := range worktrees {
		if w.Branch != nil && *w.Branch == want {
			return w.Path
		}
	}
	return ""
}

// EnsurePlan is worktree ensure's output, keys in wire order. Diverged: the
// local branch holds commits the PR head lacks; the worktree was left on it.
type EnsurePlan struct {
	Action      string `json:"action"`
	Path        string `json:"path"`
	SelfCreated bool   `json:"selfCreated"`
	MainClone   string `json:"mainClone"`
	Diverged    bool   `json:"diverged,omitempty"`
}

func ensurePlan(worktrees []Worktree, headRef string, pr int) (EnsurePlan, error) {
	mainClone := mainCloneOf(worktrees)
	if mainClone == "" {
		return EnsurePlan{}, errors.New("could not determine the main clone from `git worktree list`")
	}
	if existing := findWorktreeForBranch(worktrees, headRef); existing != "" {
		return EnsurePlan{Action: "reuse", Path: existing, MainClone: mainClone}, nil
	}
	return EnsurePlan{Action: "create", Path: fmt.Sprintf("%s/.worktrees/pr-%d", mainClone, pr), SelfCreated: true, MainClone: mainClone}, nil
}

// gitRunner runs one git command in the anchored repo and returns stdout.
type gitRunner func(args ...string) (string, error)

// Worktrees anchor on GitHub's refs/pull/<N>/head — it exists for fork and
// same-repo PRs alike, where origin/<headRef> may be absent or, for a fork
// reusing a common name, an unrelated branch. A stale local branch is
// fast-forwarded to the PR head; one carrying commits the PR head lacks —
// diverged or strictly ahead — or whose fast-forward is blocked by
// uncommitted work is someone's work: left as is and reported (true).
func fetchPrHead(git gitRunner, pr int) (string, error) {
	prHead := "refs/pr/" + strconv.Itoa(pr)
	_, err := git("fetch", "origin", fmt.Sprintf("+refs/pull/%d/head:%s", pr, prHead))
	return prHead, err
}

func alignToPrHead(git gitRunner, path, prHead string) (bool, error) {
	count, err := git("-C", path, "rev-list", "--count", prHead+"..HEAD")
	if err != nil {
		return false, err
	}
	if localOnly, _ := jsNumber(count); localOnly > 0 {
		return true, nil
	}
	if _, err := git("-C", path, "merge", "--ff-only", "--quiet", prHead); err != nil {
		return true, nil
	}
	return false, nil
}

func createWorktree(git gitRunner, path, headRef string, pr int, localExists bool) (bool, error) {
	prHead, err := fetchPrHead(git, pr)
	if err != nil {
		return false, err
	}
	if localExists {
		if _, err := git("worktree", "add", path, headRef); err != nil {
			return false, err
		}
		return alignToPrHead(git, path, prHead)
	}
	if _, err := git("worktree", "add", "-b", headRef, path, prHead); err != nil {
		return false, err
	}
	// Track origin/<headRef> only when it IS the PR head — a name match alone
	// could be an unrelated origin branch a fork PR happens to share. No such
	// branch on origin (fork PR): nothing to track.
	if _, err := git("fetch", "origin", headRef); err != nil {
		return false, nil
	}
	origin, err := git("rev-parse", "origin/"+headRef)
	if err != nil {
		return false, nil
	}
	head, err := git("rev-parse", prHead)
	if err != nil {
		return false, nil
	}
	if strings.TrimSpace(origin) == strings.TrimSpace(head) {
		_, _ = git("-C", path, "branch", "--set-upstream-to", "origin/"+headRef)
	}
	return false, nil
}

// refreshWorktree brings a REUSED worktree to the PR's current head — later
// rounds must see a teammate's or bot's push, not the head it was created at.
func refreshWorktree(git gitRunner, path string, pr int) (bool, error) {
	prHead, err := fetchPrHead(git, pr)
	if err != nil {
		return false, err
	}
	return alignToPrHead(git, path, prHead)
}

// worktreeArgs: `ensure` is the one sub-subcommand; it and its <headRef> and
// <pr> are the three positionals.
var worktreeArgs = verbArgs{values: []string{"repo"}, positionals: 3, usage: "usage: vybava gitkit worktree ensure <headRef> <pr> [--repo <abs>]"}

func runWorktree(args []string, stdout, stderr io.Writer) int {
	flags, pos, err := worktreeArgs.parse("worktree", args)
	if err != nil {
		return fail(stderr, err)
	}
	if len(pos) < 3 || pos[0] != "ensure" || pos[1] == "" || pos[2] == "" {
		return fail(stderr, errors.New(worktreeArgs.usage))
	}
	headRef, prRaw := pos[1], pos[2]
	pr, ok := positiveInt(prRaw)
	if !ok {
		return fail(stderr, fmt.Errorf("bad pr number: \"%s\"", prRaw))
	}
	root, err := repoRoot(repoAnchor(flags))
	if err != nil {
		return fail(stderr, err)
	}
	git := func(a ...string) (string, error) {
		return execFile(execOpts{dir: root, echo: stderr, timeout: 120 * time.Second, maxBuffer: 32 << 20}, "git", a...)
	}
	list, err := git("worktree", "list", "--porcelain")
	if err != nil {
		return fail(stderr, err)
	}
	plan, err := ensurePlan(parseWorktreeList(list), headRef, pr)
	if err != nil {
		return fail(stderr, err)
	}
	if plan.Action == "create" {
		// `show-ref --verify` exits non-zero when the ref is absent — that is
		// the answer, not an error.
		_, missing := git("show-ref", "--verify", "--quiet", "refs/heads/"+headRef)
		plan.Diverged, err = createWorktree(git, plan.Path, headRef, pr, missing == nil)
	} else {
		plan.Diverged, err = refreshWorktree(git, plan.Path, pr)
	}
	if err != nil {
		return fail(stderr, err)
	}
	if err := writeJSON(stdout, plan); err != nil {
		return fail(stderr, err)
	}
	return 0
}
