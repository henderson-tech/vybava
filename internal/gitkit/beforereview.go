package gitkit

import (
	"io"
	"path/filepath"
	"slices"
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

func runBeforeReview(args []string, stdout, stderr io.Writer) int {
	pr := 0
	if i := slices.Index(args, "--pr"); i != -1 {
		next := ""
		if i+1 < len(args) {
			next = args[i+1]
		}
		pr, _ = jsParseInt(next)
	}
	root, err := repoRoot(args)
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

	cfg, configFound := readGitConfig(mainClone)
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
