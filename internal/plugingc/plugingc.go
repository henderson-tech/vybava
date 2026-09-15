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
	"strconv"
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
	// PlanOrphaned is the strongest form of stale: the version carries an
	// .orphaned_at stamp AND its plugin is no longer installed at all.
	// `claude plugin uninstall` writes that stamp and leaves the tree — the
	// cache survives both uninstall and marketplace removal.
	PlanOrphaned Plan = "orphaned"
	// PlanKeep is a version the scan refused to judge. Reported, never
	// touched.
	PlanKeep Plan = "keep"
)

// MarkerDir is the refcount directory Claude Code writes inside a version.
const MarkerDir = ".in_use"

// modulesDir is the only tree a strip removes.
const modulesDir = "node_modules"

// orphanFile is the stamp `claude plugin uninstall` leaves behind instead of
// deleting the version: milliseconds since the epoch, nothing else.
const orphanFile = ".orphaned_at"

// DefaultOrphanGrace is how long an orphaned version is kept before it counts
// as reclaimable. An uninstall is often a mistake discovered the same day; a
// week of regret is cheap at these sizes.
const DefaultOrphanGrace = 7 * 24 * time.Hour

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
	// OrphanedAt is the .orphaned_at stamp, when the version carries one.
	// Reported whatever the plan, because it explains the version's presence.
	OrphanedAt *time.Time `json:"orphaned_at,omitempty"`
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
	// Path is the plugin's directory, the parent of every version.
	Path string `json:"path"`
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
	// StripBytes, RemoveBytes and OrphanBytes are what the enabled moves
	// would reclaim, kept apart so the dry run can name each class;
	// SweepMarkers is how many dead markers they would clear.
	StripBytes   int64 `json:"strip_bytes"`
	RemoveBytes  int64 `json:"remove_bytes"`
	OrphanBytes  int64 `json:"orphan_bytes"`
	SweepMarkers int   `json:"sweep_markers"`
	// Reclaimed is what actually left the disk (zero on a dry run).
	Reclaimed int64     `json:"reclaimed_bytes"`
	Outcomes  []Outcome `json:"outcomes,omitempty"`
	Warnings  []string  `json:"warnings,omitempty"`
}

// Reclaimable is everything the enabled moves would free.
func (r Report) Reclaimable() int64 { return r.StripBytes + r.RemoveBytes + r.OrphanBytes }

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
	// OrphanGrace is how long an uninstalled version is kept before `remove`
	// will take it (default DefaultOrphanGrace). A younger orphan is reported
	// and left alone.
	OrphanGrace time.Duration
	// now is the resolved clock, filled by Run from Env.Now.
	now time.Time
}

// Env is the machine the run reads; tests substitute it.
type Env struct {
	// Home is the plugin home, normally ~/.claude/plugins.
	Home string
	// Processes lists the live process table. A run that cannot read it
	// refuses to sweep rather than guess.
	Processes ProcessLister
	// Now is the clock the orphan grace is measured against; zero means
	// time.Now.
	Now time.Time
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
	if opts.OrphanGrace <= 0 {
		opts.OrphanGrace = DefaultOrphanGrace
	}
	opts.now = env.Now
	if opts.now.IsZero() {
		opts.now = time.Now()
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

	// An install record that names nothing while the cache holds versions is
	// not a machine with no plugins — it is a record this build cannot read
	// (a renamed field, a schema bump, a truncated file). Reading it at face
	// value would mark the version in use "stale" and delete it. Report, do
	// not destroy.
	if len(record.Plugins) == 0 && countVersions(plugins) > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf(
			"installed_plugins.json (schema version %d) names no plugins while the cache holds %d version(s) — the active version cannot be trusted, so only the report runs",
			record.Version, countVersions(plugins)))
		opts.Skip = append(opts.Skip, MoveStrip, MoveRemove)
	}

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

func countVersions(plugins []Plugin) int {
	count := 0
	for _, plugin := range plugins {
		count += len(plugin.Versions)
	}
	return count
}

// tally adds one version's yield to the report's totals, counting only the
// moves this run has enabled.
func tally(report *Report, version Version, enabled map[Move]bool) {
	if enabled[MoveSweep] {
		report.SweepMarkers += version.Dead
	}
	switch version.Plan {
	case PlanOrphaned:
		// Counted apart from RemoveBytes so the dry run can say "this is not
		// merely unreferenced — the plugin is gone".
		if enabled[MoveRemove] {
			report.OrphanBytes += version.DirBytes
			return
		}
		if enabled[MoveStrip] {
			report.StripBytes += version.ModuleBytes
		}
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
			if (version.Plan == PlanStale || version.Plan == PlanOrphaned) && enabled[MoveRemove] {
				removeTree(ctx, report, MoveRemove, version.Path)
				continue
			}
			if enabled[MoveStrip] {
				for _, tree := range version.Modules {
					removeTree(ctx, report, MoveStrip, tree.Path)
				}
			}
		}
		// A plugin whose last version just left would otherwise linger as an
		// empty directory and be reported forever. os.Remove refuses a
		// non-empty directory, so this can only ever take an empty one.
		if report.Plugins[i].Path != "" {
			_ = os.Remove(report.Plugins[i].Path)
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
				Path:        filepath.Join(cache, marketplace.Name(), name.Name()),
			}
			if !wanted(plugin) {
				continue
			}
			plugin.Active = record.activeVersions(plugin.Key)
			// "Installed at all" is what separates an orphan from a merely
			// superseded version, and it is a property of the PLUGIN.
			v := verdict{installed: len(record.Plugins[plugin.Key]) > 0, now: opts.now, orphanGrace: opts.OrphanGrace}
			versions, err := scanVersions(ctx, filepath.Join(cache, marketplace.Name(), name.Name()), record, plugin, view, opts.Grace, v)
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

func scanVersions(ctx context.Context, dir string, record installed, plugin Plugin, view processView, grace time.Duration, v verdict) ([]Version, error) {
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
			version.OrphanedAt = readOrphanedAt(path)
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
		decide(&version, v)
		versions = append(versions, version)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].Name < versions[j].Name })
	return versions, nil
}

// verdict is everything decide needs beyond the version itself.
type verdict struct {
	// installed says whether the OWNING PLUGIN is installed at any version.
	// An .orphaned_at stamp on a version of a still-installed plugin only
	// means that version was superseded; on a plugin nobody has installed it
	// means the whole thing is gone.
	installed   bool
	now         time.Time
	orphanGrace time.Duration
}

// decide sets the plan for an inactive version. Held-but-strippable is the
// high-yield case; the refusals are deliberate and each says why.
func decide(version *Version, v verdict) {
	version.OrphanedAt = readOrphanedAt(version.Path)
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
	version.DirBytes = version.Bytes

	// An orphan is a stale version with a receipt: `claude plugin uninstall`
	// stamped it and walked away. Only when the plugin itself is gone, and
	// only once the regret window has passed.
	if version.OrphanedAt != nil && !v.installed {
		age := v.now.Sub(*version.OrphanedAt)
		if age >= v.orphanGrace {
			version.Plan = PlanOrphaned
			version.Reason = fmt.Sprintf("plugin uninstalled %s ago, nothing holds it", roundDuration(age))
			return
		}
		version.Plan = PlanKeep
		version.DirBytes = 0
		version.Modules, version.ModuleBytes = nil, 0
		version.Reason = fmt.Sprintf("plugin uninstalled only %s ago — inside the %s orphan grace", roundDuration(age), roundDuration(v.orphanGrace))
		return
	}

	version.Plan = PlanStale
	switch {
	case version.Dead > 0:
		version.Reason = fmt.Sprintf("%d dead marker(s), no live session", version.Dead)
	default:
		version.Reason = "no marker, no live session"
	}
}

// readOrphanedAt reads the .orphaned_at stamp, milliseconds since the epoch.
// A missing, empty or unparseable file simply means "not orphaned" — the
// stamp only ever adds confidence, never removes it.
func readOrphanedAt(versionPath string) *time.Time {
	body, err := os.ReadFile(filepath.Join(versionPath, orphanFile))
	if err != nil {
		return nil
	}
	millis, err := strconv.ParseInt(strings.TrimSpace(string(body)), 10, 64)
	if err != nil || millis <= 0 {
		return nil
	}
	stamped := time.UnixMilli(millis)
	return &stamped
}

// roundDuration prints an age the way a human says it.
func roundDuration(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
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
