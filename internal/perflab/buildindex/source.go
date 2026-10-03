package buildindex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SourceTree is a detached in-repo worktree at one commit, for exporting a
// "before" bundle or an isolated build without touching the caller's tree.
type SourceTree struct {
	Dir    string `json:"dir"`
	Commit string `json:"commit"`
	Reused bool   `json:"reused"`
}

// installedMarker lives in the gitignored node_modules, so the source tree
// stays clean (a dirty tree would change the bundle provenance).
const installedMarker = "node_modules/.perflab-installed"

// EnsureSourceWorktree resolves ref in repoRoot and returns the detached
// worktree <main checkout>/.worktrees/perflab-src-<sha12>, creating it and
// installing its dependencies once; later calls for the same commit reuse
// it. It never switches a branch of any checkout.
func EnsureSourceWorktree(ctx context.Context, r Runner, repoRoot, ref string, progress *Progress) (SourceTree, error) {
	git := func(dir string, args ...string) (string, error) {
		res, err := run(ctx, r, Cmd{Argv: append([]string{"git", "-C", dir}, args...), Timeout: 5 * time.Minute})
		if err != nil {
			return "", err
		}
		if res.Exit != 0 {
			return "", diag(DiagUsage, fmt.Sprintf("git %s failed: %s", strings.Join(args, " "), lastLines(res.Stderr, 2)),
				"pass --ref <commit|branch|tag> that exists in "+repoRoot)
		}
		return strings.TrimSpace(string(res.Stdout)), nil
	}
	commit, err := git(repoRoot, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return SourceTree{}, err
	}
	common, err := git(repoRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return SourceTree{}, err
	}
	mainRoot := filepath.Dir(common)
	dir := filepath.Join(mainRoot, ".worktrees", "perflab-src-"+commit[:12])
	tree := SourceTree{Dir: dir, Commit: commit, Reused: true}
	if !fileExists(filepath.Join(dir, ".git")) {
		tree.Reused = false
		phase(progress, "source-worktree")
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return SourceTree{}, err
		}
		if _, err := git(mainRoot, "worktree", "add", "--detach", dir, commit); err != nil {
			return SourceTree{}, err
		}
	}
	if !fileExists(filepath.Join(dir, installedMarker)) {
		phase(progress, "source-install")
		argv := installCmd(dir)
		res, err := run(ctx, r, Cmd{Argv: argv, Dir: dir, Timeout: 20 * time.Minute, Stall: 5 * time.Minute})
		if err != nil {
			return SourceTree{}, err
		}
		if res.Exit != 0 || res.Stalled || res.TimedOut {
			return SourceTree{}, diag(DiagBuildFailed,
				fmt.Sprintf("installing dependencies in %s failed (exit %d): %s", dir, res.Exit, lastLines(res.Stderr, 3)),
				"(cd "+dir+" && "+strings.Join(argv, " ")+")")
		}
		if err := writeFileAtomic(filepath.Join(dir, installedMarker), []byte(commit+"\n")); err != nil {
			return SourceTree{}, err
		}
	}
	return tree, nil
}

// fingerprintRuleFiles configure the Expo fingerprint itself (what it
// hashes), as opposed to the native inputs it hashes.
var fingerprintRuleFiles = []string{"fingerprint.config.js", "fingerprint.config.cjs", ".fingerprintignore"}

// withProjectRules runs fn with the project's fingerprint rule files laid
// over a source tree's app root, then restores the tree byte for byte. A
// --ref key is then the ref's native inputs judged by the project's rules:
// a ref older than the app's fingerprint.config.js (the Reanimated
// staticFeatureFlags extraSources) otherwise keys differently with identical
// native inputs, and pack refuses every such "before" bundle. projectApp ==
// treeApp (no --ref) is a no-op.
func withProjectRules(projectApp, treeApp string, fn func() error) (err error) {
	if filepath.Clean(projectApp) == filepath.Clean(treeApp) {
		return fn()
	}
	type saved struct {
		path string
		data []byte
		had  bool
	}
	var restore []saved
	defer func() {
		for _, s := range restore {
			var rerr error
			if s.had {
				rerr = writeFileAtomic(s.path, s.data)
			} else {
				rerr = os.Remove(s.path)
			}
			if rerr != nil && err == nil {
				err = fmt.Errorf("restoring %s after the fingerprint: %w", s.path, rerr)
			}
		}
	}()
	for _, name := range fingerprintRuleFiles {
		want, werr := os.ReadFile(filepath.Join(projectApp, name))
		path := filepath.Join(treeApp, name)
		have, herr := os.ReadFile(path)
		switch {
		case werr != nil && herr != nil: // neither has it
		case werr == nil && herr == nil && string(want) == string(have):
		case werr == nil:
			restore = append(restore, saved{path: path, data: have, had: herr == nil})
			if err := writeFileAtomic(path, want); err != nil {
				return err
			}
		default: // only the ref has it: judge without it, like the project
			restore = append(restore, saved{path: path, data: have, had: true})
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	return fn()
}
