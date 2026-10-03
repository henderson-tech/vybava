package uiloop

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// cachedPass is stagePass with provenance and a clock an hour ahead, so every
// file is past the racy window and the digest cache records it.
func cachedPass(t *testing.T) (*Tool, string) {
	t.Helper()
	tool := newTool(t, testConfig())
	evidenceRepo(t, tool)
	if err := tool.captureProvenance(1, false); err != nil {
		t.Fatal(err)
	}
	dir := stagePass(t, tool)
	tool.Now = func() time.Time { return time.Now().Add(time.Hour) }
	return tool, dir
}

func stateOf(t *testing.T, tool *Tool) (StateData, []runxDiagnostic) {
	t.Helper()
	res, err := tool.State(StateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return res.Data.(StateData), res.Diagnostics
}

func cacheWarnings(diags []runxDiagnostic) int {
	n := 0
	for _, d := range diags {
		if d.Code == DiagDigestCache {
			n++
		}
	}
	return n
}

// A cache entry counts only while the file's stamp is unchanged: a shot
// rewritten in place with its size and mtime restored is hashed again and
// reopens its screen alone. A warm read equals a cold one, and the cache is
// never evidence.
func TestTheDigestCacheRehashesARewrittenShotAndIsNeverEvidence(t *testing.T) {
	tool, dir := cachedPass(t)
	cold, _ := stateOf(t, tool)
	var cache digestCacheFile
	if found, err := readJSON(filepath.Join(dir, ".cache", "digests.json"), &cache); !found || err != nil || cache.V != 1 || len(cache.Files) != 8 {
		t.Fatalf("cache after a cold read: %d entries, found %v: %v", len(cache.Files), found, err)
	}
	if warm, _ := stateOf(t, tool); !reflect.DeepEqual(warm, cold) {
		t.Fatalf("warm read %+v\ncold read %+v", warm, cold)
	}
	snap, err := tool.snapshot(1, nil)
	if err != nil {
		t.Fatal(err)
	}
	records, err := LoadRecords(dir)
	if err != nil {
		t.Fatal(err)
	}
	plan, _, err := tool.planRecords(1, records, nil, snap.hashes)
	if err != nil {
		t.Fatal(err)
	}
	for rel := range snap.hashes {
		if strings.Contains(rel, ".cache") {
			t.Fatalf("the basis covers %s", rel)
		}
	}
	for _, set := range plan.Sets {
		if slices.ContainsFunc(set.Files, func(f PlanFile) bool { return strings.Contains(f.Path, ".cache") }) {
			t.Fatalf("publish plans %+v", set.Files)
		}
	}
	before, err := tool.screenDigests(1, records, snap.hashes)
	if err != nil {
		t.Fatal(err)
	}

	png := filepath.Join(dir, "shots", "tasks", "phone.light.png")
	info, err := os.Stat(png)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, png, "\x01")
	if err := os.Chtimes(png, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if s, _ := stateOf(t, tool); s.ReviewBasis == cold.ReviewBasis {
		t.Fatal("a same-size rewrite with its mtime restored kept the cached digest")
	}
	snap, err = tool.snapshot(1, nil)
	if err != nil {
		t.Fatal(err)
	}
	after, err := tool.screenDigests(1, records, snap.hashes)
	if err != nil {
		t.Fatal(err)
	}
	for id := range before {
		if moved := before[id] != after[id]; moved != (id == "tasks") {
			t.Errorf("screen %s moved=%v after a retake of tasks", id, moved)
		}
	}
}

func TestACorruptDigestCacheWarnsAndIsRebuilt(t *testing.T) {
	tool, dir := cachedPass(t)
	want, _ := stateOf(t, tool)
	file := filepath.Join(dir, ".cache", "digests.json")
	for _, body := range []string{`{"v":1,"files":`, `{"v":2,"files":{}}`} {
		writeFile(t, file, body)
		s, diags := stateOf(t, tool)
		if cacheWarnings(diags) != 1 || s.ReviewBasis != want.ReviewBasis {
			t.Fatalf("cache %s: %+v", body, diags)
		}
		var cache digestCacheFile
		if _, err := readJSON(file, &cache); err != nil || cache.V != 1 || len(cache.Files) != 8 {
			t.Fatalf("cache %s was not rebuilt: %+v %v", body, cache, err)
		}
		if _, diags := stateOf(t, tool); cacheWarnings(diags) != 0 {
			t.Fatalf("a rebuilt cache still warns: %+v", diags)
		}
	}
}

// Parallel review runs read one pass at once: each writes the cache through
// its own temp file, so none reads a torn file and none fails.
func TestConcurrentReadsOfOnePassShareTheDigestCache(t *testing.T) {
	tool, dir := cachedPass(t)
	want, _ := stateOf(t, tool)
	if err := os.RemoveAll(filepath.Join(dir, ".cache")); err != nil {
		t.Fatal(err)
	}
	// A watcher decodes the cache as fast as it can while the readers write.
	stop, torn := make(chan struct{}), make(chan int)
	go func() {
		n := 0
		for {
			select {
			case <-stop:
				torn <- n
				return
			default:
			}
			var cache digestCacheFile
			if _, err := readJSON(filepath.Join(dir, ".cache", "digests.json"), &cache); err != nil {
				n++
			}
		}
	}()
	var wg sync.WaitGroup
	for i := range 8 {
		reader, err := New(tool.Root, tool.ConfigPath, tool.Raw, "1.2.3", nil)
		if err != nil {
			t.Fatal(err)
		}
		reader.Now = tool.Now
		wg.Go(func() {
			for range 3 {
				res, err := reader.State(StateOptions{})
				if err != nil {
					t.Errorf("reader %d: %v", i, err)
					return
				}
				if s := res.Data.(StateData); s.ReviewBasis != want.ReviewBasis || cacheWarnings(res.Diagnostics) != 0 {
					t.Errorf("reader %d: basis %s, %+v", i, s.ReviewBasis, res.Diagnostics)
				}
				// Each round drops the cache again, so every reader writes.
				if err := os.Remove(filepath.Join(dir, ".cache", "digests.json")); err != nil && !os.IsNotExist(err) {
					t.Errorf("reader %d: %v", i, err)
				}
			}
		})
	}
	wg.Wait()
	close(stop)
	if n := <-torn; n > 0 {
		t.Fatalf("the cache was read torn %d times", n)
	}
	if _, diags := stateOf(t, tool); cacheWarnings(diags) != 0 {
		t.Fatalf("cache left behind: %+v", diags)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, ".cache", "*.tmp")); len(left) > 0 {
		t.Fatalf("temp files left: %v", left)
	}
}
