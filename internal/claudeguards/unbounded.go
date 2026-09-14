package claudeguards

import "strings"

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
	if len(f) == 0 || commandWord(f[0]) != "git" {
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

func unboundedOutput(segment string, cfg Config) *Denial {
	f := shellFields(trimAssignments(trimSubshell(segment)))
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
		pathBound := false
		for i, a := range f {
			if a == "--" && i+1 < len(f) {
				pathBound = true
			}
		}
		if !has("--stat", "--name-only", "--name-status", "--numstat", "--shortstat") && !pathBound {
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
