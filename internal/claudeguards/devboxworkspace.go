package claudeguards

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// machine:devbox-workspace - a command this repo routes to the Devbox ONLY
// when the checkout already has a workspace there, run on the Mac anyway.
// Sibling of machine:devbox-only for work that is fine on the Mac in a bare
// worktree but belongs on the box once one is synced to it: FixIt's
// typechecks (2026-09-25; the api spec check is 3 GB and 60 s, and several at
// once froze the Mac at 50 GB of swap the day before). The repo lists the
// commands in guards.devboxWhenWorkspace (same RE2-per-segment matching as
// devboxOnly); the workspace fact is read from the devbox CLI's local
// registry, ~/.devbox/workspaces/<name>/workspace.yaml, whose apps carry the
// synced checkout path (`sync:`). A parked workspace keeps its record, so it
// still counts; `devbox down`/gc drop it. No network, no subprocess.
//
// The checkout is the git root above the command's cwd, compared for
// EQUALITY with the recorded path: worktrees nest inside the main clone
// (`.worktrees/<name>`), so a parent match would hand the main clone's
// workspace to every bare worktree under it.
// ---------------------------------------------------------------------------

// compileDevboxPatterns compiles a guards list; loadGuardConfig already
// rejected invalid patterns, so a compile error here only skips the entry.
func compileDevboxPatterns(list []string) []*regexp.Regexp {
	patterns := make([]*regexp.Regexp, 0, len(list))
	for _, p := range list {
		if re, err := regexp.Compile(p); err == nil {
			patterns = append(patterns, re)
		}
	}
	return patterns
}

// checkoutRoot is the nearest ancestor of dir (dir included) holding a .git
// entry - a worktree's .git FILE counts, so a nested worktree is its own
// checkout, never its main clone. "" when dir is outside any checkout.
func checkoutRoot(dir string) string {
	if dir == "" {
		return ""
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for d := filepath.Clean(abs); ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

// pathVariants is the cleaned path plus its symlink-resolved form when that
// differs, so /var vs /private/var or a symlinked ~/Work still compares equal.
func pathVariants(p string) []string {
	out := []string{filepath.Clean(p)}
	if resolved, err := filepath.EvalSymlinks(p); err == nil && filepath.Clean(resolved) != out[0] {
		out = append(out, filepath.Clean(resolved))
	}
	return out
}

// devboxWorkspacesDir is the devbox CLI's local workspace registry, resolved
// as the CLI resolves it: $DEVBOX_WORKSPACES_DIR, else ~/.devbox/workspaces.
func devboxWorkspacesDir() string {
	if dir := os.Getenv("DEVBOX_WORKSPACES_DIR"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".devbox", "workspaces")
}

// workspaceSyncPaths lists the Mac-side checkouts one registry entry syncs:
// its workspace.yaml apps[].sync and its rendered/mutagen.yaml sessions'
// alpha. Both are read because a third of the entries on a real Mac carry only
// the rendered file. name is the manifest's name, else "".
func workspaceSyncPaths(entry string) (name string, paths []string) {
	if raw, err := os.ReadFile(filepath.Join(entry, "workspace.yaml")); err == nil {
		var ws struct {
			Name string `yaml:"name"`
			Apps map[string]struct {
				Sync string `yaml:"sync"`
			} `yaml:"apps"`
		}
		if yaml.Unmarshal(raw, &ws) == nil {
			name = ws.Name
			for _, app := range ws.Apps {
				paths = append(paths, app.Sync)
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(entry, "rendered", "mutagen.yaml")); err == nil {
		var m struct {
			Sync map[string]struct {
				Alpha string `yaml:"alpha"`
			} `yaml:"sync"`
		}
		if yaml.Unmarshal(raw, &m) == nil {
			for _, session := range m.Sync {
				paths = append(paths, session.Alpha)
			}
		}
	}
	return name, paths
}

// devboxWorkspaceFor returns the name of the Devbox workspace whose synced
// checkout is exactly root, or "" when no local record says so.
func devboxWorkspaceFor(root string) string {
	dir := devboxWorkspacesDir()
	if dir == "" || root == "" {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	roots := pathVariants(root)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name, paths := workspaceSyncPaths(filepath.Join(dir, e.Name()))
		for _, sync := range paths {
			sync = strings.TrimSpace(sync)
			if !filepath.IsAbs(sync) {
				continue // empty, or a box-side path (devops:ws/…)
			}
			for _, s := range pathVariants(sync) {
				if slices.Contains(roots, s) {
					if name != "" {
						return name
					}
					return e.Name()
				}
			}
		}
	}
	return ""
}

func guardDevboxWhenWorkspace(in *HookInput) *Denial {
	cmd := in.ToolInput.Command
	if cmd == "" || escapeHatch(cmd, "CLAUDE_GUARDS_ALLOW_LOCAL_STACK") {
		return nil
	}
	cfg := in.guards()
	if len(cfg.DevboxWhenWorkspace) == 0 {
		return nil
	}
	// Every matching command is judged by the checkout it RUNS in (after its
	// cd / --cwd), never the session's: from the main clone, `(cd
	// .worktrees/x && tsc)` is judged by .worktrees/x, which may have no
	// workspace. Where that is open (see devboxMatches), any candidate with a
	// workspace refuses. A directory outside every checkout has none; only an
	// unknown hook cwd falls back to the config's checkout.
	for _, hit := range devboxMatches(cmd, in.CWD, compileDevboxPatterns(cfg.DevboxWhenWorkspace)) {
		for _, run := range hit.runs {
			root := checkoutRoot(run)
			if run == "" {
				root = cfg.root
			}
			ws := devboxWorkspaceFor(root)
			if ws == "" {
				continue
			}
			return deny("machine:devbox-workspace", fmt.Sprintf(`%s

runs on this Mac, and its checkout is synced to the Devbox workspace %s; this
repo's guards.devboxWhenWorkspace routes it there:
    %s
(drop --no-up when the command needs the app services). A checkout without a
workspace may run the same command here; the registry is ~/.devbox/workspaces.`, hit.seg, ws, devboxRerun(hit, run, in.CWD, " --no-up")), devboxOnlyEscape)
		}
	}
	return nil
}
