package uiloop

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, file, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// stagePass is two areas, three screens: tasks (tasks, task-detail) and admin (users).
func stagePass(t *testing.T, tool *Tool) string {
	t.Helper()
	writePass(t, tool, 1, []shot{
		{order: 0, id: "users", area: "admin", vp: "desktop", theme: "dark", status: "ok", bytes: 1},
		{order: 1, id: "tasks", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 1},
		{order: 1, id: "tasks", area: "tasks", vp: "phone", theme: "dark", status: "ok", bytes: 1},
		{order: 2, id: "task-detail", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 1},
	})
	return tool.passAbs(1)
}

func TestStateCountsAPassAndNamesTheNextStage(t *testing.T) {
	tool := newTool(t, testConfig())
	dir := stagePass(t, tool)
	state := func() StateData {
		t.Helper()
		res, err := tool.State(StateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return res.Data.(StateData)
	}
	s := state()
	if s.Pass != 1 || s.Shots != 4 || s.Screens != 3 || s.Published || !slices.Equal(s.Unpublished, []string{"tasks", "admin"}) || s.Next.Stage != "capture" || !s.Next.Resume {
		t.Fatalf("unpublished pass: %+v", s)
	}
	if !slices.Equal(s.Areas, []AreaCount{{"tasks", 2}, {"admin", 1}}) || s.Review.Planned != 2 || s.Review.BatchesFile {
		t.Errorf("areas %+v, review %+v", s.Areas, s.Review)
	}
	// A legacy chunk row never counts; the area sets do.
	writeFile(t, filepath.Join(dir, "publish", "index.json"), `{"pass":1,"sets":[
		{"key":"ui-polish-tasks","area":"tasks","status":"pushed","url":"u1"},
		{"key":"ui-polish-admin","status":"skipped"}],"legacy":[{"key":"old","status":"failed"}]}`)
	writeFile(t, filepath.Join(dir, "review", "raw", "tasks-1.json"), `{"batch":"tasks-1","area":"tasks","findings":[],"acceptance":[]}`)
	if s = state(); !s.Published || s.Next.Stage != "review" || !slices.Equal(s.Review.Left, []string{"admin-1"}) || !slices.Equal(s.Review.ReviewedAreas, []string{"tasks"}) {
		t.Fatalf("half reviewed: %+v", s)
	}
	// One review run per 4 left batches, at most 3, and the one that synthesizes.
	if s.Next.Parallel != 1 || reviewParallel(0) != 1 || reviewParallel(5) != 2 || reviewParallel(40) != 3 {
		t.Errorf("parallel: %d, %d, %d, %d", s.Next.Parallel, reviewParallel(0), reviewParallel(5), reviewParallel(40))
	}
	writeFile(t, filepath.Join(dir, "review", "raw", "admin-1.json"), `{"batch":"admin-1","area":"admin"}`)
	writeFile(t, filepath.Join(dir, "review", "backlog.json"), `{"v":1,"pass":1,"reviewed":["tasks","users"],"findings":[
		{"key":"a","screen":"users","area":"admin","severity":"broken","status":"open","title":"t","files":["x.ts"],"acceptance":"x"},
		{"key":"b","screen":"tasks","area":"tasks","severity":"polish","status":"open","title":"t","files":["y.ts"],"acceptance":"x"}]}`)
	writeFile(t, filepath.Join(dir, "fix", "a.json"), `{"v":1,"key":"a","status":"done"}`)
	writeFile(t, filepath.Join(dir, "fix", "lanes.json"), `{"v":1,"primitives":[]}`)
	s = state()
	if !s.HasBacklog || s.Backlog.Open != 2 || s.Backlog.BySeverity["broken"] != 1 || s.Backlog.Reviewed != 2 || s.Checkpoints.Total != 1 {
		t.Fatalf("backlog %+v, checkpoints %+v", s.Backlog, s.Checkpoints)
	}
	if !reflect.DeepEqual(s.Next, NextStage{Stage: "fix", Resume: true, Reason: "1 of 2 open items without a checkpoint"}) {
		t.Errorf("next: %+v", s.Next)
	}
	writeFile(t, filepath.Join(dir, "fix", "b.json"), `{"v":1,"key":"b","status":"skipped","note":"spec"}`)
	if s = state(); s.Next.Stage != "verify" {
		t.Errorf("checkpointed round: %+v", s.Next)
	}
}

// Only app drift moves a pass: before the review it reshoots everything,
// while fixing it is expected, after the round (or on a converged pass) the
// verify reshoots the open, the fixed and the drift-touched screens, and a
// changed primitive reshoots everything.
func TestNextStageRoutesAppDriftAndSelectsTheVerify(t *testing.T) {
	records := []Record{
		{ID: "users", Area: "admin", SourceFiles: []string{"apps/portal/users.ts"}},
		{ID: "profile", Area: "admin", SourceFiles: []string{"libs/ui-lib/avatar.ts"}},
		{ID: "tasks", Area: "tasks", Viewport: "phone", SourceFiles: []string{"apps/portal/tasks"}},
		{ID: "tasks", Area: "tasks", Viewport: "desktop", SourceFiles: []string{"apps/portal/tasks"}},
		{ID: "task-detail", Area: "tasks", SourceFiles: []string{"apps/portal/task-detail.ts"}},
	}
	areas := []string{"tasks", "admin"}
	open := []Finding{{Key: "a", Screen: "users", Status: "open"}, {Key: "b", Screen: "profile", Status: "met"}}
	met := open[1:]
	round := []Checkpoint{{Key: "a", Status: "done", Screens: []string{"users", "task-detail"}}}
	for _, c := range []struct {
		name              string
		reviewed          []string
		findings          []Finding
		checkpoints       []Checkpoint
		drift, primitives []string
		stage, reason     string
		only              []string
	}{
		{"an app commit after a checkpointed round", areas, open, round, []string{"apps/portal/tasks/list.ts"}, nil, "verify", "checkpointed", []string{"task-detail", "tasks", "users"}},
		{"an app change under a primitive", areas, open, round, []string{"apps/portal/tasks/list.ts", "libs/ui-lib/button.ts"}, []string{"libs/ui-lib"}, "verify", "primitive changed → full reshoot: libs/ui-lib/button.ts", []string{}},
		{"an app change before the review finished", []string{"tasks"}, nil, nil, []string{"apps/portal/users.ts"}, nil, "capture", "apps/portal/users.ts", []string{}},
		{"an app change while fixing", areas, open, nil, []string{"apps/portal/users.ts"}, nil, "fix", "", nil},
		{"a converged pass whose app drifted", areas, met, nil, []string{"libs/ui-lib/avatar.ts"}, nil, "verify", "converged", []string{"profile"}},
		{"a converged pass", areas, met, nil, nil, nil, "done", "converged", nil},
	} {
		n := nextStage(2, records, true, areas, c.reviewed, c.findings != nil, c.findings, c.checkpoints, c.drift, "", c.primitives, 6)
		if n.Stage != c.stage || !strings.Contains(n.Reason, c.reason) || n.Resume || (n.Only == nil) != (c.only == nil) || !slices.Equal(n.Only, c.only) {
			t.Errorf("%s: %+v", c.name, n)
		}
	}
}

// A pass whose drift cannot be weighed (no provenance, or a revision this
// clone lacks) may have drifted: it is reshot in full wherever app drift
// would reshoot it, an unpublished one included, and only a fix round in
// progress keeps fixing.
func TestNextStageReshootsAPassWhoseDriftCannotBeWeighed(t *testing.T) {
	records := []Record{{ID: "users", Area: "admin"}, {ID: "tasks", Area: "tasks"}}
	areas := []string{"tasks", "admin"}
	open := []Finding{{Key: "a", Screen: "users", Status: "open"}}
	met := []Finding{{Key: "a", Screen: "users", Status: "met"}}
	missing := "was captured at " + strings.Repeat("ab", 20) + ", which this clone lacks (CAPTURE_REVISION_MISSING)"
	for _, c := range []struct {
		name, unjudged string
		published      bool
		reviewed       []string
		findings       []Finding
		stage, reason  string
		only           []string
	}{
		{"an unpublished pass without provenance", "has no capture provenance", false, nil, nil, "capture", "not published, and it has no capture provenance: reshoot", []string{}},
		{"a pass without provenance before the review", "has no capture provenance", true, []string{"tasks"}, nil, "capture", "pass 2 has no capture provenance: reshoot before the review", []string{}},
		{"a pass without provenance while fixing", "has no capture provenance", true, areas, open, "fix", "1 of 1 open items", nil},
		{"a converged pass whose revision is missing", missing, true, areas, met, "verify", "CAPTURE_REVISION_MISSING) → full reshoot", []string{}},
	} {
		n := nextStage(2, records, c.published, areas, c.reviewed, c.findings != nil, c.findings, nil, nil, c.unjudged, nil, 6)
		if n.Stage != c.stage || !strings.Contains(n.Reason, c.reason) || n.Resume || (n.Only == nil) != (c.only == nil) || !slices.Equal(n.Only, c.only) {
			t.Errorf("%s: %+v", c.name, n)
		}
	}
}

// The workflow gates on contract and quotes the lint the shots were taken
// with, so both travel even before the first pass, defaults filled; a pass
// reports its run.json's lint, not a config changed since.
func TestStateCarriesTheContractAndTheEffectiveLint(t *testing.T) {
	tool := newTool(t, testConfig())
	lint := func() StateLint {
		t.Helper()
		res, err := tool.State(StateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		s := res.Data.(StateData)
		if s.Contract != StateContract || s.Vybava != "1.2.3" {
			t.Errorf("contract %d, vybava %q", s.Contract, s.Vybava)
		}
		return s.Config.Lint
	}
	if l := lint(); l != (StateLint{Grid: 4, TouchTarget: 44}) {
		t.Errorf("no pass: the config's lint, defaults filled, got %+v", l)
	}
	stagePass(t, tool)
	writeFile(t, filepath.Join(tool.passAbs(1), "run.json"), `{"lint":{"grid":4,"touchTarget":40}}`)
	if l := lint(); l != (StateLint{Grid: 4, TouchTarget: 40}) {
		t.Errorf("pass 1 was linted at 40, got %+v", l)
	}
}

// While a capture runs every stage waits; a newer pass without shots is
// the capture starting, or pending once none runs, and state routes the
// newest pass with shots either way.
func TestStateWaitsForARunningCaptureAndRoutesTheNewestShotPass(t *testing.T) {
	tool := newTool(t, testConfig())
	stagePass(t, tool)
	writeFile(t, filepath.Join(tool.passAbs(2), "run.json"), `{"v":1,"pass":2}`)
	tool.Now = func() time.Time { return time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC) }
	lease, held, err := tool.acquireLease(2, leaseCapture, leaseReq{owner: "ui-loop run", pid: os.Getpid(), ttl: captureLeaseTTL, run: "2026-10-03T09:00:00Z"})
	if err != nil || held != nil {
		t.Fatal(held, err)
	}
	state := func(pass int) StateData {
		t.Helper()
		res, err := tool.State(StateOptions{Pass: pass})
		if err != nil {
			t.Fatal(err)
		}
		return res.Data.(StateData)
	}
	s := state(0)
	if s.Pass != 1 || s.Pending != nil || s.Capture != (CaptureState{Running: true, Pass: 2, Since: "2026-10-03T09:00:00Z", Owner: "ui-loop run"}) ||
		!reflect.DeepEqual(s.Next, NextStage{Stage: "wait", Reason: "pass 2 capture running since 2026-10-03T09:00:00Z (ui-loop run)"}) {
		t.Fatalf("a running capture: pass %d, pending %v, capture %+v, next %+v", s.Pass, s.Pending, s.Capture, s.Next)
	}
	if err := tool.releaseLease(2, leaseCapture, lease); err != nil {
		t.Fatal(err)
	}
	if s = state(0); s.Pass != 1 || s.Pending == nil || *s.Pending != 2 || s.Capture.Running || s.Next.Stage != "capture" || !s.Next.Resume {
		t.Errorf("a pending pass 2: pass %d, pending %v, capture %+v, next %+v", s.Pass, s.Pending, s.Capture, s.Next)
	}
	if s = state(2); s.Pass != 2 || s.Pending != nil {
		t.Errorf("an explicit --pass is read as asked: pass %d, pending %v", s.Pass, s.Pending)
	}
}

// checkpointStatus is loadCheckpoints of pass 1 as key → status, plus its warnings.
func checkpointStatus(t *testing.T, tool *Tool) (map[string]string, []runxDiagnostic) {
	t.Helper()
	cps, diags, err := tool.loadCheckpoints(1)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, c := range cps {
		if _, dup := got[c.Key]; dup {
			t.Fatalf("key %q counted twice: %+v", c.Key, cps)
		}
		got[c.Key] = c.Status
	}
	return got, diags
}

func TestASlashKeyCheckpointFinishesItsItem(t *testing.T) {
	tool := newTool(t, testConfig())
	dir := stagePass(t, tool)
	writeFile(t, filepath.Join(dir, "publish", "index.json"), `{"pass":1,"sets":[
		{"key":"ui-polish-tasks","area":"tasks","status":"pushed","url":"u1"},{"key":"ui-polish-admin","area":"admin","status":"pushed","url":"u2"}]}`)
	writeFile(t, filepath.Join(dir, "review", "raw", "tasks-1.json"), `{"batch":"tasks-1","area":"tasks"}`)
	writeFile(t, filepath.Join(dir, "review", "raw", "admin-1.json"), `{"batch":"admin-1","area":"admin"}`)
	writeFile(t, filepath.Join(dir, "review", "backlog.json"), `{"v":1,"pass":1,"findings":[
		{"key":"shell-6/topbar","screen":"tasks","area":"tasks","severity":"broken","status":"open","title":"t","files":["x.ts"],"acceptance":"x"}]}`)
	// The writer names the file after the key, so "/" makes a subdirectory.
	writeFile(t, filepath.Join(dir, "fix", "shell-6", "topbar.json"), `{"v":1,"key":"shell-6/topbar","status":"done"}`)
	res, err := tool.State(StateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s := res.Data.(StateData); s.Checkpoints.Total != 1 || s.Next.Stage != "verify" {
		t.Fatalf("slash-key checkpoint unread: %+v, next %+v", s.Checkpoints, s.Next)
	}
}

func TestAnArchivedRoundNeverCountsAndOneKeyCountsOnce(t *testing.T) {
	tool := newTool(t, testConfig())
	fix := filepath.Join(stagePass(t, tool), "fix")
	writeFile(t, filepath.Join(fix, "a.json"), `{"v":1,"key":"a","status":"done"}`)
	writeFile(t, filepath.Join(fix, "shell-6", "topbar.json"), `{"v":1,"key":"shell-6/topbar","status":"done"}`)
	// Round 1's originals, moved aside when round 2 rewrote them.
	writeFile(t, filepath.Join(fix, "r1", "a.json"), `{"v":1,"key":"a","status":"blocked"}`)
	writeFile(t, filepath.Join(fix, "r1", "shell-6", "topbar.json"), `{"v":1,"key":"shell-6/topbar","status":"blocked"}`)
	writeFile(t, filepath.Join(fix, "r1", "gone.json"), `{"v":1,"key":"gone","status":"done"}`)
	got, diags := checkpointStatus(t, tool)
	if len(got) != 2 || got["a"] != "done" || got["shell-6/topbar"] != "done" || len(diags) != 0 {
		t.Fatalf("archive counted: %v %+v", got, diags)
	}
	// Two live files claiming one key: the writers' path fix/<key>.json
	// counts over a newer stray, once, and the stray is the one warned.
	writeFile(t, filepath.Join(fix, "shell-6--topbar.json"), `{"v":1,"key":"shell-6/topbar","status":"skipped"}`)
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(fix, "shell-6", "topbar.json"), old, old); err != nil {
		t.Fatal(err)
	}
	got, diags = checkpointStatus(t, tool)
	if len(got) != 2 || got["shell-6/topbar"] != "done" || len(diags) != 1 || diags[0].Code != DiagCheckpointInvalid ||
		!strings.Contains(diags[0].Detail, "fix/shell-6--topbar.json and fix/shell-6/topbar.json") || !strings.Contains(diags[0].Detail, "fix/shell-6/topbar.json counts") {
		t.Fatalf("duplicate key: %v %+v", got, diags)
	}
}

func TestAStaleCheckpointNeverShadowsACurrentOne(t *testing.T) {
	tool := newTool(t, testConfig())
	head := evidenceRepo(t, tool)
	if err := tool.captureProvenance(1, false); err != nil {
		t.Fatal(err)
	}
	fix := filepath.Join(stagePass(t, tool), "fix")
	basis, err := tool.reviewBasis(1)
	if err != nil {
		t.Fatal(err)
	}
	current := Checkpoint{Basis: basis, Key: "shell-6/topbar", Status: "done", Commit: head, FileDigests: map[string]string{"app.ts": digest("original\n", 64)}}
	if err := writeJSON(filepath.Join(fix, "shell-6--topbar.json"), current); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(fix, "shell-6--topbar.json"), old, old); err != nil {
		t.Fatal(err)
	}
	// Newer and at the writers' path, but from another review basis.
	stale := current
	stale.Basis = "an earlier review"
	if err := writeJSON(filepath.Join(fix, "shell-6", "topbar.json"), stale); err != nil {
		t.Fatal(err)
	}
	got, diags := checkpointStatus(t, tool)
	if len(got) != 1 || got["shell-6/topbar"] != "done" || len(diags) != 0 {
		t.Fatalf("stale file shadowed the current checkpoint: %v %+v", got, diags)
	}
}

func TestCheckpointsHandsOnWhatStateCounts(t *testing.T) {
	tool := newTool(t, testConfig())
	fix := filepath.Join(stagePass(t, tool), "fix")
	writeFile(t, filepath.Join(fix, "shell-6", "nav.json"), `{"v":1,"key":"shell-6/nav","status":"done","screens":["tasks","nav"],"i18n":[{"key":"NAV.X","cs":"x"}]}`)
	writeFile(t, filepath.Join(fix, "r1", "gone.json"), `{"v":1,"key":"gone","status":"done","screens":["old"]}`)
	writeFile(t, filepath.Join(fix, lanesFile), `{"v":1,"primitives":[],"areas":[]}`)
	res, err := tool.Checkpoints(CheckpointsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	d := res.Data.(CheckpointsData)
	if d.Pass != 1 || len(d.Checkpoints) != 1 || len(res.Diagnostics) != 0 {
		t.Fatalf("checkpoints: %+v %+v", d, res.Diagnostics)
	}
	if c := d.Checkpoints[0]; c.Key != "shell-6/nav" || !slices.Equal(c.Screens, []string{"tasks", "nav"}) || string(c.I18n) != `[{"key":"NAV.X","cs":"x"}]` {
		t.Fatalf("checkpoint not handed on verbatim: %+v (i18n %s)", c, c.I18n)
	}
}

func TestACheckpointWithoutAKeyIsItsPath(t *testing.T) {
	tool := newTool(t, testConfig())
	fix := filepath.Join(stagePass(t, tool), "fix")
	writeFile(t, filepath.Join(fix, "shell-6", "nav.json"), `{"v":1,"status":"blocked"}`)
	writeFile(t, filepath.Join(fix, "b.json"), `{"v":1,"status":"done"}`)
	writeFile(t, filepath.Join(fix, "empty.json"), `{"v":1}`)
	got, diags := checkpointStatus(t, tool)
	if len(got) != 2 || got["shell-6/nav"] != "blocked" || got["b"] != "done" || len(diags) != 1 || !strings.Contains(diags[0].Detail, "fix/empty.json") {
		t.Fatalf("path fallback: %v %+v", got, diags)
	}
}

func TestBatchesPersistAndRefuseToRedefineAStartedReview(t *testing.T) {
	tool := newTool(t, testConfig())
	dir := stagePass(t, tool)
	res, err := tool.Batches(BatchesOptions{Size: 1})
	if err != nil {
		t.Fatal(err)
	}
	b := res.Data.(BatchesData)
	want := []Batch{{ID: "tasks-1", Area: "tasks", Screens: []string{"task-detail"}}, {ID: "tasks-2", Area: "tasks", Screens: []string{"tasks"}}, {ID: "admin-1", Area: "admin", Screens: []string{"users"}}}
	if b.Size != 1 || b.Screens != 3 || len(b.Batches) != 3 || !slices.Equal(b.Batches[1].Screens, want[1].Screens) || b.Batches[2].ID != "admin-1" {
		t.Fatalf("batches: %+v", b)
	}
	var file BatchesFile
	if _, err := readJSON(filepath.Join(dir, "review", "batches.json"), &file); err != nil || len(file.Batches) != 3 {
		t.Fatalf("batches.json: %+v %v", file, err)
	}
	writeFile(t, filepath.Join(dir, "review", "raw", "tasks-1.json"), `{"batch":"tasks-1","area":"tasks"}`)
	// No --size keeps the persisted one; --areas narrows the answer, never the file.
	res, err = tool.Batches(BatchesOptions{Areas: []string{"tasks"}})
	if err != nil {
		t.Fatal(err)
	}
	if b = res.Data.(BatchesData); b.Size != 1 || len(b.Batches) != 2 || !slices.Equal(b.Done, []string{"tasks-1"}) || !slices.Equal(b.Left, []string{"tasks-2"}) {
		t.Errorf("narrowed: %+v", b)
	}
	if _, err := tool.Batches(BatchesOptions{Size: 14}); diagCode(err) != DiagSelectionInvalid {
		t.Errorf("resizing a started review: %v", err)
	}
}

func TestMergeReviewCarriesVerdictsAndTakesReviewedFromBatches(t *testing.T) {
	tool := newTool(t, testConfig())
	writePass(t, tool, 1, []shot{{order: 0, id: "tasks", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 1}})
	writeFile(t, filepath.Join(tool.passAbs(1), "review", "backlog.json"), `{"v":1,"pass":1,"reviewed":["tasks"],"findings":[
		{"key":"old-a","screen":"tasks","area":"tasks","severity":"broken","status":"open","title":"A","files":["a.ts"],"acceptance":"x"},
		{"key":"old-b","screen":"task-detail","area":"tasks","severity":"polish","status":"open","title":"B","files":["b.ts"],"acceptance":"x"},
		{"key":"old-c","screen":"tasks","area":"tasks","severity":"polish","status":"met","title":"C","files":[],"acceptance":"x"}]}`)
	writePass(t, tool, 2, []shot{
		{order: 1, id: "tasks", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 1},
		{order: 2, id: "task-detail", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 1},
		{order: 0, id: "users", area: "admin", vp: "desktop", theme: "dark", status: "ok", bytes: 1},
	})
	raw := filepath.Join(tool.passAbs(2), "review", "raw")
	// Two viewports' reviewers file one defect twice; one verdict per batch, the worst wins.
	writeFile(t, filepath.Join(raw, "tasks-1.json"), `{"batch":"tasks-1","area":"tasks",
		"acceptance":[{"key":"old-a","verdict":"met"}],
		"findings":[
			{"screen":"tasks","severity":"polish","title":"Header Clips!","acceptance":"fits","files":["h.ts"],"viewports":["phone"],"systemic":true},
			{"screen":"tasks","severity":"needs-work","title":"header clips","acceptance":"fits","files":["h.ts","i.ts"],"viewports":["tablet"]},
			{"key":"old-a","screen":"tasks","severity":"broken","title":"A again","acceptance":"x","files":["a.ts"]},
			{"screen":"task-detail","severity":"polish","title":"no files","acceptance":"x"}],
		"unreviewed":["task-detail (recipe-failed: dialog never opened)","tasks@tablet (blank)"]}`)
	writeFile(t, filepath.Join(raw, "tasks-2.json"), `{"batch":"tasks-2","area":"tasks","acceptance":[{"key":"old-a","verdict":"partly"}]}`)
	if _, err := tool.Batches(BatchesOptions{Pass: 2, Size: 1}); err != nil {
		t.Fatal(err)
	}
	res, err := tool.MergeReview(MergeReviewOptions{Pass: 2})
	if err != nil {
		t.Fatal(err)
	}
	m := res.Data.(MergeReviewData)
	if m.Previous == nil || m.Previous.Pass != 1 || !slices.Equal(m.Unjudged, []string{"old-b"}) || !slices.Equal(m.Left, []string{"admin-1"}) || len(res.Diagnostics) != 1 {
		t.Fatalf("merge: %+v %+v", m, res.Diagnostics)
	}
	if len(m.Problems) != 1 || !slices.Equal(m.Problems[0].Missing, []string{"files"}) || !slices.Equal(m.Unreviewed, []string{"task-detail"}) || len(m.Invalid) != 0 {
		t.Errorf("problems %+v, unreviewed %v, invalid %v", m.Problems, m.Unreviewed, m.Invalid)
	}
	draft, err := LoadBacklog(filepath.Join(tool.passAbs(2), "review", "backlog.draft.json"))
	if err != nil {
		t.Fatal(err)
	}
	// tasks-1 is task-detail (skipped), tasks-2 is tasks: only tasks was judged.
	if !slices.Equal(draft.Reviewed, []string{"tasks"}) || len(draft.Findings) != 3 {
		t.Fatalf("draft: %+v", draft)
	}
	byKey := map[string]Finding{}
	for _, f := range draft.Findings {
		byKey[f.Key] = f
	}
	if byKey["old-a"].Status != "partly" || byKey["old-b"].Status != "not-met" {
		t.Errorf("verdicts: %+v %+v", byKey["old-a"], byKey["old-b"])
	}
	h := byKey["tasks-header-clips"]
	if h.Severity != "needs-work" || h.Area != "tasks" || !slices.Equal(h.Files, []string{"h.ts", "i.ts"}) || !slices.Equal(h.Viewports, []string{"phone", "tablet"}) {
		t.Errorf("folded fresh finding: %+v", h)
	}
	if body, _ := os.ReadFile(filepath.Join(tool.passAbs(2), "review", "backlog.draft.json")); strings.Contains(string(body), "systemic") {
		t.Errorf("a reviewer-only key leaked into the draft: %s", body)
	}
}

func TestLanesOwnDirectoriesAndPackPrimitivesFirst(t *testing.T) {
	root := "/repo"
	f := func(key, area, sev, status string, files ...string) Finding {
		return Finding{Key: key, Screen: key, Area: area, Severity: sev, Status: status, Title: key, Acceptance: "x", Files: files}
	}
	findings := []Finding{
		// libs/ui/button is a primitive by prefix; apps/x/shared by two areas sharing it.
		f("p1", "tasks", "polish", "open", "libs/ui/button/button.ts"),
		f("p2", "admin", "broken", "open", "apps/i18n/en.json", "libs/ui/button/button.html"),
		f("s1", "tasks", "polish", "open", "apps/x/shared/a.ts"),
		f("s2", "admin", "polish", "not-met", "/repo/apps/x/shared/b.ts"),
		f("t1", "tasks", "polish", "open", "apps/x/tasks/list.ts"),
		f("t2", "tasks", "polish", "not-met", "apps/x/tasks/detail.ts"),
		f("t3", "tasks", "broken", "open", "apps/x/tasks/sub/row.ts"),
		f("u1", "admin", "needs-work", "open", "apps/x/users/users.ts"),
		f("done", "admin", "polish", "open", "apps/x/users/users.ts"),
		f("met", "admin", "polish", "met", "apps/x/roles/r.ts"),
		f("far", "admin", "polish", "open", "/elsewhere/other-repo/x.ts", "../sibling/y.ts"),
		f("cat", "admin", "polish", "open", "apps/portal/src/i18n/cs.json"),
	}
	l := PlanLanes(root, findings, map[string]bool{"done": true}, []string{"libs/ui/"}, 2)
	if !slices.Equal(l.Frozen, []string{"apps/x/shared", "libs/ui/button"}) || !slices.Equal(l.Foreign, []string{"far"}) || !slices.Equal(l.I18n, []string{"cat"}) {
		t.Fatalf("frozen %v, foreign %v, i18n %v", l.Frozen, l.Foreign, l.I18n)
	}
	if l.Open != 11 || l.Finished != 1 || len(l.Primitives) != 2 || len(l.Areas) != 2 {
		t.Fatalf("lanes: %+v", l)
	}
	// Biggest group first onto the lightest lane: 2-item groups tie, dir order breaks it.
	if l.Primitives[0].Lane != "prim-1" || !slices.Equal(l.Primitives[0].Dirs, []string{"apps/x/shared"}) || !slices.Equal(l.Primitives[0].Keys, []string{"s2", "s1"}) {
		t.Errorf("prim-1 (carried before fresh): %+v", l.Primitives[0])
	}
	if !slices.Equal(l.Primitives[1].Keys, []string{"p2", "p1"}) {
		t.Errorf("prim-2 (worst first): %+v", l.Primitives[1])
	}
	// A lane owns a dir, not its subdirectories: apps/x/tasks/sub is its own group.
	if !slices.Equal(l.Areas[0].Dirs, []string{"apps/x/tasks"}) || !slices.Equal(l.Areas[0].Keys, []string{"t2", "t1"}) ||
		!slices.Equal(l.Areas[1].Dirs, []string{"apps/x/tasks/sub", "apps/x/users"}) || !slices.Equal(l.Areas[1].Keys, []string{"t3", "u1"}) {
		t.Errorf("areas: %+v", l.Areas)
	}
}

// Without --primitives, lanes freezes uiLoop.primitives.
func TestLanesDefaultToTheConfiguredPrimitives(t *testing.T) {
	cfg := testConfig()
	cfg.Primitives = []string{"libs/ui"}
	tool := newTool(t, cfg)
	dir := stagePass(t, tool)
	writeFile(t, filepath.Join(dir, "review", "backlog.json"), `{"v":1,"pass":1,"findings":[
		{"key":"a","screen":"tasks","area":"tasks","severity":"polish","status":"open","title":"t","files":["libs/ui/button/button.ts"],"acceptance":"x"}]}`)
	res, err := tool.Lanes(LanesOptions{Pass: 1})
	if err != nil {
		t.Fatal(err)
	}
	if l := res.Data.(LanesData); !slices.Equal(l.Frozen, []string{"libs/ui/button"}) || len(l.Primitives) != 1 {
		t.Fatalf("lanes without --primitives: frozen %v, primitives %+v", l.Frozen, l.Primitives)
	}
}

// UILOOP_REAL_BACKLOG points at a real pass's backlog.json (pwf-ui pass 1,
// 340 items); the test reads a copy and checks the lanes stay balanced.
func TestLanesBalanceARealBacklog(t *testing.T) {
	src := os.Getenv("UILOOP_REAL_BACKLOG")
	if src == "" {
		t.Skip("set UILOOP_REAL_BACKLOG to a real review/backlog.json")
	}
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "backlog.json")
	writeFile(t, file, string(body))
	b, err := LoadBacklog(file)
	if err != nil {
		t.Fatal(err)
	}
	l := PlanLanes(filepath.Dir(src), b.Findings, nil, []string{"libs/ui-lib/", "libs/tailwind-preset/", "libs/formio-templates/", "libs/shared-components/", "libs/framework/", "libs/formio/", "libs/widgets/"}, 4)
	for _, phase := range [][]Lane{l.Primitives, l.Areas} {
		lo, hi := len(phase[0].Keys), len(phase[0].Keys)
		for _, lane := range phase {
			lo, hi = min(lo, len(lane.Keys)), max(hi, len(lane.Keys))
		}
		if len(phase) != 4 || hi-lo > 2 {
			t.Errorf("unbalanced phase: %d lanes, %d..%d items", len(phase), lo, hi)
		}
	}
	t.Logf("primitives %d×, areas %d×, frozen %d dirs, foreign %d, i18n %d", len(l.Primitives), len(l.Areas), len(l.Frozen), len(l.Foreign), len(l.I18n))
}
