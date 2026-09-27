package claudeguards

import (
	"path/filepath"
	"strings"

	"github.com/henderson-tech/vybava/internal/shellseg"
)

// cappedValue reports whether one of these flags carries a real limit. A flag
// whose value is `all` names the uncapped default, so it caps nothing.
func cappedValue(f []string, flags ...string) bool {
	for i := 2; i < len(f); i++ {
		for _, flag := range flags {
			if f[i] == flag && i+1 < len(f) {
				return !strings.EqualFold(f[i+1], "all")
			}
			if strings.HasPrefix(f[i], flag+"=") {
				return !strings.EqualFold(strings.TrimPrefix(f[i], flag+"="), "all")
			}
		}
	}
	return false
}

// gitGlobalWithValue are git's global options that consume the next field;
// gitGlobalFlags stand alone. Both sit between `git` and the subcommand, so
// `git -C repo log` is still a git log — the pair below read it as `git -C`,
// found no rule, and let a fully uncapped log through.
var gitGlobalWithValue = map[string]bool{
	"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true,
}

var gitGlobalFlags = map[string]bool{
	"-P": true, "--no-pager": true, "--paginate": true, "--bare": true,
	"--no-replace-objects": true, "--literal-pathspecs": true,
}

func skipGitGlobals(f []string) []string {
	if len(f) == 0 || shellseg.CommandWord(f[0]) != "git" {
		return f
	}
	i := 1
	for i < len(f) {
		switch {
		case gitGlobalWithValue[f[i]]:
			i += 2
		case gitGlobalFlags[f[i]]:
			i++
		default:
			if k := strings.IndexByte(f[i], '='); k > 0 && gitGlobalWithValue[f[i][:k]] {
				i++
				continue
			}
			return append(f[:1:1], f[i:]...)
		}
	}
	return f[:1]
}

// blobSpecs returns a `git show`'s arguments when every revision it names is a
// <rev>:<path> blob spec, nil otherwise — a bare commit prints its whole diff,
// and --textconv/--filters print a converter's output, not the blob.
func blobSpecs(args []string) []string {
	var specs []string
	for _, a := range args {
		if a == "--textconv" || a == "--filters" {
			return nil
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		if !strings.Contains(a, ":") {
			return nil
		}
		specs = append(specs, a)
	}
	return specs
}

// gitRepoGlobals are the global options that pick the repository; the blob is
// measured in the same one. -c stays out: the hook never runs caller config.
var gitRepoGlobals = map[string]bool{"--git-dir": true, "--work-tree": true, "--namespace": true}

// gitShowBlobs recognises `git [globals] show <rev>:<path>…` and returns the
// directory git runs in (the cwd moved by each -C), the repository-selecting
// globals as `--opt=value`, and the blob specs; nil specs for any other command.
func gitShowBlobs(fields []string, cwd string) (dir string, repo, specs []string) {
	g := skipGitGlobals(fields)
	if len(g) < 3 || g[0] != "git" || g[1] != "show" {
		return "", nil, nil
	}
	if specs = blobSpecs(g[2:]); specs == nil {
		return "", nil, nil
	}
	dir = cwd
	for i := 1; i < len(fields) && strings.HasPrefix(fields[i], "-"); i++ {
		name, value, joined := strings.Cut(fields[i], "=")
		if !joined && gitGlobalWithValue[name] && i+1 < len(fields) {
			i++
			value = fields[i]
		}
		switch {
		case name == "-C":
			dir = resolvePath(value, dir)
		case gitRepoGlobals[name]:
			repo = append(repo, name+"="+value)
		}
	}
	return dir, repo, specs
}

// blobPath is the working-tree file a <rev>:<path> spec names, so the
// protected-file rules (noRead, transcripts, lok catalogs) judge it as they
// judge cat: `rev:./x` and `rev:../x` resolve against dir, any other path
// against the root of the repository the command selects — its --work-tree,
// else the checkout holding its --git-dir, else dir's checkout. "" when there
// is none to name (a bare --git-dir has no working tree).
func blobPath(spec, dir string, repo []string) string {
	_, p, _ := strings.Cut(spec, ":")
	if p == "" {
		return ""
	}
	if strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") {
		return resolvePath(p, dir)
	}
	root, gitDir, workTree := checkoutRoot(dir), "", ""
	for _, g := range repo {
		switch name, value, _ := strings.Cut(g, "="); name {
		case "--git-dir":
			gitDir = resolvePath(value, dir)
		case "--work-tree":
			workTree = resolvePath(value, dir)
		}
	}
	switch {
	case workTree != "":
		root = workTree
	case gitDir != "" && filepath.Base(gitDir) == ".git":
		root = filepath.Dir(gitDir)
	case gitDir != "":
		root = ""
	}
	if root == "" {
		return ""
	}
	return filepath.Join(root, p)
}

func unboundedOutput(segment string, cfg Config) *Denial {
	f := shellseg.Fields(shellseg.TrimAssignments(shellseg.TrimSubshell(segment)))
	f = skipGitGlobals(f)
	if len(f) < 2 {
		return nil
	}
	command := f[0] + " " + f[1]
	if len(f) > 2 && f[0] == "gh" {
		command += " " + f[2]
	}
	if cfg.UnboundedCommands != nil {
		enabled := false
		for _, c := range cfg.UnboundedCommands {
			if c == command {
				enabled = true
			}
		}
		if !enabled {
			return nil
		}
	}
	has := func(flags ...string) bool {
		for _, a := range f[2:] {
			for _, flag := range flags {
				if a == flag || strings.HasPrefix(a, flag+"=") {
					return true
				}
			}
		}
		return false
	}
	cappedCount := func() bool {
		for i, a := range f[2:] {
			if len(a) > 1 && a[0] == '-' && isDigits(a[1:]) {
				return true
			}
			if strings.HasPrefix(a, "-n") && isDigits(strings.TrimPrefix(a, "-n")) {
				return true
			}
			if strings.HasPrefix(a, "--max-count=") && isDigits(strings.TrimPrefix(a, "--max-count=")) {
				return true
			}
			if (a == "-n" || a == "--max-count") && i+3 < len(f) && isDigits(f[i+3]) {
				return true
			}
		}
		return false
	}
	fix := ""
	switch command {
	case "docker logs":
		// docker spells its uncapped default `--tail all` / `-n all`, so the
		// flag being present is not evidence of a cap.
		if !has("--since") && !cappedValue(f, "--tail", "-n") {
			fix = "docker logs --tail 200 <container>"
		}
	case "gh run view":
		if has("--log", "--log-failed") {
			fix = "gh run view <id> --log-failed | tail -200"
		}
	case "git log":
		if !cappedCount() {
			fix = "git log -n 20 --oneline"
		}
	case "git diff", "git show":
		// `git show <rev>:<path>` prints one file as it is at rev: a file read,
		// which dumpBudget measures like cat. Refusing it as a diff was 300 of
		// the 304 git-show denials in the 2026-09-25 field audit.
		if f[1] == "show" && blobSpecs(f[2:]) != nil {
			return nil
		}
		pathBound := false
		for i, a := range f {
			if a == "--" && i+1 < len(f) {
				pathBound = true
			}
		}
		// `--output=<file>` / `--output <file>` sends the diff to a file, not
		// to context (10 escapes on that shape by 2026-09-27).
		if !has("--stat", "--name-only", "--name-status", "--numstat", "--shortstat", "--output") && !pathBound {
			// Keep the revisions the caller typed; a suggestion that drops them
			// is a different command from the one they wanted.
			fix = strings.Join(f, " ") + " --stat (or -- <path>)"
		}
	// Test runners are deliberately absent: a passing suite prints a few lines,
	// a failing one puts the part worth reading at the end, and every repo here
	// documents a bare `go test ./...` / `bun test` as its verification step.
	// A guard that refuses the documented verify command only teaches people to
	// route around the guard. Add one through guards.unboundedCommands if a
	// specific suite really does flood.
	default:
		if cfg.UnboundedCommands != nil {
			fix = strings.Join(f, " ") + " | tail -100"
		}
	}
	if fix == "" {
		return nil
	}
	return deny("context:unbounded-output", "This command can flood context. Cap or filter its output: "+fix, contextReadEscape)
}
