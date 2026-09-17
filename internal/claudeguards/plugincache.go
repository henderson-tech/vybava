package claudeguards

import (
	"os"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// guardPluginCache — nothing installs packages into the Claude Code plugin
// cache.
//
// ~/.claude/plugins/cache/<marketplace>/<plugin>/<version>/ is a clone of a
// plugin's published tree. Measured on this machine: 18 cached versions of one
// plugin carrying ~478 MB of node_modules/.bun each, 8.4 GB in total, against
// a ~520 KB surface (skills/, agents/, .claude-plugin/) that is all the loader
// ever reads. Three things were ruled out — Claude Code does not install into
// plugin caches, node_modules is not tracked in the source repo, and the
// marketplace is a GitHub source so the clone cannot carry it. Every
// node_modules appeared HOURS after its version was installed (active version:
// installed 12:02, node_modules created 14:00), and the culprit was never
// identified.
//
// So this rule is two things at once: it stops the growth, and when it fires
// it names the workflow doing it. There is deliberately no bypass — there is
// no legitimate reason to install packages into an installed plugin's cache.
// ---------------------------------------------------------------------------

// pluginHomeRel is the protected tree, relative to the user's home.
var pluginHomeRel = filepath.Join(".claude", "plugins")

// packageManagers are the tools that write a dependency tree.
var packageManagers = map[string]bool{
	"bun": true, "npm": true, "pnpm": true, "yarn": true,
}

// installVerbs WRITE into the working directory. `run`, `list`, `why`, `exec`
// and friends are absent on purpose: reading a plugin's files — including
// running something from it — stays allowed.
var installVerbs = map[string]bool{
	"install": true, "i": true, "add": true, "ci": true,
	"update": true, "upgrade": true, "up": true, "rebuild": true, "link": true,
}

// dirFlags carry an explicit target directory, which beats the cwd. Each is
// accepted both as `--flag value` and `--flag=value`.
var dirFlags = []string{"--cwd", "--prefix", "--dir", "--install-dir", "-C"}

// pluginInstallMatch reports the package manager and the directory it would
// write to when a command would install into the plugin cache, or "" when it
// would not. Pure: home and cwd are passed in so the whole rule is testable.
func pluginInstallMatch(cmd, cwd, home string) (manager, target string) {
	guarded := filepath.Join(home, pluginHomeRel)
	// `cd` moves the target for every later segment of the same command, so
	// the segments are walked in order with a running directory.
	here := cwd
	for _, seg := range segments(cmd) {
		if textOnly(seg) {
			continue
		}
		fields := strings.Fields(trimAssignments(trimSubshell(seg)))
		if len(fields) == 0 {
			continue
		}
		word := commandWord(seg)
		if word == "cd" && len(fields) > 1 {
			here = resolveDir(unquote(fields[1]), here, home)
			continue
		}
		if !packageManagers[word] {
			continue
		}
		if !installing(word, fields) {
			continue
		}
		// An explicit target directory wins over the cwd; otherwise the
		// install lands wherever the shell currently is.
		where := here
		if flagged, ok := dirFlagValue(fields, here, home); ok {
			where = flagged
		}
		if under(where, guarded) {
			return word, where
		}
	}
	return "", ""
}

// installing reports whether this argv is an install rather than a read. The
// first positional word after the manager is the subcommand — but a
// value-taking flag eats the word after it, so `pnpm --dir <path> add` must
// not read the path as its verb. A bare `yarn` with no subcommand is
// yarn-classic's install.
func installing(manager string, fields []string) bool {
	for i := 1; i < len(fields); i++ {
		f := fields[i]
		if !strings.HasPrefix(f, "-") {
			return installVerbs[f]
		}
		if takesValue(f) {
			i++ // skip the value this flag consumes
		}
	}
	return manager == "yarn"
}

// takesValue reports whether a flag consumes the following word. Only the
// separated form does — `--dir=<path>` carries its own value.
func takesValue(field string) bool {
	for _, flag := range dirFlags {
		if field == flag {
			return true
		}
	}
	return false
}

// dirFlagValue extracts an explicit target directory from the argv.
func dirFlagValue(fields []string, here, home string) (string, bool) {
	for i, f := range fields {
		for _, flag := range dirFlags {
			if f == flag && i+1 < len(fields) {
				return resolveDir(unquote(fields[i+1]), here, home), true
			}
			if strings.HasPrefix(f, flag+"=") {
				return resolveDir(unquote(strings.TrimPrefix(f, flag+"=")), here, home), true
			}
		}
	}
	return "", false
}

// resolveDir expands a leading ~ and resolves a relative path against the
// directory the shell is currently in.
func resolveDir(path, here, home string) string {
	switch {
	case path == "~":
		return home
	case strings.HasPrefix(path, "~/"):
		return filepath.Join(home, path[2:])
	case filepath.IsAbs(path):
		return filepath.Clean(path)
	case here == "":
		return path
	default:
		return filepath.Join(here, path)
	}
}

func unquote(s string) string { return strings.Trim(s, `"'`) }

// under reports whether path is the guarded tree or inside it.
func under(path, guarded string) bool {
	if path == "" {
		return false
	}
	clean := filepath.Clean(path)
	return clean == guarded || strings.HasPrefix(clean, guarded+string(filepath.Separator))
}

func guardPluginCache(in *HookInput) *Denial {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil // fail open: no home, no rule
	}
	cwd := in.CWD
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	manager, target := pluginInstallMatch(in.ToolInput.Command, cwd, home)
	if manager == "" {
		return nil
	}
	return pluginCacheDenial(manager, target)
}

// pluginCacheDenial is the block Claude reads. It names the sanctioned move
// AND asks what led here — the rule is the only detector we have for whichever
// workflow keeps installing into the cache.
func pluginCacheDenial(manager, target string) *Denial {
	return deny("plugincache:package-install",
		manager+" would install into the Claude Code plugin cache:\n  "+target+"\n\n"+
			"That tree is a published plugin's clone, re-created per version, and nothing past skills/, agents/ and .claude-plugin/ is ever read. "+
			"Installing there is what turned one plugin into 8.4 GB of node_modules on this machine.",
		"Run the install in the plugin's SOURCE REPO checkout instead — never under ~/.claude/plugins/.\n\n"+
			"If you did not mean to do this, say so and name the workflow that led here (the skill, command or script). "+
			"Nothing is supposed to install into the cache, the culprit was never found, and this block is how we find it.")
}
