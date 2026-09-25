package claudeguards

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

// devboxWorkspacesDir is the devbox CLI's local workspace registry.
func devboxWorkspacesDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".devbox", "workspaces")
}

// devboxWorkspaceFor returns the name of the Devbox workspace whose synced
// checkout is root (or a parent of it), or "" when no local record says so.
func devboxWorkspaceFor(root string) string {
	dir := devboxWorkspacesDir()
	if dir == "" || root == "" {
		return ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	roots := []string{filepath.Clean(root)}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != roots[0] {
		roots = append(roots, resolved)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name(), "workspace.yaml"))
		if err != nil {
			continue
		}
		var ws struct {
			Name string `yaml:"name"`
			Apps map[string]struct {
				Sync string `yaml:"sync"`
			} `yaml:"apps"`
		}
		if yaml.Unmarshal(raw, &ws) != nil {
			continue
		}
		for _, app := range ws.Apps {
			sync := filepath.Clean(strings.TrimSpace(app.Sync))
			if sync == "." || sync == "" {
				continue
			}
			for _, r := range roots {
				if r == sync || strings.HasPrefix(r, sync+string(filepath.Separator)) {
					if ws.Name != "" {
						return ws.Name
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
	seg := devboxOnlyMatch(cmd, compileDevboxPatterns(cfg.DevboxWhenWorkspace))
	if seg == "" {
		return nil
	}
	ws := devboxWorkspaceFor(cfg.root)
	if ws == "" {
		return nil
	}
	return deny("machine:devbox-workspace", fmt.Sprintf(`%s

runs on this Mac, and this checkout is synced to the Devbox workspace %s; this
repo's guards.devboxWhenWorkspace routes it there:
    devbox run --no-up -- '%s'
(drop --no-up when the command needs the app services). A checkout without a
workspace may run the same command here; the registry is ~/.devbox/workspaces.`, seg, ws, seg), devboxOnlyEscape)
}
