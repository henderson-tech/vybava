package plugingc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// cache builds a plugin home: one marketplace, one plugin, the named versions,
// each with a plugin manifest, a skill and a node_modules tree.
func cache(t *testing.T, active string, versions ...string) string {
	t.Helper()
	home := t.TempDir()
	for _, version := range versions {
		dir := filepath.Join(home, "cache", "kit", "vitrinka", version)
		write(t, filepath.Join(dir, ".claude-plugin", "plugin.json"), `{"name":"vitrinka"}`)
		write(t, filepath.Join(dir, "skills", "publish", "SKILL.md"), "surface")
		write(t, filepath.Join(dir, "node_modules", "left-pad", "index.js"), "module bytes——")
		write(t, filepath.Join(dir, "node_modules", ".bun", "node_modules", "dep.js"), "nested")
	}
	record := `{"version":2,"plugins":{"vitrinka@kit":[{"scope":"user","version":"` + active + `",` +
		`"installPath":"` + filepath.Join(home, "cache", "kit", "vitrinka", active) + `"}]}}`
	write(t, filepath.Join(home, "installed_plugins.json"), record)
	return home
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// marker writes an .in_use marker for pid, aged by age.
func marker(t *testing.T, home, version string, pid int, age time.Duration) {
	t.Helper()
	path := filepath.Join(home, "cache", "kit", "vitrinka", version, MarkerDir, strconv.Itoa(pid))
	write(t, path, `{"pid":`+strconv.Itoa(pid)+`,"procStart":"Tue Sep 15 16:07:45 2026"}`)
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

func table(processes ...Process) ProcessLister {
	return func(context.Context) (ProcessTable, error) {
		t := ProcessTable{}
		for _, process := range processes {
			t[process.PID] = process
		}
		return t, nil
	}
}

func find(t *testing.T, report Report, version string) Version {
	t.Helper()
	for _, plugin := range report.Plugins {
		for _, candidate := range plugin.Versions {
			if candidate.Name == version {
				return candidate
			}
		}
	}
	t.Fatalf("version %q not in report", version)
	return Version{}
}

// The bug this package exists for: sorting version strings puts 3.11.0 above
// 5.3.0, so the active version must come from the install record's path.
func TestActiveVersionComesFromTheInstallRecordNotVersionOrder(t *testing.T) {
	home := cache(t, "5.3.0", "3.11.0", "5.3.0")
	report, err := Run(context.Background(), Env{Home: home, Processes: table()}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if plan := find(t, report, "5.3.0").Plan; plan != PlanActive {
		t.Fatalf("5.3.0 is installed, got plan %q", plan)
	}
	if plan := find(t, report, "3.11.0").Plan; plan != PlanStale {
		t.Fatalf("3.11.0 is not installed, got plan %q", plan)
	}
	if report.RemoveBytes == 0 || report.RemoveBytes != find(t, report, "3.11.0").Bytes {
		t.Fatalf("stale version should be the whole reclaim, got %d", report.RemoveBytes)
	}
}

func TestMissingInstallRecordRefusesRatherThanGuess(t *testing.T) {
	home := cache(t, "5.3.0", "5.3.0")
	if err := os.Remove(filepath.Join(home, "installed_plugins.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), Env{Home: home, Processes: table()}, Options{Apply: true}); !errors.Is(err, ErrNoInstallRecord) {
		t.Fatalf("want ErrNoInstallRecord, got %v", err)
	}
}

// A live PID proves nothing on its own: it may be recycled, and macOS recycles
// freely. Only gone / not-a-session / started-after-the-marker are dead.
func TestMarkerLivenessNeedsProcessIdentityNotJustALivePID(t *testing.T) {
	now := time.Now()
	written := now.Add(-time.Hour)
	for _, tc := range []struct {
		name    string
		process Process
		running bool
		want    Liveness
	}{
		{name: "no process at all", want: LivenessGone},
		{name: "a live claude session", running: true, want: LivenessHeld,
			process: Process{PID: 42, Command: "/Users/x/.bun/bin/claude", Started: written.Add(-time.Minute)}},
		{name: "pid recycled by an unrelated program", running: true, want: LivenessForeign,
			process: Process{PID: 42, Command: "/Applications/Safari.app/Contents/MacOS/Safari", Started: written.Add(-time.Minute)}},
		{name: "pid recycled by another claude", running: true, want: LivenessRecycled,
			process: Process{PID: 42, Command: "/Users/x/.bun/bin/claude", Started: now}},
		{name: "a node host is never called foreign", running: true, want: LivenessHeld,
			process: Process{PID: 42, Command: "/usr/local/bin/node", Started: written.Add(-time.Minute)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := ProcessTable{}
			if tc.running {
				live[42] = tc.process
			}
			view := processView{table: live, known: true}
			got, _ := view.classify(Marker{PID: 42, Written: written}, defaultGrace)
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}

	// An UNREADABLE table is not an empty one: knowing nothing must hold every
	// marker, not condemn every marker.
	unknown := processView{table: nil, known: false}
	if got, _ := unknown.classify(Marker{PID: 42, Written: written}, defaultGrace); got != LivenessHeld {
		t.Fatalf("unknown process table gave %q, want held", got)
	}
}

func TestParsePSKeepsTheWholeCommandAndTheStartTime(t *testing.T) {
	out := "77773 Tue Sep 15 17:22:47 2026     /Users/lukaspribik/.bun/bin/claude\n" +
		"  135 Mon Sep  8 18:08:11 2026     /usr/libexec/tipsd\n"
	live := ParsePS(out, time.UTC)
	claude, ok := live[77773]
	if !ok {
		t.Fatal("claude process missing")
	}
	if claude.Command != "/Users/lukaspribik/.bun/bin/claude" {
		t.Fatalf("command %q", claude.Command)
	}
	if want := time.Date(2026, 9, 15, 17, 22, 47, 0, time.UTC); !claude.Started.Equal(want) {
		t.Fatalf("started %v, want %v", claude.Started, want)
	}
	if live.sessions() != 1 {
		t.Fatalf("sessions %d, want 1", live.sessions())
	}
}

// The high-yield, low-risk move: an inactive version a live session holds
// keeps its surface and loses node_modules, and no marker is touched.
func TestStripLeavesTheSurfaceAndTheMarkersOfAHeldVersion(t *testing.T) {
	home := cache(t, "5.3.0", "4.1.0", "5.3.0")
	marker(t, home, "4.1.0", 42, time.Hour)
	env := Env{Home: home, Processes: table(Process{PID: 42, Command: "/bin/claude", Started: time.Now().Add(-2 * time.Hour)})}

	report, err := Run(context.Background(), env, Options{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	held := find(t, report, "4.1.0")
	if held.Plan != PlanHeld {
		t.Fatalf("plan %q, want held", held.Plan)
	}
	dir := filepath.Join(home, "cache", "kit", "vitrinka", "4.1.0")
	if _, err := os.Stat(filepath.Join(dir, "skills", "publish", "SKILL.md")); err != nil {
		t.Fatalf("the plugin surface must survive a strip: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("node_modules should be gone, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, MarkerDir, "42")); err != nil {
		t.Fatalf("a live marker must survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "cache", "kit", "vitrinka", "5.3.0", "node_modules")); err != nil {
		t.Fatalf("the active version must not be touched: %v", err)
	}
	if report.Reclaimed == 0 {
		t.Fatal("a strip that deleted something must report bytes")
	}
}

// Sweeping a dead marker is what makes the version removable in the same run.
func TestApplySweepsDeadMarkersThenRemovesTheFreedVersion(t *testing.T) {
	home := cache(t, "5.3.0", "3.9.0", "5.3.0")
	marker(t, home, "3.9.0", 4242, time.Hour)
	env := Env{Home: home, Processes: table()} // PID 4242 is gone

	report, err := Run(context.Background(), env, Options{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.SweepMarkers != 1 {
		t.Fatalf("swept %d markers, want 1", report.SweepMarkers)
	}
	if _, err := os.Stat(filepath.Join(home, "cache", "kit", "vitrinka", "3.9.0")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreferenced version should be gone, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "cache", "kit", "vitrinka", "5.3.0")); err != nil {
		t.Fatalf("the active version must survive: %v", err)
	}
}

// Reporting is the default; nothing may leave the disk without --apply.
func TestDryRunDeletesNothing(t *testing.T) {
	home := cache(t, "5.3.0", "3.9.0", "5.3.0")
	report, err := Run(context.Background(), Env{Home: home, Processes: table()}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !report.DryRun || report.Reclaimed != 0 || len(report.Outcomes) != 0 {
		t.Fatalf("dry run touched something: %+v", report)
	}
	if report.Reclaimable() == 0 {
		t.Fatal("a dry run must still say what it would reclaim")
	}
	if _, err := os.Stat(filepath.Join(home, "cache", "kit", "vitrinka", "3.9.0")); err != nil {
		t.Fatalf("dry run deleted a version: %v", err)
	}
}

// An unreadable process table would make every marker look dead, so the moves
// that depend on it are disabled instead.
func TestUnreadableProcessTableDisablesSweepAndRemove(t *testing.T) {
	home := cache(t, "5.3.0", "3.9.0", "5.3.0")
	marker(t, home, "3.9.0", 4242, time.Hour)
	env := Env{Home: home, Processes: func(context.Context) (ProcessTable, error) {
		return nil, errors.New("no ps here")
	}}

	report, err := Run(context.Background(), env, Options{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Warnings) == 0 {
		t.Fatal("want a warning about the unreadable process table")
	}
	if report.SweepMarkers != 0 {
		t.Fatalf("swept %d markers with no process table", report.SweepMarkers)
	}
	// The marker must read as HELD, not gone — an unreadable table is not an
	// empty one, and the version must not be reported reclaimable.
	stale := find(t, report, "3.9.0")
	if stale.Plan != PlanHeld || stale.Dead != 0 {
		t.Fatalf("plan %q with %d dead markers, want held/0", stale.Plan, stale.Dead)
	}
	if _, err := os.Stat(filepath.Join(home, "cache", "kit", "vitrinka", "3.9.0", MarkerDir, "4242")); err != nil {
		t.Fatalf("marker must survive an unreadable process table: %v", err)
	}
}

// orphan stamps a version the way `claude plugin uninstall` does: a
// milliseconds-since-epoch file, the rest of the tree left in place.
func orphan(t *testing.T, home, plugin, version string, at time.Time) {
	t.Helper()
	write(t, filepath.Join(home, "cache", "kit", plugin, version, orphanFile),
		strconv.FormatInt(at.UnixMilli(), 10))
}

// `claude plugin uninstall` deletes nothing — it stamps .orphaned_at and walks
// away, so the cache outlives the plugin. Past the grace, that whole tree goes.
func TestUninstalledPluginsCacheIsReclaimedOnceTheOrphanGracePasses(t *testing.T) {
	now := time.Now()
	home := cache(t, "5.3.0", "5.3.0")
	// A second plugin nobody has installed any more.
	write(t, filepath.Join(home, "cache", "kit", "gone", "1.0.0", "skills", "x", "SKILL.md"), "left behind")
	orphan(t, home, "gone", "1.0.0", now.Add(-30*24*time.Hour))

	env := Env{Home: home, Processes: table(), Now: now}
	report, err := Run(context.Background(), env, Options{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	orphaned := find(t, report, "1.0.0")
	if orphaned.Plan != PlanOrphaned {
		t.Fatalf("plan %q, want orphaned", orphaned.Plan)
	}
	if orphaned.OrphanedAt == nil {
		t.Fatal("the .orphaned_at stamp must be reported")
	}
	if report.OrphanBytes == 0 {
		t.Fatal("orphan bytes must be counted apart from ordinary removals")
	}
	if report.RemoveBytes != 0 {
		t.Fatalf("an orphan is not an ordinary removal, got %d remove bytes", report.RemoveBytes)
	}
	if _, err := os.Stat(filepath.Join(home, "cache", "kit", "gone")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the uninstalled plugin's cache should be gone, got %v", err)
	}
}

// An uninstall is often a mistake found the same day, so a fresh orphan is
// reported and left alone.
func TestFreshOrphanIsReportedButKept(t *testing.T) {
	now := time.Now()
	home := cache(t, "5.3.0", "5.3.0")
	write(t, filepath.Join(home, "cache", "kit", "gone", "1.0.0", "skills", "x", "SKILL.md"), "left behind")
	orphan(t, home, "gone", "1.0.0", now.Add(-2*time.Hour))

	env := Env{Home: home, Processes: table(), Now: now}
	report, err := Run(context.Background(), env, Options{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	fresh := find(t, report, "1.0.0")
	if fresh.Plan != PlanKeep {
		t.Fatalf("plan %q, want keep", fresh.Plan)
	}
	if !strings.Contains(fresh.Reason, "orphan grace") {
		t.Fatalf("reason %q must name the grace", fresh.Reason)
	}
	if report.Reclaimable() != 0 || report.Reclaimed != 0 {
		t.Fatalf("a fresh orphan must not be counted, got %d", report.Reclaimable())
	}
	if _, err := os.Stat(filepath.Join(home, "cache", "kit", "gone", "1.0.0")); err != nil {
		t.Fatalf("a fresh orphan must survive: %v", err)
	}
}

// A stamp on a version of a plugin that IS still installed only means that
// version was superseded — it is stale, never an orphan.
func TestOrphanStampOnAStillInstalledPluginIsOnlyStale(t *testing.T) {
	now := time.Now()
	home := cache(t, "5.3.0", "3.9.0", "5.3.0")
	orphan(t, home, "vitrinka", "3.9.0", now.Add(-30*24*time.Hour))

	report, err := Run(context.Background(), Env{Home: home, Processes: table(), Now: now}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	superseded := find(t, report, "3.9.0")
	if superseded.Plan != PlanStale {
		t.Fatalf("plan %q, want stale", superseded.Plan)
	}
	if superseded.OrphanedAt == nil {
		t.Fatal("the stamp is still worth reporting on a superseded version")
	}
	if report.OrphanBytes != 0 {
		t.Fatalf("a superseded version is not an orphan, got %d orphan bytes", report.OrphanBytes)
	}
}

// A clean fixture has no .in_use directory at all; that is "no markers", never
// an error.
func TestMissingMarkerDirectoryIsNoMarkersNotAnError(t *testing.T) {
	home := cache(t, "5.3.0", "4.1.0", "5.3.0")
	report, err := Run(context.Background(), Env{Home: home, Processes: table()}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	bare := find(t, report, "4.1.0")
	if bare.Live != 0 || bare.Dead != 0 || len(bare.Markers) != 0 {
		t.Fatalf("want no markers, got %d live / %d dead", bare.Live, bare.Dead)
	}
	if bare.Plan != PlanStale {
		t.Fatalf("plan %q, want stale", bare.Plan)
	}
}

// A record this build cannot read names no plugins — reading that at face
// value would call the version in use "stale" and delete it.
func TestUnreadableInstallRecordShapeDisablesEveryDestructiveMove(t *testing.T) {
	home := cache(t, "5.3.0", "5.3.0")
	// A plausible future schema: same file, renamed container.
	write(t, filepath.Join(home, "installed_plugins.json"), `{"version":3,"installed":{"vitrinka@kit":[]}}`)

	report, err := Run(context.Background(), Env{Home: home, Processes: table()}, Options{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Warnings) == 0 {
		t.Fatal("want a warning that the install record was not understood")
	}
	if report.Reclaimed != 0 || report.Reclaimable() != 0 {
		t.Fatalf("nothing may be destroyed on an unreadable record, got %d", report.Reclaimed)
	}
	if _, err := os.Stat(filepath.Join(home, "cache", "kit", "vitrinka", "5.3.0", "node_modules")); err != nil {
		t.Fatalf("the version in use must survive: %v", err)
	}
}

// A held version whose manifest could execute out of node_modules is reported,
// never stripped.
func TestHeldVersionWhoseManifestNamesNodeModulesIsKept(t *testing.T) {
	home := cache(t, "5.3.0", "4.1.0", "5.3.0")
	write(t, filepath.Join(home, "cache", "kit", "vitrinka", "4.1.0", ".claude-plugin", "plugin.json"),
		`{"name":"vitrinka","mcpServers":{"x":{"command":"node","args":["node_modules/.bin/server"]}}}`)
	marker(t, home, "4.1.0", 42, time.Hour)
	env := Env{Home: home, Processes: table(Process{PID: 42, Command: "/bin/claude", Started: time.Now().Add(-2 * time.Hour)})}

	report, err := Run(context.Background(), env, Options{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	kept := find(t, report, "4.1.0")
	if kept.Plan != PlanKeep || kept.ModuleBytes != 0 {
		t.Fatalf("plan %q with %d module bytes, want keep/0", kept.Plan, kept.ModuleBytes)
	}
	if _, err := os.Stat(filepath.Join(home, "cache", "kit", "vitrinka", "4.1.0", "node_modules")); err != nil {
		t.Fatalf("node_modules must survive when the manifest names it: %v", err)
	}
}

// Sweep is per-marker, strip and remove are per-version: a proven-dead marker
// on the ACTIVE version is still swept, while its content is untouchable.
func TestActiveVersionKeepsItsContentButLosesItsDeadMarkers(t *testing.T) {
	home := cache(t, "5.3.0", "5.3.0")
	marker(t, home, "5.3.0", 4242, time.Hour) // gone
	marker(t, home, "5.3.0", 42, time.Hour)   // live
	env := Env{Home: home, Processes: table(Process{PID: 42, Command: "/bin/claude", Started: time.Now().Add(-2 * time.Hour)})}

	report, err := Run(context.Background(), env, Options{Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.SweepMarkers != 1 {
		t.Fatalf("swept %d markers, want 1", report.SweepMarkers)
	}
	dir := filepath.Join(home, "cache", "kit", "vitrinka", "5.3.0")
	if _, err := os.Stat(filepath.Join(dir, MarkerDir, "4242")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dead marker should be swept even on the active version, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, MarkerDir, "42")); err != nil {
		t.Fatalf("live marker must survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); err != nil {
		t.Fatalf("the active version's content is untouchable: %v", err)
	}
	if report.Reclaimed != 0 {
		t.Fatalf("no tree should have been deleted, reclaimed %d", report.Reclaimed)
	}
}

func TestOnlyStripNeverRemovesAVersionDirectory(t *testing.T) {
	home := cache(t, "5.3.0", "3.9.0", "5.3.0")
	report, err := Run(context.Background(), Env{Home: home, Processes: table()},
		Options{Apply: true, Only: []Move{MoveStrip}})
	if err != nil {
		t.Fatal(err)
	}
	if report.RemoveBytes != 0 {
		t.Fatalf("remove was not enabled, got %d bytes", report.RemoveBytes)
	}
	dir := filepath.Join(home, "cache", "kit", "vitrinka", "3.9.0")
	if _, err := os.Stat(filepath.Join(dir, "skills", "publish", "SKILL.md")); err != nil {
		t.Fatalf("--only strip must leave the version directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "node_modules")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("--only strip must still strip, got %v", err)
	}
}
