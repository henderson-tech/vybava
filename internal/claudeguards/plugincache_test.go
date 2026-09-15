package claudeguards

import (
	"path/filepath"
	"strings"
	"testing"
)

const testHome = "/Users/x"

var cacheDir = filepath.Join(testHome, ".claude", "plugins", "cache", "kit", "vitrinka", "5.3.0")

// An install whose working directory is the plugin cache is the whole point of
// the rule.
func TestInstallInThePluginCacheCwdIsBlocked(t *testing.T) {
	manager, target := pluginInstallMatch("bun install", cacheDir, testHome)
	if manager != "bun" {
		t.Fatalf("manager %q, want bun", manager)
	}
	if target != cacheDir {
		t.Fatalf("target %q, want %q", target, cacheDir)
	}
}

// The two evasions the neighbouring rules also cover: walk in first, or name
// the directory on the command line.
func TestCdIntoTheCacheAndExplicitDirFlagsAreBlocked(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  string
		cwd  string
	}{
		{name: "cd then install", cmd: "cd " + cacheDir + " && bun install", cwd: "/Users/x/Work/repo"},
		{name: "subshell cd", cmd: "(cd " + cacheDir + " && npm ci)", cwd: "/Users/x/Work/repo"},
		{name: "tilde cd", cmd: "cd ~/.claude/plugins/cache/kit/vitrinka/5.3.0 && pnpm install", cwd: "/Users/x/Work/repo"},
		{name: "npm --prefix", cmd: "npm install --prefix " + cacheDir, cwd: "/Users/x/Work/repo"},
		{name: "bun --cwd=", cmd: "bun install --cwd=" + cacheDir, cwd: "/Users/x/Work/repo"},
		{name: "pnpm --dir", cmd: "pnpm --dir " + cacheDir + " add left-pad", cwd: "/Users/x/Work/repo"},
		{name: "relative cd from the plugin home", cmd: "cd kit/vitrinka/5.3.0 && yarn", cwd: filepath.Join(testHome, ".claude/plugins/cache")},
		{name: "leading assignment cannot disarm it", cmd: "CI=1 bun add left-pad", cwd: cacheDir},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if manager, _ := pluginInstallMatch(tc.cmd, tc.cwd, testHome); manager == "" {
				t.Fatalf("install into the cache was not blocked: %s", tc.cmd)
			}
		})
	}
}

// Reading and running inside the cache is legitimate — sessions load skill
// files from there constantly — and an install outside it is nobody's business.
func TestReadsAndInstallsOutsideTheCacheAreAllowed(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  string
		cwd  string
	}{
		{name: "listing the cache", cmd: "ls -la " + cacheDir, cwd: cacheDir},
		{name: "reading a skill", cmd: "cat " + cacheDir + "/skills/publish/SKILL.md", cwd: "/Users/x/Work/repo"},
		{name: "running a script from the cache", cmd: "bun run build", cwd: cacheDir},
		{name: "npm run in the cache", cmd: "npm run lint", cwd: cacheDir},
		{name: "install in a real repo", cmd: "bun install", cwd: "/Users/x/Work/repo"},
		{name: "install in the marketplaces tree is not the cache rule", cmd: "grep -rn node_modules " + cacheDir, cwd: "/Users/x/Work/repo"},
		{name: "a quoted mention is not a command", cmd: `git commit -m "cd ` + cacheDir + ` && bun install was the bug"`, cwd: "/Users/x/Work/repo"},
		{name: "cd out again before installing", cmd: "cd " + cacheDir + " && cd /Users/x/Work/repo && bun install", cwd: "/Users/x/Work/repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if manager, target := pluginInstallMatch(tc.cmd, tc.cwd, testHome); manager != "" {
				t.Fatalf("legitimate command was blocked: %s (as %s in %s)", tc.cmd, manager, target)
			}
		})
	}
}

// The denial names the offending path, the sanctioned move, and the ask that
// makes this rule a detector rather than only a block.
func TestDenialNamesThePathTheAlternativeAndAsksWhatCausedIt(t *testing.T) {
	text := pluginCacheDenial("bun", cacheDir).Text()
	for _, want := range []string{
		"plugincache:package-install",
		cacheDir,
		"SOURCE REPO",
		"name the workflow that led here",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("denial text missing %q:\n%s", want, text)
		}
	}
}
