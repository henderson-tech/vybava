package uiloop

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// DevboxSyncIgnores are the sync_ignores a Devbox app that syncs the repo
// needs so the box keeps what the capture writes under <out>. Devbox sync is
// one-way (Mac → box): without them the next sync deletes shots/, .auth/,
// auth.json and params.json mid-run, and the following test fails with "no
// signed-in session". Only run.json (written on the Mac) is left to sync.
func DevboxSyncIgnores(out string) []string {
	out = "/" + strings.Trim(path.Clean(out), "/")
	var ignores []string
	for _, name := range []string{"shots", ".auth", "auth.json", "params.json", "report.json", "report.md", "done.json", "review", "fix", "*.tmp-*", "playwright"} {
		ignores = append(ignores, out+"/*/"+name)
	}
	return ignores
}

// devboxRecipe is the part of a devbox.yaml (or its devbox.worktree.yaml
// patch) the sync check reads; everything else is ignored.
type devboxRecipe struct {
	Apps map[string]struct {
		Sync        string   `yaml:"sync"`
		SyncIgnores []string `yaml:"sync_ignores"`
	} `yaml:"apps"`
}

// SyncGap is a Devbox app that syncs the repo without the capture's ignores.
type SyncGap struct {
	Recipe  string   `json:"recipe"`
	App     string   `json:"app"`
	Missing []string `json:"missing"`
}

// devboxSyncGaps finds the Devbox recipes next to the repo — its own
// devbox.yaml and its siblings' (a `sync: sibling:<repo>` workspace) — whose
// apps sync the repo root without DevboxSyncIgnores. A devbox.worktree.yaml
// beside a recipe is folded in (its sync wins, its ignores add). Best effort:
// an unreadable or unparsable recipe is skipped, never an error.
func (t *Tool) devboxSyncGaps() []SyncGap {
	root := canonical(t.Root)
	dirs := []string{t.Root}
	if siblings, err := os.ReadDir(filepath.Dir(t.Root)); err == nil {
		for _, s := range siblings {
			if dir := filepath.Join(filepath.Dir(t.Root), s.Name()); s.IsDir() && dir != t.Root {
				dirs = append(dirs, dir)
			}
		}
	}
	want := DevboxSyncIgnores(t.Config.Out)
	gaps := []SyncGap{}
	for _, dir := range dirs {
		recipe, ok := readRecipe(filepath.Join(dir, "devbox.yaml"))
		if !ok {
			continue
		}
		patch, _ := readRecipe(filepath.Join(dir, "devbox.worktree.yaml"))
		for _, name := range sortedAppNames(recipe, patch) {
			app, over := recipe.Apps[name], patch.Apps[name]
			sync := app.Sync
			if over.Sync != "" {
				sync = over.Sync
			}
			if sync == "" || canonical(syncTarget(dir, sync)) != root {
				continue
			}
			ignores := append(slices.Clone(app.SyncIgnores), over.SyncIgnores...)
			var missing []string
			for _, w := range want {
				if !slices.Contains(ignores, w) && !slices.Contains(ignores, strings.TrimPrefix(w, "/")) {
					missing = append(missing, w)
				}
			}
			if len(missing) > 0 {
				rel, err := filepath.Rel(t.Root, filepath.Join(dir, "devbox.yaml"))
				if err != nil {
					rel = filepath.Join(dir, "devbox.yaml")
				}
				gaps = append(gaps, SyncGap{Recipe: filepath.ToSlash(rel), App: name, Missing: missing})
			}
		}
	}
	return gaps
}

func readRecipe(file string) (devboxRecipe, bool) {
	var r devboxRecipe
	b, err := os.ReadFile(file)
	if err != nil || yaml.Unmarshal(b, &r) != nil {
		return devboxRecipe{}, false
	}
	return r, true
}

func sortedAppNames(recipes ...devboxRecipe) []string {
	var names []string
	for _, r := range recipes {
		for n := range r.Apps {
			if !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
	}
	slices.Sort(names)
	return names
}

// syncTarget resolves an app's `sync` against the recipe's directory:
// `sibling:<name>` is a directory beside it, `~/…` is home, anything else a path.
func syncTarget(dir, sync string) string {
	switch {
	case strings.HasPrefix(sync, "sibling:"):
		return filepath.Join(filepath.Dir(dir), strings.TrimPrefix(sync, "sibling:"))
	case strings.HasPrefix(sync, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(home, sync[2:])
	case filepath.IsAbs(sync):
		return sync
	default:
		return filepath.Join(dir, sync)
	}
}

func canonical(p string) string {
	if p == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

func syncGapDiag(g SyncGap) runxDiagnostic {
	return warn(DiagDevboxSync, fmt.Sprintf("%s app %q syncs this repo one-way without ignoring the capture's output: a sync mid-run deletes what the box wrote (missing sync_ignores: %s)",
		g.Recipe, g.App, strings.Join(g.Missing, ", ")),
		"add the missing patterns to that app's sync_ignores (docs/uiloop.md, On a Devbox)")
}
