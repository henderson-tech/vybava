package claudeguards

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// context:root-walk - a find/bfs/fd rooted at the disk, the home directory or
// one of the macOS top-level trees stats millions of entries and pins every
// core for tens of minutes. On 2026-09-19 `bfs / -name home-grid-1.png -newer
// /tmp/hcg-e2e.log` ran 21 minutes at 175% CPU and made the Mac unusable. A
// depth cap makes the same walk bounded, and a whole-disk name lookup has an
// indexed answer in `mdfind -name`.
// ---------------------------------------------------------------------------

// walkers are the recursive directory walkers this rule knows.
var walkers = map[string]bool{"find": true, "bfs": true, "fd": true}

// walkerOptionFlags are the leading find/bfs options that precede the roots
// and take no value. `-d` is depth-FIRST here, not a depth cap: only fd spells
// its cap `-d N`.
var walkerOptionFlags = map[string]bool{
	"-E": true, "-H": true, "-L": true, "-P": true, "-X": true,
	"-d": true, "-s": true, "-x": true, "-O": true,
}

// fdValueFlags take the next token as their value, so that token is never a
// root. --search-path IS a root and is handled separately.
var fdValueFlags = map[string]bool{
	"-d": true, "--max-depth": true, "--maxdepth": true, "--min-depth": true,
	"--exact-depth": true, "-e": true, "--extension": true, "-t": true,
	"--type": true, "-E": true, "--exclude": true, "--ignore-file": true,
	"-c": true, "--color": true, "-j": true, "--threads": true, "-S": true,
	"--size": true, "--changed-within": true, "--changed-before": true,
	"--changed-since": true, "--newer": true, "--older": true, "-o": true,
	"--owner": true, "--base-directory": true, "--path-separator": true,
	"--batch-size": true, "--max-buffer-time": true, "--max-results": true,
	"--format": true, "--and": true,
}

// chainCommand returns the argv of the first command in a launcher chain whose
// word is one of names, or nil. Like commandChainHas, it only follows a real
// wrapper chain (`sudo find /`, `timeout 30 find /`, `nice -n 5 bfs /`); a
// quoted mention is one field and never matches.
func chainCommand(seg string, names map[string]bool) []string {
	toks := shellFields(trimAssignments(seg))
	for i, t := range toks {
		if j := strings.LastIndexByte(t, '/'); j >= 0 {
			t = t[j+1:]
		}
		if names[t] {
			return append([]string{t}, toks[i+1:]...)
		}
		if i == 0 && !commandRunners[t] {
			return nil
		}
	}
	return nil
}

// walkRoots returns the directories a find/bfs/fd invocation walks, and
// whether a depth cap bounds it. An empty roots list means the walk starts at
// the current directory.
func walkRoots(argv []string) (roots []string, capped bool) {
	args := argv[1:]
	if argv[0] == "fd" {
		return fdRoots(args)
	}
	i := 0
	for ; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-f" && i+1 < len(args):
			i++
			roots = append(roots, args[i])
		case walkerOptionFlags[a]:
		case a == "--":
		case strings.HasPrefix(a, "-"), a == "(", a == "!":
			i = len(args)
		default:
			roots = append(roots, a)
		}
	}
	for _, a := range args {
		if a == "-maxdepth" || strings.HasPrefix(a, "-maxdepth=") {
			capped = true
		}
	}
	return roots, capped
}

// fdRoots reads `fd [OPTIONS] [pattern] [path...]`: the first positional is
// the pattern, the rest are roots, and --search-path names a root explicitly.
// Parsing stops at --exec, whose remainder is a command line.
func fdRoots(args []string) (roots []string, capped bool) {
	positional := 0
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-x" || a == "--exec" || a == "-X" || a == "--exec-batch":
			return roots, capped
		case a == "--search-path" && i+1 < len(args):
			i++
			roots = append(roots, args[i])
		case strings.HasPrefix(a, "--search-path="):
			roots = append(roots, strings.TrimPrefix(a, "--search-path="))
		case a == "-d" || a == "--max-depth" || a == "--maxdepth" || a == "--exact-depth":
			capped = true
			i++
		case strings.HasPrefix(a, "--max-depth=") || strings.HasPrefix(a, "--maxdepth=") ||
			strings.HasPrefix(a, "--exact-depth=") ||
			(strings.HasPrefix(a, "-d") && isDigits(strings.TrimPrefix(a, "-d"))):
			capped = true
		case fdValueFlags[a]:
			i++
		case strings.HasPrefix(a, "-"):
		default:
			if positional > 0 {
				roots = append(roots, a)
			}
			positional++
		}
	}
	return roots, capped
}

// unboundedRoot reports whether root names the disk, the home directory or a
// macOS top-level tree. The literal home (`/Users/<name>`) is recognised for
// any name, not just this user's: `find /Users/shared` is the same walk.
func unboundedRoot(root, home string) bool {
	r := strings.TrimSpace(root)
	switch r {
	case "~", "$HOME", "${HOME}":
		return true
	}
	if strings.HasPrefix(r, "$HOME/") || strings.HasPrefix(r, "${HOME}/") || strings.HasPrefix(r, "~/") {
		rest := r[strings.Index(r, "/")+1:]
		return strings.Trim(rest, "/") == ""
	}
	if !strings.HasPrefix(r, "/") {
		return false
	}
	r = filepath.Clean(r)
	switch r {
	case "/", "/Users", "/Volumes", "/Library":
		return true
	}
	if home != "" && r == filepath.Clean(home) {
		return true
	}
	parts := strings.Split(strings.TrimPrefix(r, "/"), "/")
	return len(parts) == 2 && parts[0] == "Users"
}

// rootWalkMatch returns the offending walker invocation and the root it would
// walk, or "" when every walker in cmd is scoped or depth-capped. Pure apart
// from the home lookup - unit-testable with an explicit cwd.
func rootWalkMatch(cmd, cwd string) (command, root string) {
	home, _ := os.UserHomeDir()
	for _, seg := range segments(cmd) {
		if textOnly(seg) {
			continue
		}
		argv := chainCommand(seg, walkers)
		if argv == nil {
			continue
		}
		roots, capped := walkRoots(argv)
		if capped {
			continue
		}
		if len(roots) == 0 {
			dir := cwd
			if dir == "" {
				dir, _ = os.Getwd()
			}
			if unboundedRoot(dir, home) {
				return strings.Join(argv, " "), dir + " (the current directory, no root given)"
			}
			continue
		}
		for _, r := range roots {
			if unboundedRoot(r, home) {
				return strings.Join(argv, " "), r
			}
		}
	}
	return "", ""
}

const rootWalkMsg = `%s

walks the whole disk from %s. A recursive walk rooted there stats millions of
entries and pins every core for tens of minutes; on 2026-09-19
'bfs / -name home-grid-1.png -newer /tmp/hcg-e2e.log' ran 21 min at 175%% CPU
and made the Mac unusable.

Scope the root to the directory that can hold the file (the artifact directory,
the repo, the worktree):        find <repo>/<dir> -name <file>
Or cap the depth:                find / -maxdepth 3 -name <file>   (fd: -d 3)
A whole-disk lookup by name is a Spotlight query, indexed and instant:
                                 mdfind -name <file>`

func guardRootWalk(in *HookInput) *Denial {
	command, root := rootWalkMatch(in.ToolInput.Command, in.CWD)
	if command == "" {
		return nil
	}
	// No escape hatch: -maxdepth or a scoped root IS the sanctioned form, and
	// there is no walk of the whole disk that an agent needs.
	return deny("context:root-walk", fmt.Sprintf(rootWalkMsg, command, root), "")
}
