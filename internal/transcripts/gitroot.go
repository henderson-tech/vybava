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
// exists (a removed worktree) resolves from its nearest existing ancestor;
// when no ancestor holds a repository either (the whole checkout moved or was
// deleted), a path inside a ".worktrees/" or ".claude/worktrees/" directory
// still folds lexically into the directory that held it.
// exact is false when no repository was found or the cwd is gone, so callers
// can cache exact answers and re-resolve the rest.
func GitRoot(cwd string) (root string, exact bool) {
	root, exact, _ = gitRoot(cwd)
	return root, exact
}

// RepoRoot is GitRoot for a path an agent wrote to — a file or a directory,
// which need not exist any more. ok is false when no repository holds it and
// no worktree directory names its owner: a scratch file under /tmp is no
// project's work.
func RepoRoot(path string) (root string, ok bool) {
	root, _, ok = gitRoot(path)
	return root, ok
}

// OnDiskCase spells an existing path the way its directories store each name.
// On a case-insensitive volume (macOS by default) an agent's
// /work/adf/forge reaches /work/ADF/forge, and the two spellings must not be
// two projects. A name with no entry on disk is kept as written.
func OnDiskCase(path string) string {
	path = filepath.Clean(path)
	parent := filepath.Dir(path)
	if parent == path {
		return path
	}
	parent, name := OnDiskCase(parent), filepath.Base(path)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return filepath.Join(parent, name)
	}
	folded := ""
	for _, e := range entries {
		if e.Name() == name {
			return filepath.Join(parent, name)
		}
		if folded == "" && strings.EqualFold(e.Name(), name) {
			folded = e.Name()
		}
	}
	if folded != "" {
		name = folded
	}
	return filepath.Join(parent, name)
}

func gitRoot(cwd string) (root string, exact, found bool) {
	if cwd == "" {
		return "", false, false
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
				return dir, exact, true
			}
			if main, ok := linkedRoot(dotgit); ok {
				return main, exact, true
			}
			return dir, exact, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			owner := worktreeOwner(cwd)
			return owner, false, owner != cwd
		}
	}
}

// worktreeOwner cuts a path at its worktree-holding directory:
// /a/repo/.worktrees/x/sub and /a/repo/.claude/worktrees/x both give /a/repo.
func worktreeOwner(path string) string {
	slashed := filepath.ToSlash(path)
	for _, marker := range []string{"/.claude/worktrees/", "/.worktrees/"} {
		if i := strings.Index(slashed, marker); i > 0 {
			return filepath.FromSlash(slashed[:i])
		}
	}
	return path
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
