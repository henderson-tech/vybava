package claudeguards

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/henderson-tech/vybava/internal/shellseg"
)

// ---------------------------------------------------------------------------
// context:heavy-walk - find and bfs ignore .gitignore, so a walk that is not
// rooted at the disk still stats every file of every node_modules, Pods tree
// and build cache below it. On 2026-09-27 a Codex session ran `find
// ~/Work/Projects -path '*/.worktrees/*' -prune -o -name devbox.yaml -print`:
// it pruned the worktrees and crawled every repo's node_modules for minutes at
// 44% CPU. `rg --files -g devbox.yaml ~/Work/Projects` answers the same
// question in about 2 s because it honours each nested repo's .gitignore.
//
// The rule probes the root a few levels deep for a heavy directory the command
// does not prune, and for a nested repository's .git when the root sits above
// the repos. The probe is bounded: an undecided tree is allowed (fail open).
// ---------------------------------------------------------------------------

// heavyDirs are the directory names whose content is installed or generated,
// never searched by hand, and large enough to pin a core when walked.
var heavyDirs = map[string]bool{
	"node_modules": true, ".pnpm-store": true, "Pods": true, "DerivedData": true,
	".next": true, ".turbo": true, ".expo": true, ".gradle": true, ".cache": true,
}

// heavyProbeDepth and heavyProbeDirs bound the probe: node_modules sits at
// depth 1 of a repo and depth 3 of ~/Work/Projects, and a tree that holds none
// within the first dirs read is left alone.
const (
	heavyProbeDepth = 4
	heavyProbeDirs  = 1500
)

// fdNoIgnore are the fd flags that turn its .gitignore handling off, which
// makes it walk exactly what find walks.
var fdNoIgnore = map[string]bool{
	"-u": true, "-uu": true, "-I": true, "--no-ignore": true, "--unrestricted": true,
}

// pruned reports whether the walker's own arguments keep it out of a
// directory named name: find's -prune or bfs's -exclude next to a pattern
// naming it, or fd's --exclude. `-not -path '*/node_modules/*'` filters the
// output but still descends, so it does not count.
func pruned(argv []string, name string) bool {
	if argv[0] == "fd" {
		for i, a := range argv {
			if (a == "-E" || a == "--exclude") && i+1 < len(argv) && strings.Contains(argv[i+1], name) {
				return true
			}
			if strings.HasPrefix(a, "--exclude=") && strings.Contains(a, name) {
				return true
			}
		}
		return false
	}
	var prunes, names bool
	for _, a := range argv[1:] {
		prunes = prunes || a == "-prune" || a == "-exclude"
		names = names || strings.Contains(a, name)
	}
	return prunes && names
}

// resolveWalkRoot turns a root argument into an absolute path, or "" when it
// cannot be known without a shell (another variable, a glob).
func resolveWalkRoot(root, cwd, home string) string {
	r := strings.TrimSpace(root)
	for _, p := range []string{"~", "$HOME", "${HOME}"} {
		if r == p {
			return home
		}
		if strings.HasPrefix(r, p+"/") {
			r = filepath.Join(home, r[len(p)+1:])
			break
		}
	}
	if strings.ContainsAny(r, "$`*?[") {
		return ""
	}
	if !filepath.IsAbs(r) {
		r = filepath.Join(cwd, r)
	}
	return filepath.Clean(r)
}

// insideHeavy reports whether dir already lies in a heavy directory: the
// agent named it as the root, so walking it is the point.
func insideHeavy(dir string) bool {
	for _, part := range strings.Split(dir, string(filepath.Separator)) {
		if heavyDirs[part] {
			return true
		}
	}
	return false
}

// insideRepo reports whether dir or one of its parents holds a .git.
func insideRepo(dir string) bool {
	for d := dir; ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return true
		}
		if filepath.Dir(d) == d {
			return false
		}
	}
}

// heavyUnder returns the first heavy directory below root that argv does not
// prune, breadth-first so the shallowest is found first. A nested .git counts
// only when root sits above the repositories. Symlinks are never followed.
func heavyUnder(root string, argv []string) string {
	gitHeavy := !insideRepo(root)
	type dir struct {
		path  string
		depth int
	}
	queue, read := []dir{{root, 0}}, 0
	for len(queue) > 0 && read < heavyProbeDirs {
		d := queue[0]
		queue = queue[1:]
		entries, err := os.ReadDir(d.path)
		read++
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name, path := e.Name(), filepath.Join(d.path, e.Name())
			if heavyDirs[name] || (gitHeavy && name == ".git") {
				if !pruned(argv, name) {
					return path
				}
				continue
			}
			if d.depth+1 < heavyProbeDepth {
				queue = append(queue, dir{path, d.depth + 1})
			}
		}
	}
	return ""
}

// heavyWalkMatch returns the offending walker invocation and the heavy
// directory it would descend into, or "" when every walker in cmd is capped,
// prunes what it meets, or is fd honouring .gitignore.
func heavyWalkMatch(cmd, cwd string) (command, heavy string) {
	home, _ := os.UserHomeDir()
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	for _, seg := range shellseg.Segments(cmd) {
		if textOnly(seg) {
			continue
		}
		argv := chainCommand(seg, walkers)
		if argv == nil {
			continue
		}
		if argv[0] == "fd" && !hasAny(argv[1:], fdNoIgnore) {
			continue
		}
		roots, capped := walkRoots(argv)
		if capped {
			continue
		}
		if len(roots) == 0 {
			roots = []string{cwd}
		}
		for _, r := range roots {
			abs := resolveWalkRoot(r, cwd, home)
			if abs == "" || insideHeavy(abs) {
				continue
			}
			if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
				continue
			}
			if h := heavyUnder(abs, argv); h != "" {
				return strings.Join(argv, " "), h
			}
		}
	}
	return "", ""
}

func hasAny(args []string, set map[string]bool) bool {
	for _, a := range args {
		if set[a] {
			return true
		}
	}
	return false
}

const heavyWalkMsg = `%s

descends into %s. find and bfs ignore .gitignore, so they stat every file
of every node_modules, build cache and nested repository under the root; on
2026-09-27 'find ~/Work/Projects -path */.worktrees/* -prune -o -name devbox.yaml
-print' crawled every repo's node_modules for minutes, where
'rg --files -g devbox.yaml ~/Work/Projects' answers in about 2 s.

List files the way git sees them (every nested .gitignore honoured):
                                 rg --files -g '<glob>' <root>
                                 git -C <repo> ls-files '<glob>'
Or prune it (-not -path still descends into it):
                                 find <root> -name %s -prune -o -name <file> -print
Or cap the depth:                find <root> -maxdepth 3 -name <file>`

func guardHeavyWalk(in *HookInput) *Denial {
	command, heavy := heavyWalkMatch(in.ToolInput.Command, in.CWD)
	if command == "" {
		return nil
	}
	// No escape hatch: a prune, a depth cap or a root inside the heavy tree is
	// the sanctioned form, and each is cheaper than the blocked walk.
	return deny("context:heavy-walk", fmt.Sprintf(heavyWalkMsg, command, heavy, filepath.Base(heavy)), "")
}
