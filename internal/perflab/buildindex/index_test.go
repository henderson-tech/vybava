package buildindex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) Store {
	t.Helper()
	root := t.TempDir()
	return Store{Dirs: Dirs{Cache: filepath.Join(root, "cache"), State: filepath.Join(root, "state")}}
}

var androidTarget = Target{Platform: "android", Profile: "perf", Kind: KindBundled}

// putBuild stores an Android build entry directly, as BuildNative would.
func putBuild(t *testing.T, s Store, key, sourceKey string, created time.Time) Build {
	t.Helper()
	st, err := newStage(s.buildDir(androidTarget, key))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(st.Dir, "app-release.apk"), []byte("apk "+key), 0o644); err != nil {
		t.Fatal(err)
	}
	m := BuildManifest{Key: key, SourceKey: sourceKey, Platform: "android", Profile: "perf", Kind: KindBundled,
		AppID: "app.fixit.client.dev", CreatedAt: created, Artifact: "app-release.apk", EnvNames: []string{}}
	res, err := s.commitBuild(st, m, Fingerprint{Key: key, Lines: []string{"platform\tandroid"}})
	if err != nil {
		t.Fatal(err)
	}
	return res.Build
}

func TestStoreNeverOverwritesAnEntry(t *testing.T) {
	s := testStore(t)
	first := putBuild(t, s, "pf1-aaaaaaaaaaaaaaaaaaaa", "pf1s-x", time.Now().UTC())
	second := putBuild(t, s, "pf1-aaaaaaaaaaaaaaaaaaaa", "pf1s-other", time.Now().UTC())
	if second.Manifest.SourceKey != "pf1s-x" {
		t.Fatalf("the second write replaced the entry: %+v", second.Manifest)
	}
	if first.Dir != second.Dir {
		t.Fatalf("dirs differ: %s %s", first.Dir, second.Dir)
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(first.Dir), ".tmp-*"))
	if len(leftovers) != 0 {
		t.Fatalf("staging dirs left behind: %v", leftovers)
	}
	if b, err := s.FindBuild(androidTarget, "pf1-aaaaaaaaaaaaaaaaaaaa"); err != nil || b.Manifest.Key != "pf1-aaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("find: %v %+v", err, b)
	}
	if !fileExists(filepath.Join(first.Dir, "key-lines.txt")) {
		t.Fatal("key lines not stored beside the build")
	}
}

func TestFindBuildMissesAndLostArtifacts(t *testing.T) {
	s := testStore(t)
	_, err := s.FindBuild(androidTarget, "pf1-bbbbbbbbbbbbbbbbbbbb")
	d := wantCode(t, err, DiagBuildNotFound)
	if !strings.Contains(d.Diag.Fix, "perflab build native --platform android --profile perf --kind bundled") {
		t.Fatalf("fix = %s", d.Diag.Fix)
	}
	b := putBuild(t, s, "pf1-cccccccccccccccccccc", "pf1s-x", time.Now().UTC())
	if err := os.Remove(b.Path); err != nil {
		t.Fatal(err)
	}
	_, err = s.FindBuild(androidTarget, b.Manifest.Key)
	wantCode(t, err, DiagBuildArtifactMissing)
	if fileExists(b.Dir) {
		t.Fatal("the artifact-less entry should be dropped so a rebuild can land")
	}
}

func TestGCKeepsNewestAndPinned(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC()
	old := putBuild(t, s, "pf1-111111111111111111aa", "pf1s-x", now.Add(-3*time.Hour))
	pinned := putBuild(t, s, "pf1-222222222222222222aa", "pf1s-x", now.Add(-2*time.Hour))
	newest := putBuild(t, s, "pf1-333333333333333333aa", "pf1s-x", now.Add(-time.Hour))
	// the entries were touched by commitBuild's FindBuild; age the markers
	for _, b := range []Build{old, pinned, newest} {
		_ = os.Chtimes(filepath.Join(b.Dir, usedMarker), b.Manifest.CreatedAt, b.Manifest.CreatedAt)
	}
	res, err := s.GC(1, 0, []string{pinned.Manifest.Key}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != old.Manifest.Key {
		t.Fatalf("removed = %v, want only the oldest", res.Removed)
	}
	if !fileExists(pinned.Dir) || !fileExists(newest.Dir) || fileExists(old.Dir) {
		t.Fatal("gc removed the wrong entries")
	}
}

func TestBuildNativeAnswersAHitWithoutBuilding(t *testing.T) {
	s := testStore(t)
	b := putBuild(t, s, "pf1-dddddddddddddddddddd", "pf1s-x", time.Now().UTC())
	r := newFake(t) // no command may run
	fp := Fingerprint{Key: b.Manifest.Key}
	res, err := s.BuildNative(context.Background(), r, BuildSpec{Target: androidTarget, Fingerprint: &fp})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Hit || res.Build.Manifest.Key != fp.Key {
		t.Fatalf("want a hit, got %+v", res)
	}
	if len(r.calls) != 0 {
		t.Fatalf("a hit ran commands: %v", r.calls)
	}
}

func TestBuildSerialisation(t *testing.T) {
	fp := Fingerprint{Key: "pf1-eeeeeeeeeeeeeeeeeeee"}
	spec := BuildSpec{Target: androidTarget, Fingerprint: &fp}
	t.Run("same key in progress", func(t *testing.T) {
		s := testStore(t)
		release, _, err := s.Dirs.takeExclusive("build-"+fp.Key+".lock", 0, LockHolder{PID: 4242, Verb: "build native"})
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		_, err = s.BuildNative(context.Background(), newFake(t), spec)
		d := wantCode(t, err, DiagBuildInProgress)
		if !strings.Contains(d.Diag.Detail, "pid 4242") {
			t.Fatalf("holder not named: %s", d.Diag.Detail)
		}
	})
	t.Run("another build holds the host", func(t *testing.T) {
		s := testStore(t)
		release, _, err := s.Dirs.takeExclusive(hostBuildLock, 0, LockHolder{PID: 777, Verb: "build native", Key: "pf1-other"})
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		_, err = s.BuildNative(context.Background(), newFake(t), spec)
		d := wantCode(t, err, DiagHostBusyBuilding)
		if !strings.Contains(d.Diag.Detail, "pf1-other") {
			t.Fatalf("holder not named: %s", d.Diag.Detail)
		}
	})
	t.Run("a run measures", func(t *testing.T) {
		s := testStore(t)
		release, err := AcquireMeasure(s.Dirs, LockHolder{Verb: "run", Device: "s20"})
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		_, err = s.BuildNative(context.Background(), newFake(t), spec)
		d := wantCode(t, err, DiagHostBusyBuilding)
		if !strings.Contains(d.Diag.Detail, "device s20") {
			t.Fatalf("the measuring run is not named: %s", d.Diag.Detail)
		}
	})
	t.Run("a measure refuses while building", func(t *testing.T) {
		s := testStore(t)
		release, _, err := s.Dirs.takeExclusive(hostBuildLock, 0, LockHolder{PID: 9, Verb: "build native", Key: "pf1-x"})
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		if h, err := HostBuildHolder(s.Dirs); err != nil || h == nil || h.Key != "pf1-x" {
			t.Fatalf("holder = %+v, %v", h, err)
		}
		_, err = AcquireMeasure(s.Dirs, LockHolder{Verb: "run"})
		wantCode(t, err, DiagHostBusyBuilding)
	})
	t.Run("free host", func(t *testing.T) {
		s := testStore(t)
		if h, err := HostBuildHolder(s.Dirs); err != nil || h != nil {
			t.Fatalf("holder = %+v, %v", h, err)
		}
		release, err := AcquireMeasure(s.Dirs, LockHolder{Verb: "probe"})
		if err != nil {
			t.Fatal(err)
		}
		release()
	})
}

// An import of another app's artifact (the store build beside the dev
// variant) is refused before the copy, and the fix is a runnable build.
func TestImportAppID(t *testing.T) {
	p := Project{IOS: IOSApp{BundleID: "app.fixit.client"}, Android: AndroidApp{Package: "app.fixit.client.dev"}}
	for _, tc := range []struct {
		target  Target
		got     string
		wantFix string
	}{
		{androidTarget, "app.fixit.client.dev", ""},
		{androidTarget, "app.fixit.client", "perflab build native --platform android --profile perf --kind bundled --json"},
		{Target{Platform: "ios", Profile: "perf", Kind: KindShell}, "app.fixit.client", ""},
		{Target{Platform: "ios", Profile: "perf", Kind: KindShell}, "app.other", "perflab build native --platform ios --profile perf --kind shell --json"},
	} {
		err := importAppID(p, tc.target, tc.got)
		if tc.wantFix == "" {
			if err != nil {
				t.Errorf("%s %s: unexpected %v", tc.target.Platform, tc.got, err)
			}
			continue
		}
		if d := wantCode(t, err, DiagBuildFailed); d.Diag.Fix != tc.wantFix {
			t.Errorf("%s %s: fix %q, want %q", tc.target.Platform, tc.got, d.Diag.Fix, tc.wantFix)
		}
	}
}

// Two phones' runs from one checkout share the adapter's runner resources
// (FixIt's Appium port 14021 on 2026-10-02: the S20 case died EADDRINUSE
// while the iPhone 11 run held it); another checkout runs alongside.
func TestProjectRunnerSerialisesOneCheckout(t *testing.T) {
	s := testStore(t)
	release, _, err := AcquireProjectRunner(s.Dirs, "/wt/perflab-acceptance", 0, LockHolder{PID: 4242, Verb: "run", Device: "iphone11"})
	if err != nil {
		t.Fatal(err)
	}
	_, busy, err := AcquireProjectRunner(s.Dirs, "/wt/perflab-acceptance/", 50*time.Millisecond, LockHolder{Verb: "run", Device: "s20"})
	if !errors.Is(err, ErrRunnerBusy) || busy == nil || busy.Device != "iphone11" {
		t.Fatalf("second run from the same checkout: err %v holder %+v", err, busy)
	}
	other, _, err := AcquireProjectRunner(s.Dirs, "/wt/another-checkout", 0, LockHolder{Verb: "run", Device: "s20"})
	if err != nil {
		t.Fatalf("another checkout must not wait: %v", err)
	}
	other()
	release()
	again, _, err := AcquireProjectRunner(s.Dirs, "/wt/perflab-acceptance", 0, LockHolder{Verb: "run", Device: "s20"})
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	again()
}

// After a bundle export the next line packs onto the indexed native build
// whose native sources match (the FixIt sweep read `--native <key>` while
// the index held pf1-eee19b76f90b2b546ecc), the newest of them; without
// one it builds first.
func TestPackNextNamesTheMatchingNativeBuild(t *testing.T) {
	s := testStore(t)
	putBuild(t, s, "pf1-0000000000000000000a", "pf1s-old", time.Now().Add(-2*time.Hour).UTC())
	putBuild(t, s, "pf1-0000000000000000000b", "pf1s-src", time.Now().Add(-time.Hour).UTC())
	putBuild(t, s, "pf1-0000000000000000000c", "pf1s-src", time.Now().UTC())
	if got := s.packNext(androidTarget, "pf1s-src", "da4288d812090000"); len(got) != 1 || got[0] != "perflab pack --native pf1-0000000000000000000c --bundle da4288d812090000 --json" {
		t.Errorf("next = %q", got)
	}
	if got := s.packNext(androidTarget, "pf1s-new", "da4288d812090000"); len(got) != 2 || !strings.HasPrefix(got[0], "perflab build native --platform android --profile perf --kind bundled") {
		t.Errorf("no matching build: next = %q", got)
	}
}
