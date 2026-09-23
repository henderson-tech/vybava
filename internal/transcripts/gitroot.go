package transcripts

import (
	"os"
	"path/filepath"
	"strings"
)

// GitRoot resolves a working directory to the repository that owns it:
//
//   - the nearest ancestor holding a .git DIRECTORY is the root;
//   - a .git FILE ("gitdir: …") is a linked worktree or a submodule: its
//     gitdir's commondir leads to the main repository's .git, whose parent is
//     the root; a gitdir without commondir (a submodule) is its own repo.
//
// Worktrees anywhere on disk fold into their repository — a path heuristic on
// ".worktrees/" would miss the ones created elsewhere. A cwd that no longer
// exists (a removed worktree) resolves from its nearest existing ancestor.
// exact is false when no repository was found or the cwd is gone, so callers
// can cache exact answers and re-resolve the rest.
func GitRoot(cwd string) (root string, exact bool) {
	if cwd == "" {
		return "", false
	}
	cwd = filepath.Clean(cwd)
	exact = true
	if _, err := os.Stat(cwd); err != nil {
		exact = false
	}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		dotgit := filepath.Join(dir, ".git")
		info, err := os.Lstat(dotgit)
		if err == nil {
			if info.IsDir() {
				return dir, exact
			}
			if main, ok := linkedRoot(dotgit); ok {
				return main, exact
			}
			return dir, exact
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return cwd, false
		}
	}
}

// linkedRoot follows a .git file to the main repository's working tree.
func linkedRoot(dotgit string) (string, bool) {
	raw, err := os.ReadFile(dotgit)
	if err != nil {
		return "", false
	}
	line, ok := strings.CutPrefix(strings.TrimSpace(string(raw)), "gitdir:")
	if !ok {
		return "", false
	}
	gitdir := strings.TrimSpace(line)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(filepath.Dir(dotgit), gitdir)
	}
	common, err := os.ReadFile(filepath.Join(gitdir, "commondir"))
	if err != nil {
		return "", false // a submodule: its own repository
	}
	commonDir := strings.TrimSpace(string(common))
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(gitdir, commonDir)
	}
	commonDir = filepath.Clean(commonDir)
	if filepath.Base(commonDir) != ".git" {
		return "", false // a bare repository has no working tree to name
	}
	return filepath.Dir(commonDir), true
}
