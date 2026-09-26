package gitkit

import (
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
)

// before-review — resolve a project's BEFORE_REVIEW_CMD, the pre-review
// quiesce hook. Symmetric with AFTER_MERGE_CMD but usable BEFORE a PR exists
// and cheap enough for every /prm round: git-only, no gh, no network.
//
// Entering a review loop invalidates whatever the checkout has running:
// merges, installs and codegen thrash the disk while dev servers and
// bundlers keep doing stale work, and clients like Expo's dev client cannot
// survive those operations anyway. Stopping first beats recovering after.
// resolvedBeforeReviewCmd null means "no hook — skip the step", never an error.

// BeforeReview is before-review's output, keys in wire order.
type BeforeReview struct {
	ConfigFound             bool    `json:"configFound"`
	ConfigPath              string  `json:"configPath"`
	BeforeReviewCmd         *string `json:"beforeReviewCmd"`
	ResolvedBeforeReviewCmd *string `json:"resolvedBeforeReviewCmd"`
	Slug                    string  `json:"slug"`
	Branch                  string  `json:"branch"`
	Worktree                string  `json:"worktree"`
	MainClone               string  `json:"mainClone"`
	IsWorktree              bool    `json:"isWorktree"`
}

// beforeReviewArgs: --pr fills the hook's {pr} token; there are no
// positionals (the create path quiesces before a PR exists).
var beforeReviewArgs = verbArgs{values: []string{"pr", "repo"}, usage: "usage: vybava gitkit before-review [--pr <N>] [--repo <abs>]"}

func runBeforeReview(args []string, stdout, stderr io.Writer) int {
	flags, _, err := beforeReviewArgs.parse("before-review", args)
	if err != nil {
		return fail(stderr, err)
	}
	pr := 0
	if raw, given := flags["pr"]; given {
		// A bad --pr used to run the hook against PR 0; refuse it instead.
		n, ok := positiveInt(raw)
		if !ok {
			return fail(stderr, fmt.Errorf("before-review: --pr needs a PR number, got %q\n%s", raw, beforeReviewArgs.usage))
		}
		pr = n
	}
	root, err := repoRoot(repoAnchor(flags))
	if err != nil {
		return fail(stderr, err)
	}
	git := func(a ...string) (string, error) {
		out, err := execFile(execOpts{dir: root, echo: stderr, timeout: 30 * time.Second}, "git", a...)
		return strings.TrimSpace(out), err
	}
	worktree, err := git("rev-parse", "--show-toplevel")
	if err != nil {
		return fail(stderr, err)
	}
	branch, err := git("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return fail(stderr, err)
	}
	list, err := git("worktree", "list", "--porcelain")
	if err != nil {
		return fail(stderr, err)
	}
	mainClone := worktree
	for line := range strings.SplitSeq(list, "\n") {
		if path, ok := strings.CutPrefix(line, "worktree "); ok {
			mainClone = strings.TrimSpace(path)
			break
		}
	}

	cfg, configFound, err := readGitConfig(mainClone)
	if err != nil {
		return fail(stderr, err)
	}
	slug := filepath.Base(worktree)
	out := BeforeReview{
		ConfigFound: configFound,
		ConfigPath:  filepath.Join(mainClone, ".claude/.claude.git.config"),
		Slug:        slug,
		Branch:      branch,
		Worktree:    worktree,
		MainClone:   mainClone,
		IsWorktree:  worktree != mainClone,
	}
	if cmd, ok := cfg.get("BEFORE_REVIEW_CMD"); ok {
		out.BeforeReviewCmd = &cmd
		if cmd != "" {
			resolved := substituteHookTokens(cmd, hookContext{slug: slug, branch: branch, worktree: worktree, pr: pr})
			out.ResolvedBeforeReviewCmd = &resolved
		}
	}
	if err := writeJSON(stdout, out); err != nil {
		return fail(stderr, err)
	}
	return 0
}
