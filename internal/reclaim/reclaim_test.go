package reclaim

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func write(t *testing.T, path string, size int, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	if age > 0 {
		when := time.Now().Add(-age)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
}

type fakeEnv struct {
	Env
	mu    sync.Mutex
	free  int64
	calls []string
}

func newFakeEnv(t *testing.T, home string, free int64) *fakeEnv {
	f := &fakeEnv{free: free}
	f.Env = Env{
		Home: home, Volume: home, Now: time.Now(), GOOS: "darwin",
		LookPath: func(name string) (string, error) {
			if name == "docker" || name == "xcrun" {
				return "/usr/bin/" + name, nil
			}
			return "", errors.New("missing")
		},
		Free: func(string) (int64, int64, error) { f.mu.Lock(); defer f.mu.Unlock(); return f.free, 1 << 40, nil },
		Exec: func(_ context.Context, name string, args ...string) ([]byte, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.calls = append(f.calls, name+" "+strings.Join(args, " "))
			if name == "docker" && args[0] == "builder" {
				f.free += 10 << 30
				return []byte("Total reclaimed space: 24.4GB\n"), nil
			}
			return nil, nil
		},
		Stderr: func(string) {},
	}
	return f
}

// A symlink inside the read-only directory (a .bun entry's link into the
// global store) is unlinked; the unlock never re-modes its target.
func TestRemoveTreeAccountsAndUnlocks(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a/b/one"), 100, 0)
	write(t, filepath.Join(root, "a/two"), 50, 0)
	write(t, filepath.Join(root, "store/pkg.js"), 7, 0)
	if err := os.Chmod(filepath.Join(root, "store/pkg.js"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "store/pkg.js"), filepath.Join(root, "a/b/link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "a/two"), 0o400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "a/b"), 0o500); err != nil {
		t.Fatal(err)
	}
	n, err := removeTree(context.Background(), filepath.Join(root, "a"), false)
	if err != nil {
		t.Fatalf("removeTree: %v", err)
	}
	if n < 150 {
		t.Fatalf("bytes = %d, want 150 plus the link", n)
	}
	if _, err := os.Stat(filepath.Join(root, "a")); !os.IsNotExist(err) {
		t.Fatal("tree should be gone")
	}
	if info, err := os.Stat(filepath.Join(root, "store/pkg.js")); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("the link's target must be untouched: %v %v", info, err)
	}
}

func TestRemoveTreeDryRunLeavesFiles(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "x/f"), 7, 0)
	n, err := removeTree(context.Background(), filepath.Join(root, "x"), true)
	if err != nil || n != 7 {
		t.Fatalf("dry: n=%d err=%v", n, err)
	}
	if _, err := os.Stat(filepath.Join(root, "x/f")); err != nil {
		t.Fatal("dry run must not delete")
	}
}

func TestRemoveAgedKeepsRecentAndTree(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "tmp/old.mov"), 1000, 90*24*time.Hour)
	write(t, filepath.Join(root, "tmp/new.mov"), 10, time.Hour)
	n, err := removeAged(context.Background(), filepath.Join(root, "tmp"), time.Now().AddDate(0, 0, -60), false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1000 {
		t.Fatalf("aged bytes = %d, want 1000", n)
	}
	if _, err := os.Stat(filepath.Join(root, "tmp/new.mov")); err != nil {
		t.Fatal("recent file must survive")
	}
	if _, err := os.Stat(filepath.Join(root, "tmp")); err != nil {
		t.Fatal("the tree itself must survive")
	}
}

func TestPlanRespectsTierOnlySkip(t *testing.T) {
	env := Env{GOOS: "darwin"}
	for _, s := range Plan(env, Options{MaxTier: TierCaches}) {
		if s.Tier > TierCaches {
			t.Fatalf("tier %d leaked into a --tier 2 plan: %s", s.Tier, s.ID)
		}
	}
	only := Plan(env, Options{Only: []string{"trash,go-build"}})
	if len(only) != 2 {
		t.Fatalf("only: got %d steps", len(only))
	}
	for _, s := range Plan(env, Options{Skip: []string{"go-build"}}) {
		if s.ID == "go-build" {
			t.Fatal("skip ignored")
		}
	}
	ids := map[string]bool{}
	for _, s := range Ladder(env) {
		if ids[s.ID] {
			t.Fatalf("duplicate step id %s", s.ID)
		}
		ids[s.ID] = true
		if s.Tier < TierBulk || s.Tier > TierAggressive {
			t.Fatalf("%s: tier %d out of ladder", s.ID, s.Tier)
		}
		if s.Regenerates == "" {
			t.Fatalf("%s: every step says what regenerates it", s.ID)
		}
		if len(s.Paths) == 0 && s.Run == nil {
			t.Fatalf("%s: neither paths nor a run func", s.ID)
		}
	}
	if len(Ladder(Env{GOOS: "linux"})) >= len(Ladder(env)) {
		t.Fatal("linux ladder must drop the mac-only steps")
	}
}

func TestLadderNeverNamesVolumesOrUserData(t *testing.T) {
	for _, s := range Ladder(Env{GOOS: "darwin"}) {
		for _, p := range s.Paths {
			if strings.Contains(p, "ScreenRecordings") || strings.Contains(p, "Messages/Attachments") || strings.Contains(p, "ms-playwright") && !strings.Contains(p, "ms-playwright-mcp") {
				t.Fatalf("%s names user data or the shared browser store: %s", s.ID, p)
			}
			if !strings.HasPrefix(p, "~/") {
				t.Fatalf("%s: path %q must be home-relative", s.ID, p)
			}
		}
	}
}

func TestRunDeletesInTierOrderAndReportsFree(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "Library/Caches/go-build/obj"), 4096, 0)
	write(t, filepath.Join(home, ".Trash/junk"), 512, 0)
	env := newFakeEnv(t, home, 1<<30)
	var seen []Result
	rep, err := Run(context.Background(), env.Env, Options{}, progressFunc(func(r Result) { seen = append(seen, r) }))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Result{}
	for _, r := range rep.Results {
		byID[r.ID] = r
	}
	if r := byID["go-build"]; r.Status != StatusDone || r.Bytes != 4096 {
		t.Fatalf("go-build: %+v", r)
	}
	if r := byID["docker-builder"]; r.Status != StatusDone || r.Bytes != 24_400_000_000 {
		t.Fatalf("docker-builder should parse the reclaimed line: %+v", r)
	}
	if r := byID["trash"]; r.Status != StatusDone || r.Bytes != 512 {
		t.Fatalf("trash: %+v", r)
	}
	if r := byID["brew"]; r.Status != StatusSkipped || !strings.Contains(r.Reason, "brew") {
		t.Fatalf("missing binary must skip, not fail: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(home, "Library/Caches/go-build")); !os.IsNotExist(err) {
		t.Fatal("go-build not removed")
	}
	if rep.Freed() != 10<<30 {
		t.Fatalf("Freed must be the df delta, got %d", rep.Freed())
	}
	for i := 1; i < len(seen); i++ {
		if seen[i].Tier < seen[i-1].Tier {
			t.Fatalf("tier %d reported after tier %d", seen[i].Tier, seen[i-1].Tier)
		}
	}
}

func TestRunStopsAtTarget(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".Trash/junk"), 512, 0)
	env := newFakeEnv(t, home, 1<<30)
	rep, err := Run(context.Background(), env.Env, Options{Until: 5 << 30}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Reached {
		t.Fatal("target should be reached after the docker prune bumped free space")
	}
	var trash Result
	for _, r := range rep.Results {
		if r.ID == "trash" {
			trash = r
		}
	}
	if trash.Status != StatusSkipped {
		t.Fatalf("tier 3 must not run once the target is met: %+v", trash)
	}
	if _, err := os.Stat(filepath.Join(home, ".Trash/junk")); err != nil {
		t.Fatal("trash was deleted after the target was met")
	}
}

func TestDryRunDeletesNothing(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "Library/Caches/go-build/obj"), 4096, 0)
	env := newFakeEnv(t, home, 1<<30)
	rep, err := Run(context.Background(), env.Env, Options{DryRun: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "Library/Caches/go-build/obj")); err != nil {
		t.Fatal("dry run deleted")
	}
	for _, r := range rep.Results {
		if r.Status == StatusDone {
			t.Fatalf("dry run reported done: %+v", r)
		}
	}
	for _, c := range env.calls {
		if strings.Contains(c, "prune") || strings.Contains(c, "delete") {
			t.Fatalf("dry run executed %q", c)
		}
	}
}

func TestUnusedRuntimes(t *testing.T) {
	runtimes := []byte(`{
	  "A": {"identifier":"A","version":"18.2","platformIdentifier":"iOS","sizeBytes":8000000000},
	  "B": {"identifier":"B","version":"17.5","platformIdentifier":"iOS","sizeBytes":7000000000},
	  "C": {"identifier":"C","version":"11.2","platformIdentifier":"watchOS","sizeBytes":3000000000}}`)
	devices := []byte(`{"devices":{
	  "com.apple.CoreSimulator.SimRuntime.iOS-18-2":[{"isAvailable":true}],
	  "com.apple.CoreSimulator.SimRuntime.iOS-17-5":[],
	  "com.apple.CoreSimulator.SimRuntime.watchOS-11-2":[]}}`)
	unused, err := UnusedRuntimes(runtimes, devices)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, rt := range unused {
		got[rt.Identifier] = true
	}
	if got["A"] || !got["B"] || !got["C"] {
		t.Fatalf("unused = %v", got)
	}
}

func TestSizes(t *testing.T) {
	if ParseReclaimed("Deleted build cache objects:\n\nTotal reclaimed space: 1.5GB") != 1_500_000_000 {
		t.Fatal("docker GB")
	}
	if n, _ := ParseHuman("100G"); n != 100<<30 {
		t.Fatal("100G")
	}
	if n, _ := ParseHuman("1.5T"); n != 1<<40+512<<30 {
		t.Fatal("1.5T")
	}
	if Human(77<<30) != "77.0G" || Human(1536) != "1.5K" || Human(500) != "500B" {
		t.Fatalf("Human: %s %s %s", Human(77<<30), Human(1536), Human(500))
	}
}

type progressFunc func(Result)

func (f progressFunc) Step(r Result)                     { f(r) }
func (progressFunc) TierDone(Tier, int64, time.Duration) {}

func TestSandboxTmpExceptsMessagesAndSignedDelta(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "Library/Containers/com.apple.MobileSMS/Data/tmp/old"), 1000, 90*24*time.Hour)
	write(t, filepath.Join(home, "Library/Containers/com.other.app/Data/tmp/old"), 10, 90*24*time.Hour)
	env := newFakeEnv(t, home, 1<<30)
	rep, err := Run(context.Background(), env.Env, Options{DryRun: true, Only: []string{"sandbox-tmp"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := rep.Results[0].Bytes; got != 10 {
		t.Fatalf("sandbox-tmp must skip the Messages tree: %d", got)
	}
	if Signed(-5<<20) != "-5.0M" || Signed(3<<30) != "+3.0G" {
		t.Fatalf("Signed: %s %s", Signed(-5<<20), Signed(3<<30))
	}
}

// A locked file (macOS user-immutable flag) is the one deletion failure a
// non-root test can stage. The tree walk must delete everything around it,
// count only what it removed, surface the error, and the ladder must carry on
// with the next step — a half-freed disk is still the goal.
func TestPartialFailureIsAggregatedAndTheLadderContinues(t *testing.T) {
	if goos := os.Getenv("GOOS"); goos != "" && goos != "darwin" {
		t.Skip("chflags uchg is macOS-only")
	}
	if _, err := exec.LookPath("chflags"); err != nil {
		t.Skip("chflags not available")
	}
	home := t.TempDir()
	locked := filepath.Join(home, "Library/Caches/go-build/locked")
	write(t, locked, 100, 0)
	write(t, filepath.Join(home, "Library/Caches/go-build/sub/free"), 300, 0)
	write(t, filepath.Join(home, ".npm/_cacache/x"), 50, 0)
	if out, err := exec.Command("chflags", "uchg", locked).CombinedOutput(); err != nil {
		t.Skipf("cannot lock file: %v %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("chflags", "nouchg", locked).Run() })

	n, err := removeTree(context.Background(), filepath.Join(home, "Library/Caches/go-build"), false)
	if err == nil {
		t.Fatal("a locked file must surface as an error")
	}
	if n != 300 {
		t.Fatalf("only the deleted bytes count: got %d, want 300", n)
	}
	if _, err := os.Stat(filepath.Join(home, "Library/Caches/go-build/sub")); !os.IsNotExist(err) {
		t.Fatal("the deletable sibling subtree must be gone")
	}
	if _, err := os.Stat(locked); err != nil {
		t.Fatal("the locked file must still exist")
	}

	write(t, filepath.Join(home, "Library/Caches/go-build/sub/free"), 300, 0)
	env := newFakeEnv(t, home, 1<<30)
	rep, err := Run(context.Background(), env.Env, Options{Only: []string{"go-build,npm"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Result{}
	for _, r := range rep.Results {
		byID[r.ID] = r
	}
	if r := byID["go-build"]; r.Status != StatusFailed || r.Bytes != 300 || !strings.Contains(r.Error, "locked") {
		t.Fatalf("go-build should report the partial failure with its bytes: %+v", r)
	}
	if r := byID["npm"]; r.Status != StatusDone || r.Bytes != 50 {
		t.Fatalf("the ladder must continue past a failed step: %+v", r)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// psOutput makes the fake env answer `ps` with the given table.
func psOutput(env *fakeEnv, table string) {
	inner := env.Exec
	env.Exec = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		if name == "ps" {
			return []byte(table), nil
		}
		return inner(ctx, name, args...)
	}
}

// A globalStore checkout's .bun entries are absolute symlinks into links/;
// the bun step deletes tarballs, index dirs and manifests around it and the
// checkout still resolves. Dot-entries (in-flight staging) stay too, and a
// running non-install bun (a dev server) does not block the step.
func TestBunStepKeepsLinksAndCheckoutsResolve(t *testing.T) {
	home := t.TempDir()
	cache := filepath.Join(home, ".bun/install/cache")
	write(t, filepath.Join(cache, "links/is-odd@3.0.1-abc/node_modules/is-odd/index.js"), 10, 0)
	write(t, filepath.Join(cache, "is-odd@3.0.1@@@1/index.js"), 100, 0)
	symlink(t, filepath.Join(cache, "is-odd@3.0.1@@@1"), filepath.Join(cache, "is-odd/3.0.1@@@1"))
	write(t, filepath.Join(cache, "@s/b@2.0.0@@@1/x.js"), 50, 0)
	write(t, filepath.Join(cache, "abc.npm"), 7, 0)
	write(t, filepath.Join(cache, ".staging-1/partial"), 5, 0)
	checkout := filepath.Join(home, "app/node_modules/.bun/is-odd@3.0.1")
	symlink(t, filepath.Join(cache, "links/is-odd@3.0.1-abc"), checkout)

	env := newFakeEnv(t, home, 1<<30)
	psOutput(env, "  900 /Users/x/.bun/bin/bun run app:dev\n  901 bun index.ts\n")
	rep, err := Run(context.Background(), env.Env, Options{Only: []string{"bun"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := rep.Results[0]; r.Status != StatusDone || r.Bytes < 157 {
		t.Fatalf("bun step: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(checkout, "node_modules/is-odd/index.js")); err != nil {
		t.Fatalf("the checkout must still resolve through links/: %v", err)
	}
	for _, gone := range []string{"is-odd@3.0.1@@@1", "is-odd", "@s", "abc.npm"} {
		if exists(filepath.Join(cache, gone)) {
			t.Errorf("%s should be deleted", gone)
		}
	}
	if !exists(filepath.Join(cache, ".staging-1/partial")) {
		t.Error("dot-entries (in-flight staging) must survive")
	}
	if trashes, _ := filepath.Glob(filepath.Join(home, ".bun/install", bunTrashPrefix+"*")); len(trashes) > 0 {
		t.Errorf("trash left behind: %v", trashes)
	}
}

func TestBunStepSkipsWhileAnInstallRuns(t *testing.T) {
	// Global flags may precede the verb. bunx writes the cache too, though
	// never a checkout's node_modules (bun-prune lets it run).
	for _, install := range []string{"/Users/x/.bun/bin/bun install --frozen-lockfile", "bun --cwd apps/web add zod", "bunx expo start"} {
		home := t.TempDir()
		write(t, filepath.Join(home, ".bun/install/cache/is-odd@3.0.1@@@1/index.js"), 100, 0)
		env := newFakeEnv(t, home, 1<<30)
		psOutput(env, "  900 "+install+"\n")
		rep, err := Run(context.Background(), env.Env, Options{Only: []string{"bun"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if r := rep.Results[0]; r.Status != StatusSkipped || !strings.Contains(r.Reason, "pid 900") {
			t.Fatalf("a running %q must skip the step: %+v", install, r)
		}
		if !exists(filepath.Join(home, ".bun/install/cache/is-odd@3.0.1@@@1/index.js")) {
			t.Fatalf("nothing may be deleted while %q runs", install)
		}
	}
}

// bunPruneTree builds a globalStore checkout in miniature. Reachable: react
// (root), @s+local (scoped root, a project-local dir), dep (only through
// @s+local's own links), deeper (only through dep, transitively), tool (via
// .bin), api-only (via a workspace package) and fallback (via .bun's
// fallback node_modules). Unreachable: the stale-dir directory, and the
// symlinks stale-link and scheduler, which only react's links/ entry names
// (links/ resolves inside links/, never back into the checkout); symlinks
// are counted, never deleted. young is an unreachable dir, but fresh.
// native and native-dep (through native's own links) are unreachable from
// node_modules but kept: apps/api/ios/Podfile.lock names native.
func bunPruneTree(t *testing.T) (checkout, links string) {
	t.Helper()
	root := t.TempDir()
	links = filepath.Join(root, "links")
	checkout = filepath.Join(root, "app")
	nm := filepath.Join(checkout, "node_modules")
	store := filepath.Join(nm, ".bun")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "package.json"), []byte(`{"workspaces":["apps/*"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"react-h", "deeper-h", "tool-h", "api-h", "fallback-h", "stale-h", "scheduler-h"} {
		write(t, filepath.Join(links, name, "node_modules/pkg/index.js"), 1, 0)
	}
	symlink(t, "../../scheduler-h/node_modules/pkg", filepath.Join(links, "react-h/node_modules/scheduler"))

	symlink(t, ".bun/react@19/node_modules/react", filepath.Join(nm, "react"))
	symlink(t, "../.bun/@s+local@1+hash/node_modules/@s/local", filepath.Join(nm, "@s/local"))
	symlink(t, "../.bun/tool@1/node_modules/tool/bin/tool", filepath.Join(nm, ".bin/tool"))
	symlink(t, "../../../node_modules/.bun/api-only@1/node_modules/api-only", filepath.Join(checkout, "apps/api/node_modules/api-only"))
	write(t, filepath.Join(checkout, "apps/.DS_Store"), 8, 0) // `apps/*` matches files too
	symlink(t, "../fallback@1/node_modules/fallback", filepath.Join(store, "node_modules/fallback"))

	symlink(t, filepath.Join(links, "react-h"), filepath.Join(store, "react@19"))
	write(t, filepath.Join(store, "@s+local@1+hash/node_modules/@s/local/index.js"), 3, 0)
	symlink(t, "../../../dep@2/node_modules/dep", filepath.Join(store, "@s+local@1+hash/node_modules/@s/dep"))
	write(t, filepath.Join(store, "dep@2/node_modules/dep/index.js"), 3, 0)
	symlink(t, "../../deeper@3/node_modules/deeper", filepath.Join(store, "dep@2/node_modules/deeper"))
	symlink(t, filepath.Join(links, "deeper-h"), filepath.Join(store, "deeper@3"))
	symlink(t, filepath.Join(links, "tool-h"), filepath.Join(store, "tool@1"))
	symlink(t, filepath.Join(links, "api-h"), filepath.Join(store, "api-only@1"))
	symlink(t, filepath.Join(links, "fallback-h"), filepath.Join(store, "fallback@1"))
	symlink(t, filepath.Join(links, "scheduler-h"), filepath.Join(store, "scheduler@0"))
	symlink(t, filepath.Join(links, "stale-h"), filepath.Join(store, "stale-link@0"))
	write(t, filepath.Join(store, "stale-dir@0/node_modules/stale/big.bin"), 4000, 0)
	write(t, filepath.Join(store, "young@0/node_modules/young/index.js"), 9, 0)
	write(t, filepath.Join(store, "native@1/node_modules/native/ios/a.m"), 6, 0)
	symlink(t, "../../native-dep@1/node_modules/native-dep", filepath.Join(store, "native@1/node_modules/native-dep"))
	write(t, filepath.Join(store, "native-dep@1/node_modules/native-dep/index.js"), 6, 0)
	pods := "EXTERNAL SOURCES:\n  Native:\n    :path: \"../../../node_modules/.bun/native@1/node_modules/native/ios\"\n" +
		"  React:\n    :path: \"../../../node_modules/.bun/react@19/node_modules/react\"\n"
	if err := os.MkdirAll(filepath.Join(checkout, "apps/api/ios"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "apps/api/ios/Podfile.lock"), []byte(pods), 0o644); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(nm, ".old_modules-abc/f.bin"), 1000, 0)
	// A hardlink shared by the leftover and a stale entry counts once.
	if err := os.Link(filepath.Join(nm, ".old_modules-abc/f.bin"), filepath.Join(store, "stale-dir@0/node_modules/stale/f.bin")); err != nil {
		t.Fatal(err)
	}
	return checkout, links
}

func noBun(context.Context) ([]BunProcess, error) { return nil, nil }

func TestBunPruneReachabilityWalk(t *testing.T) {
	checkout, _ := bunPruneTree(t)
	now := time.Now().Add(48 * time.Hour) // everything built above is two days old
	young := filepath.Join(checkout, "node_modules/.bun/young@0")
	if err := os.Chtimes(young, now, now); err != nil {
		t.Fatal(err)
	}
	rep, err := BunPrune(context.Background(), BunPruneOptions{Checkout: checkout, Now: now, BunCwds: noBun})
	if err != nil {
		t.Fatal(err)
	}
	var unreachable []string
	for _, e := range rep.Unreachable {
		unreachable = append(unreachable, e.Name+":"+string(e.Kind))
	}
	sort.Strings(unreachable)
	if got, want := strings.Join(unreachable, " "), "stale-dir@0:dir"; got != want {
		t.Fatalf("unreachable = %s, want %s", got, want)
	}
	if rep.Entries != 13 || rep.Reachable != 7 || rep.UnreachableLinks != 2 {
		t.Fatalf("entries %d reachable %d links %d, want 13, 7 and 2 (scheduler, stale-link)", rep.Entries, rep.Reachable, rep.UnreachableLinks)
	}
	var native []string
	for _, e := range rep.Native {
		native = append(native, e.Name)
	}
	sort.Strings(native)
	if got := strings.Join(native, " "); got != "native-dep@1 native@1" || rep.NativeBytes != 12 ||
		strings.Join(rep.NativeManifests, " ") != filepath.FromSlash("apps/api/ios/Podfile.lock") {
		t.Fatalf("kept_native = %s (%d bytes) from %v", got, rep.NativeBytes, rep.NativeManifests)
	}
	if len(rep.Young) != 1 || rep.Young[0].Name != "young@0" {
		t.Fatalf("young = %+v", rep.Young)
	}
	if len(rep.Leftovers) != 1 || rep.Leftovers[0].Name != ".old_modules-abc" {
		t.Fatalf("leftovers = %+v", rep.Leftovers)
	}
	if rep.Bytes != 5000 {
		t.Fatalf("bytes = %d, want 5000 (the shared hardlink counted once)", rep.Bytes)
	}
	if !exists(filepath.Join(checkout, "node_modules/.bun/stale-dir@0")) {
		t.Fatal("a dry run deleted")
	}
}

func TestBunPruneApplyRefusesBusyAndDeletesOnlyCandidates(t *testing.T) {
	checkout, _ := bunPruneTree(t)
	now := time.Now().Add(48 * time.Hour)
	if err := os.Chtimes(filepath.Join(checkout, "node_modules/.bun/young@0"), now, now); err != nil {
		t.Fatal(err)
	}
	busy := func(context.Context) ([]BunProcess, error) {
		// lsof reports the disk's case; the checkout may be spelled otherwise.
		return []BunProcess{{PID: 42, Cwd: strings.ToUpper(filepath.Join(checkout, "apps/api")), Args: "bun add zod"}}, nil
	}
	if _, err := BunPrune(context.Background(), BunPruneOptions{Checkout: checkout, Apply: true, Now: now, BunCwds: busy}); err == nil || !strings.Contains(err.Error(), "bun is running inside") {
		t.Fatalf("a bun process in the checkout must refuse --apply: %v", err)
	}
	store := filepath.Join(checkout, "node_modules/.bun")
	if !exists(filepath.Join(store, "stale-dir@0")) {
		t.Fatal("a refused apply deleted")
	}
	rep, err := BunPrune(context.Background(), BunPruneOptions{Checkout: checkout, Apply: true, Now: now, BunCwds: noBun})
	if err != nil || !rep.Applied || len(rep.Errors) > 0 {
		t.Fatalf("apply: %v %+v", err, rep)
	}
	for _, gone := range []string{"stale-dir@0", "../.old_modules-abc"} {
		if exists(filepath.Join(store, gone)) {
			t.Errorf("%s should be deleted", gone)
		}
	}
	for _, kept := range []string{"stale-link@0", "scheduler@0", "young@0", "native@1", "native-dep@1"} {
		if !exists(filepath.Join(store, kept)) {
			t.Errorf("%s must survive: symlinks are only counted, young and Podfile.lock-named entries kept", kept)
		}
	}
	if _, err := os.Stat(filepath.Join(checkout, "node_modules/@s/local/index.js")); err != nil {
		t.Fatalf("a reachable project-local package must survive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store, "deeper@3/node_modules/pkg/index.js")); err != nil {
		t.Fatalf("a transitively reachable entry must survive: %v", err)
	}
	if trashes, _ := filepath.Glob(filepath.Join(checkout, "node_modules", bunPruneTrashPrefix+"*")); len(trashes) > 0 {
		t.Errorf("trash left behind: %v", trashes)
	}
}

// An install that starts and finishes between the two busy checks (here:
// relinking a root to stale-dir@0, an old entry, so not young) is invisible
// to both; the fence over what the walk read refuses the delete.
func TestBunPruneApplyRefusesAnInstallThatRanDuringTheWalk(t *testing.T) {
	checkout, _ := bunPruneTree(t)
	now := time.Now().Add(48 * time.Hour)
	checks := 0
	installDuringWalk := func(context.Context) ([]BunProcess, error) {
		if checks++; checks == 2 {
			symlink(t, ".bun/stale-dir@0/node_modules/stale", filepath.Join(checkout, "node_modules/stale"))
		}
		return nil, nil
	}
	_, err := BunPrune(context.Background(), BunPruneOptions{Checkout: checkout, Apply: true, Now: now, BunCwds: installDuringWalk})
	if err == nil || !strings.Contains(err.Error(), "changed during the walk") {
		t.Fatalf("a root relinked during the walk must refuse --apply: %v", err)
	}
	if !exists(filepath.Join(checkout, "node_modules/.bun/stale-dir@0/node_modules/stale/big.bin")) {
		t.Fatal("the entry the install relinked was deleted")
	}
}

// The same race where no walked node_modules directory moves: the install
// replaced a store entry, the workspace list gained a pattern whose package
// links stale-dir@0, or a new package directory appeared under a pattern.
// Each must refuse, and stale-dir@0 must survive.
func TestBunPruneApplyRefusesAStoreOrWorkspaceChangeDuringTheWalk(t *testing.T) {
	cases := map[string]func(t *testing.T, checkout string){
		"store entry replaced": func(t *testing.T, checkout string) {
			store := filepath.Join(checkout, "node_modules/.bun")
			if err := os.Rename(filepath.Join(store, "stale-dir@0"), filepath.Join(store, ".stale-dir@0-old")); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(store, "stale-dir@0/node_modules/stale/big.bin"), 4000, 0)
		},
		"workspace list edited": func(t *testing.T, checkout string) {
			if err := os.WriteFile(filepath.Join(checkout, "package.json"), []byte(`{"workspaces":["apps/*","tools/*"]}`), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		"workspace package added": func(t *testing.T, checkout string) {
			symlink(t, "../../../node_modules/.bun/stale-dir@0/node_modules/stale", filepath.Join(checkout, "apps/web/node_modules/stale"))
		},
	}
	for name, install := range cases {
		t.Run(name, func(t *testing.T) {
			checkout, _ := bunPruneTree(t)
			// Outside the workspace list until the edit adds tools/*.
			symlink(t, "../../../node_modules/.bun/stale-dir@0/node_modules/stale", filepath.Join(checkout, "tools/gen/node_modules/stale"))
			now := time.Now().Add(48 * time.Hour)
			checks := 0
			installDuringWalk := func(context.Context) ([]BunProcess, error) {
				if checks++; checks == 2 {
					install(t, checkout)
				}
				return nil, nil
			}
			_, err := BunPrune(context.Background(), BunPruneOptions{Checkout: checkout, Apply: true, Now: now, BunCwds: installDuringWalk})
			if err == nil || !strings.Contains(err.Error(), "changed during the walk") {
				t.Fatalf("%s during the walk must refuse --apply: %v", name, err)
			}
			if !exists(filepath.Join(checkout, "node_modules/.bun/stale-dir@0/node_modules/stale/big.bin")) {
				t.Fatal("the entry the install made live was deleted")
			}
		})
	}
}

// A workspace match that exists but cannot be stat'ed (here apps/ is
// listable but not searchable) may be a package whose entries are live: the
// plan fails rather than dropping its root.
func TestBunPruneRefusesAnUnreadableWorkspaceMatch(t *testing.T) {
	checkout, _ := bunPruneTree(t)
	apps := filepath.Join(checkout, "apps")
	if err := os.Chmod(apps, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(apps, 0o755) })
	_, err := BunPrune(context.Background(), BunPruneOptions{Checkout: checkout, BunCwds: noBun})
	if err == nil || !strings.Contains(err.Error(), `workspace pattern "apps/*"`) {
		t.Fatalf("an unreadable workspace match must fail the plan: %v", err)
	}
}

// A bun in a checkout nested under the pruned one (a worktree's .git file, a
// clone's .git dir) with a package.json of its own is its own project and does
// not hold the prune. Anywhere else under the checkout it does, and so does a
// nested checkout bun resolves to the pruned root: no package.json on the way
// up, or one the root's workspaces list. The checkout may be typed in another
// case than lsof reports.
func TestBunPruneBusyCheckSkipsNestedBunProjects(t *testing.T) {
	checkout := filepath.Join(t.TempDir(), "App")
	file := func(rel, body string) {
		t.Helper()
		path := filepath.Join(checkout, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	file("package.json", `{"workspaces":["apps/*"]}`)
	file("apps/api/package.json", `{}`)
	file(".worktrees/wt/.git", "gitdir: ../../.git/worktrees/wt\n")
	file(".worktrees/wt/package.json", `{"workspaces":["apps/*"]}`)
	file(".worktrees/wt/apps/api/package.json", `{}`)
	file("vendor/clone/.git/HEAD", "ref: refs/heads/main\n")
	file("vendor/clone/package.json", `{}`)
	file(".worktrees/no-pkg/.git", "gitdir: ../../.git/worktrees/no-pkg\n")
	file("apps/submodule/.git", "gitdir: ../../.git/modules/submodule\n")
	file("apps/submodule/package.json", `{}`)
	// Look-alikes that are no checkout: a plain dir under .worktrees, and one
	// whose .git is only a symlink.
	file(".worktrees/plain/package.json", `{}`)
	file(".worktrees/linked/package.json", `{}`)
	if err := os.Symlink(filepath.Join(checkout, ".worktrees/wt/.git"), filepath.Join(checkout, ".worktrees/linked/.git")); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		cwd     string
		refuses bool
	}{
		{"", true},
		{"apps/api", true},
		{".worktrees/wt/apps/api", false},
		{".worktrees/wt", false},
		{"vendor/clone", false},
		{".worktrees/no-pkg", true},
		{"apps/submodule", true},
		{".worktrees/plain", true},
		{".worktrees/linked", true},
	}
	for _, spelling := range []string{checkout, strings.ToUpper(checkout)} {
		for _, c := range cases {
			cwd := filepath.Join(checkout, c.cwd)
			procs := func(context.Context) ([]BunProcess, error) { return []BunProcess{{PID: 7, Cwd: cwd}}, nil }
			err := refuseBusyCheckout(context.Background(), BunPruneOptions{BunCwds: procs}, []string{spelling})
			if refused := err != nil; refused != c.refuses {
				t.Errorf("checkout %s, bun in %q: refused = %v (%v), want %v", spelling, c.cwd, refused, err, c.refuses)
			}
		}
	}
}

// Only a bun that writes node_modules holds the prune: the install family in
// every argv shape (full path, flag-first, a flag value before the verb, which
// bun itself would read as the verb, so counting it only over-counts), or a
// bun whose command line ps no longer listed. A session launcher, a dev server
// or bunx with its cwd in the checkout root only resolves modules.
func TestBunPruneBusyCheckCountsOnlyInstalls(t *testing.T) {
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "package.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		args    string
		refuses bool
	}{
		{"bun /Users/x/Work/Projects/Libraries/claude-switcheroo/bin/switcheroo start --model opus", false},
		{"bun /Users/x/.local/bin/switcheroo start --resume-pick", false},
		{"bun run app:dev:e2e", false},
		{"/Users/x/.bun/bin/bun --cwd apps/web run dev", false},
		{"bun --bun next dev", false},
		{"bun --watch src/index.ts", false},
		{"bun index.ts", false},
		{"bun cli.ts", false},
		{"bun x expo start", false},
		{"bunx expo start", false},
		{"bun install", true},
		{"/Users/x/.bun/bin/bun install --frozen-lockfile", true},
		{"bun i", true},
		{"bun --cwd x install", true},
		{"bun --cwd=apps/web add zod", true},
		{"bun --filter @fixit/api a zod", true},
		{"bun --silent add x", true},
		{"bun remove zod", true},
		{"bun rm zod", true},
		{"bun uninstall zod", true},
		{"bun update", true},
		{"bun upgrade", true},
		{"bun link", true},
		{"bun unlink", true},
		{"bun pm trust --all", true},
		{"bun patch react", true},
		{"bun patch-commit node_modules/react", true},
		{"bun ci", true},
		{"bun create vite scratch", true},
		{"bun c vite scratch", true},
		{"bun init -y", true},
		{"", true},
	}
	for _, c := range cases {
		procs := func(context.Context) ([]BunProcess, error) {
			return []BunProcess{{PID: 7, Cwd: checkout, Args: c.args}}, nil
		}
		err := refuseBusyCheckout(context.Background(), BunPruneOptions{BunCwds: procs}, []string{checkout})
		if refused := err != nil; refused != c.refuses {
			t.Errorf("bun %q in the checkout root: refused = %v (%v), want %v", c.args, refused, err, c.refuses)
		}
	}
}

func TestParseLsofCwds(t *testing.T) {
	got := parseLsofCwds([]byte("p123\nfcwd\nn/w/app\np456\nfcwd\nn/w/other dir\n"))
	if len(got) != 2 || got[0] != (BunProcess{PID: 123, Cwd: "/w/app"}) || got[1] != (BunProcess{PID: 456, Cwd: "/w/other dir"}) {
		t.Fatalf("got %+v", got)
	}
}
