package gitkit

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// Every git/gh call a verb makes is anchored to ONE explicit repository
// root. The harness Bash tool keeps a persistent shell whose cwd survives
// between calls, so a helper letting git infer the repo from ambient cwd
// answers about whatever repo the shell is parked in — silently and wrongly.
// Callers pass `--repo <abs>` (or `--repo=<abs>`, or GIT_SKILL_REPO); without
// one the ambient cwd's toplevel is used. A verb resolves the root once and
// threads it through, so one invocation can never straddle two repos.

// explicitRepoArg reads the anchor from argv or GIT_SKILL_REPO. ok is false
// when no anchor was given; `--repo=` yields ("", true) and fails resolution.
func explicitRepoArg(argv []string) (string, bool) {
	for i, a := range argv {
		if a == "--repo" {
			if i+1 < len(argv) && argv[i+1] != "" {
				return argv[i+1], true
			}
			break
		}
	}
	for _, a := range argv {
		if value, found := strings.CutPrefix(a, "--repo="); found {
			return value, true
		}
	}
	if env := strings.TrimSpace(os.Getenv("GIT_SKILL_REPO")); env != "" {
		return env, true
	}
	return "", false
}

// repoRoot returns the absolute toplevel this invocation operates on. An
// explicit anchor wins over cwd; a bad anchor fails loudly rather than
// falling back — a wrong-repo answer is the failure mode being eliminated.
func repoRoot(argv []string) (string, error) {
	dir, explicit := explicitRepoArg(argv)
	if explicit {
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			return "", fmt.Errorf("--repo is not an existing directory: %s", dir)
		}
	} else {
		// syscall.Getwd, not os.Getwd: the physical path, as Node's
		// process.cwd() reports it (os.Getwd trusts a symlinked $PWD).
		cwd, err := syscall.Getwd()
		if err != nil {
			return "", err
		}
		dir = cwd
	}
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("not inside a git repository: %s", dir)
	}
	return strings.TrimSpace(string(out)), nil
}
