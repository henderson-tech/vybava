// Package plugingc keeps the Claude Code plugin cache honest.
//
// Claude Code keeps every plugin version it has ever installed under
// ~/.claude/plugins/cache/<marketplace>/<plugin>/<version>/ and refcounts it
// with PID marker files in <version>/.in_use/. A version becomes reclaimable
// only when no marker remains, so an abandoned `claude` process pins its
// version forever. Measured on the machine this was written for: one plugin
// held 18 versions at ~480 MB each — 8.4 GB — of which 99% was a node_modules
// tree that nothing reads at skill-load time. The plugin surface the loader
// actually reads (skills/, agents/, .claude-plugin/) was ~520 KB per version.
//
// Three moves, all opt-in, ordered by how little each can possibly break:
//
//   - MoveStrip  — delete node_modules under an INACTIVE version. Reclaims
//     almost every byte without touching a marker or a running session.
//   - MoveSweep  — delete .in_use markers whose PID is dead: gone, or held by
//     a process that is plainly not a Claude Code session, or by one that
//     started AFTER the marker was written (a recycled PID).
//   - MoveRemove — delete an inactive version directory once no live marker
//     remains on it.
//
// The active version is resolved from installed_plugins.json, never by
// sorting version strings — lexical order puts 3.11.0 above 5.3.0, which is
// the bug that made this package necessary — and is never touched, nor is
// ~/.claude/plugins/marketplaces/. Anything the scan cannot decide is
// reported and kept: under-claiming costs disk, over-claiming breaks a live
// session.
package plugingc

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Move is one of the three things a run may do. A run with no moves enabled
// is a report.
type Move string

const (
	// MoveStrip deletes node_modules trees under inactive versions.
	MoveStrip Move = "strip"
	// MoveSweep deletes .in_use markers whose PID is dead.
	MoveSweep Move = "sweep"
	// MoveRemove deletes inactive version directories that no live marker
	// holds.
	MoveRemove Move = "remove"
)

// Moves is every move a run can perform, in the order a run performs them:
// sweeping first tells remove the truth, and stripping is what is left for a
// version remove will not take.
var Moves = []Move{MoveSweep, MoveStrip, MoveRemove}

// Plan is what the scan decided for one version directory.
type Plan string

const (
	// PlanActive is the version installed_plugins.json points at. Never
	// touched, whatever else is true of it.
	PlanActive Plan = "active"
	// PlanHeld is an inactive version a live session still holds; its
	// node_modules can go, its directory cannot.
	PlanHeld Plan = "held"
	// PlanStale is an inactive version with no live marker: the whole
	// directory is reclaimable.
	PlanStale Plan = "stale"
	// PlanKeep is a version the scan refused to judge. Reported, never
	// touched.
	PlanKeep Plan = "keep"
)

// MarkerDir is the refcount directory Claude Code writes inside a version.
const MarkerDir = ".in_use"

// modulesDir is the only tree a strip removes.
const modulesDir = "node_modules"

// Version is one cached version directory.
type Version struct {
	Name    string   `json:"version"`
	Path    string   `json:"path"`
	Plan    Plan     `json:"plan"`
	Reason  string   `json:"reason,omitempty"`
	Bytes   int64    `json:"bytes"`
	Markers []Marker `json:"markers,omitempty"`
	// Live and Dead count Markers by liveness, so a projection can drop the
	// marker list and stay readable.
	Live int `json:"live_markers"`
	Dead int `json:"dead_markers"`
	// Modules are the node_modules trees found under this version, largest
	// first, with the bytes each holds.
	Modules []Tree `json:"node_modules,omitempty"`
	// ModuleBytes is what a strip would reclaim here; DirBytes is what a
	// remove would. They never both count: a removed directory takes its
	// node_modules with it.
	ModuleBytes int64 `json:"module_bytes"`
	DirBytes    int64 `json:"dir_bytes"`
}

// Tree is one directory and what it holds.
type Tree struct {
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// Plugin is one plugin inside one marketplace.
type Plugin struct {
	Marketplace string `json:"marketplace"`
	Name        string `json:"plugin"`
	// Key is the "<plugin>@<marketplace>" form installed_plugins.json uses.
	Key string `json:"key"`
	// Active lists every version the install record points at — a plugin
	// installed at both user and project scope has more than one, and all of
	// them are untouchable.
	Active   []string  `json:"active_versions,omitempty"`
	Versions []Version `json:"versions"`
}

// Outcome records what a move actually did.
type Outcome struct {
	Move  Move   `json:"move"`
	Path  string `json:"path"`
	Bytes int64  `json:"bytes,omitempty"`
	Error string `json:"error,omitempty"`
}

// Report is one run, dry or not.
type Report struct {
	Home   string `json:"home"`
	DryRun bool   `json:"dry_run"`
	// Sessions is how many live Claude Code processes the scan saw, the
	// number that explains why so much is held.
	Sessions int      `json:"live_sessions"`
	Plugins  []Plugin `json:"plugins"`
	// StripBytes and RemoveBytes are what the enabled moves would reclaim;
	// SweepMarkers is how many dead markers they would clear.
	StripBytes   int64 `json:"strip_bytes"`
	RemoveBytes  int64 `json:"remove_bytes"`
	SweepMarkers int   `json:"sweep_markers"`
	// Reclaimed is what actually left the disk (zero on a dry run).
	Reclaimed int64     `json:"reclaimed_bytes"`
	Outcomes  []Outcome `json:"outcomes,omitempty"`
	Warnings  []string  `json:"warnings,omitempty"`
}

// Reclaimable is everything the enabled moves would free.
func (r Report) Reclaimable() int64 { return r.StripBytes + r.RemoveBytes }

// Options steer one run.
type Options struct {
	// Apply turns the report into the deed. Zero value is a dry run.
	Apply bool
	// Only and Skip filter Moves by id, exactly as reclaim's step filters do.
	Only, Skip []Move
	// Plugins narrows the run to these plugins, by name or by
	// "<plugin>@<marketplace>"; empty means every plugin.
	Plugins []string
	// Grace is how far a process may have started after its marker was
	// written before the PID is judged recycled (default 2 minutes).
	Grace time.Duration
}

// Env is the machine the run reads; tests substitute it.
type Env struct {
	// Home is the plugin home, normally ~/.claude/plugins.
	Home string
	// Processes lists the live process table. A run that cannot read it
	// refuses to sweep rather than guess.
	Processes ProcessLister
}

// ErrNoInstallRecord is returned when installed_plugins.json cannot be read.
// Without it the active version is unknowable and every move is a guess, so
// the run refuses outright.
var ErrNoInstallRecord = errors.New("plugin-gc: cannot read installed_plugins.json — refusing to guess the active version")

// Run scans the cache and, when opts.Apply is set, performs the enabled
// moves. A dry run and an applied run take the same decisions; only the
// deletions differ.
func Run(ctx context.Context, env Env, opts Options) (Report, error) {
	if opts.Grace <= 0 {
		opts.Grace = defaultGrace
	}
	report := Report{Home: env.Home, DryRun: !opts.Apply}

	record, err := loadInstalled(env.Home)
	if err != nil {
		return report, fmt.Errorf("%w: %v", ErrNoInstallRecord, err)
	}

	table, procErr := env.processTable(ctx)
	view := processView{table: table, known: procErr == nil}
	if procErr != nil {
		// Without a process table no marker can be proven dead — and reading
		// that as "every PID is gone" would condemn the whole machine. Every
		// marker is held, and the moves that depend on the verdict are off.
		report.Warnings = append(report.Warnings, fmt.Sprintf("process table unreadable (%v) — every marker is treated as live, nothing is swept or removed", procErr))
		opts.Skip = append(opts.Skip, MoveSweep, MoveRemove)
	}
	report.Sessions = table.sessions()

	plugins, err := scan(ctx, env.Home, record, view, opts)
	if err != nil {
		return report, err
	}
	report.Plugins = plugins

	enabled := enabledMoves(opts)
	for i := range report.Plugins {
		for j := range report.Plugins[i].Versions {
			tally(&report, report.Plugins[i].Versions[j], enabled)
		}
	}
	if !opts.Apply {
		return report, nil
	}
	apply(ctx, &report, enabled)
	return report, nil
}

// tally adds one version's yield to the report's totals, counting only the
// moves this run has enabled.
func tally(report *Report, version Version, enabled map[Move]bool) {
	if enabled[MoveSweep] {
		report.SweepMarkers += version.Dead
	}
	switch version.Plan {
	case PlanStale:
		if enabled[MoveRemove] {
			report.RemoveBytes += version.DirBytes
			return
		}
		// Not removing it: its node_modules is still worth stripping.
		if enabled[MoveStrip] {
			report.StripBytes += version.ModuleBytes
		}
	case PlanHeld:
		if enabled[MoveStrip] {
			report.StripBytes += version.ModuleBytes
		}
	}
}

// apply performs the enabled moves version by version, sweeping before it
// removes so a version freed by the sweep is reclaimed in the same run.
//
// Sweep is per-MARKER, strip and remove are per-VERSION. A proven-dead marker
// is dead whatever plan its version carries, including the active one — that
// is the refcount going honest, and it is what lets today's active version be
// reclaimed the day it rolls over. Only the version's CONTENT is untouchable.
func apply(ctx context.Context, report *Report, enabled map[Move]bool) {
	for i := range report.Plugins {
		for j := range report.Plugins[i].Versions {
			version := &report.Plugins[i].Versions[j]
			if enabled[MoveSweep] {
				sweepMarkers(report, version)
			}
			if version.Plan == PlanActive || version.Plan == PlanKeep {
				continue
			}
			if version.Plan == PlanStale && enabled[MoveRemove] {
				removeTree(ctx, report, MoveRemove, version.Path)
				continue
			}
			if enabled[MoveStrip] {
				for _, tree := range version.Modules {
					removeTree(ctx, report, MoveStrip, tree.Path)
				}
			}
		}
	}
}

func sweepMarkers(report *Report, version *Version) {
	for _, marker := range version.Markers {
		if !marker.Liveness.Dead() {
			continue
		}
		outcome := Outcome{Move: MoveSweep, Path: marker.Path}
		if err := os.Remove(marker.Path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			outcome.Error = err.Error()
		}
		report.Outcomes = append(report.Outcomes, outcome)
	}
}

func removeTree(ctx context.Context, report *Report, move Move, path string) {
	size, err := dirSize(ctx, path)
	outcome := Outcome{Move: move, Path: path, Bytes: size}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		outcome.Error = err.Error()
		report.Outcomes = append(report.Outcomes, outcome)
		return
	}
	if err := os.RemoveAll(path); err != nil {
		outcome.Error = err.Error()
		outcome.Bytes = 0
		report.Outcomes = append(report.Outcomes, outcome)
		return
	}
	report.Reclaimed += size
	report.Outcomes = append(report.Outcomes, outcome)
}

// scan reads the cache tree into plugins, deciding a plan per version.
func scan(ctx context.Context, home string, record installed, view processView, opts Options) ([]Plugin, error) {
	cache := filepath.Join(home, "cache")
	marketplaces, err := os.ReadDir(cache)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	wanted := selector(opts.Plugins)
	var plugins []Plugin
	for _, marketplace := range marketplaces {
		if !marketplace.IsDir() {
			continue
		}
		names, err := os.ReadDir(filepath.Join(cache, marketplace.Name()))
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			if !name.IsDir() {
				continue
			}
			plugin := Plugin{
				Marketplace: marketplace.Name(),
				Name:        name.Name(),
				Key:         name.Name() + "@" + marketplace.Name(),
			}
			if !wanted(plugin) {
				continue
			}
			plugin.Active = record.activeVersions(plugin.Key)
			versions, err := scanVersions(ctx, filepath.Join(cache, marketplace.Name(), name.Name()), record, plugin, view, opts.Grace)
			if err != nil {
				return nil, err
			}
			plugin.Versions = versions
			plugins = append(plugins, plugin)
		}
	}
	sort.Slice(plugins, func(i, j int) bool { return plugins[i].Key < plugins[j].Key })
	return plugins, nil
}

func scanVersions(ctx context.Context, dir string, record installed, plugin Plugin, view processView, grace time.Duration) ([]Version, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var versions []Version
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		version := Version{Name: entry.Name(), Path: path}
		if record.isActive(plugin.Key, path) {
			version.Plan = PlanActive
			version.Reason = "installed_plugins.json points here"
			version.Markers, version.Live, version.Dead = readMarkers(path, view, grace)
			// An active version is never counted as yield, so its size is
			// informational only.
			version.Bytes, _ = dirSize(ctx, path)
			versions = append(versions, version)
			continue
		}
		version.Markers, version.Live, version.Dead = readMarkers(path, view, grace)
		version.Bytes, version.Modules, err = measure(ctx, path)
		if err != nil {
			return nil, err
		}
		for _, tree := range version.Modules {
			version.ModuleBytes += tree.Bytes
		}
		decide(&version)
		versions = append(versions, version)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].Name < versions[j].Name })
	return versions, nil
}

// decide sets the plan for an inactive version. Held-but-strippable is the
// high-yield case; the refusals are deliberate and each says why.
func decide(version *Version) {
	if version.Live > 0 {
		version.Plan = PlanHeld
		version.Reason = fmt.Sprintf("%d live session(s) hold it", version.Live)
		if reason, ok := stripRefusal(version.Path); ok {
			version.Plan = PlanKeep
			version.Reason = reason
			version.Modules, version.ModuleBytes = nil, 0
		}
		return
	}
	version.Plan = PlanStale
	version.DirBytes = version.Bytes
	switch {
	case version.Dead > 0:
		version.Reason = fmt.Sprintf("%d dead marker(s), no live session", version.Dead)
	default:
		version.Reason = "no marker, no live session"
	}
}

// stripRefusal reports why node_modules must stay even though the version is
// inactive. The one way a live session could reach into node_modules is an
// entry point its manifest names, so the manifests are read and any mention
// of the tree is taken at face value.
func stripRefusal(versionPath string) (string, bool) {
	for _, name := range []string{
		filepath.Join(".claude-plugin", "plugin.json"),
		".mcp.json",
	} {
		body, err := os.ReadFile(filepath.Join(versionPath, name))
		if err != nil {
			continue
		}
		if strings.Contains(string(body), modulesDir) {
			return "held, and " + name + " names node_modules — refusing to strip a tree a live session may execute", true
		}
	}
	return "", false
}

// measure sizes a version and finds its node_modules trees in ONE walk — the
// cache holds hundreds of thousands of files and a second pass is minutes.
// A found node_modules is sized whole and not descended into: a bun workspace
// nests node_modules/.bun/node_modules, and counting both doubles the yield.
func measure(ctx context.Context, root string) (int64, []Tree, error) {
	var total int64
	var trees []Tree
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.IsDir() {
			if entry.Name() != modulesDir {
				return nil
			}
			size, err := dirSize(ctx, path)
			if err != nil {
				return err
			}
			trees = append(trees, Tree{Path: path, Bytes: size})
			total += size
			return fs.SkipDir
		}
		if info, err := entry.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	sort.Slice(trees, func(i, j int) bool { return trees[i].Bytes > trees[j].Bytes })
	return total, trees, nil
}

// dirSize is the logical size of a tree, symlinks counted as links and never
// followed — a node_modules is full of them.
func dirSize(ctx context.Context, root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// selector turns the --plugin filters into a predicate. A bare name matches
// the plugin in any marketplace; "<plugin>@<marketplace>" pins both.
func selector(filters []string) func(Plugin) bool {
	if len(filters) == 0 {
		return func(Plugin) bool { return true }
	}
	wanted := make(map[string]bool, len(filters))
	for _, filter := range filters {
		wanted[strings.TrimSpace(filter)] = true
	}
	return func(p Plugin) bool { return wanted[p.Name] || wanted[p.Key] }
}

func enabledMoves(opts Options) map[Move]bool {
	only := make(map[Move]bool, len(opts.Only))
	for _, move := range opts.Only {
		only[move] = true
	}
	skip := make(map[Move]bool, len(opts.Skip))
	for _, move := range opts.Skip {
		skip[move] = true
	}
	enabled := make(map[Move]bool, len(Moves))
	for _, move := range Moves {
		if skip[move] {
			continue
		}
		if len(only) > 0 && !only[move] {
			continue
		}
		enabled[move] = true
	}
	return enabled
}

// ParseMoves validates move ids coming off the command line.
func ParseMoves(values []string) ([]Move, error) {
	var moves []Move
	for _, value := range values {
		move := Move(strings.TrimSpace(value))
		known := false
		for _, candidate := range Moves {
			if candidate == move {
				known = true
				break
			}
		}
		if !known {
			return nil, fmt.Errorf("unknown move %q (known: %s)", value, strings.Join(moveIDs(), ", "))
		}
		moves = append(moves, move)
	}
	return moves, nil
}

func moveIDs() []string {
	ids := make([]string, 0, len(Moves))
	for _, move := range Moves {
		ids = append(ids, string(move))
	}
	return ids
}
