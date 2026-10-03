package uiloop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/henderson-tech/vybava/internal/gittest"
)

func TestMain(m *testing.M) {
	gittest.NoDaemons()
	os.Exit(m.Run())
}

func evidenceRepo(t *testing.T, tool *Tool) string {
	t.Helper()
	writeFile(t, filepath.Join(tool.Root, ".gitignore"), tool.Config.Out+"/\n.vitrinka/\n")
	writeFile(t, filepath.Join(tool.Root, "app.ts"), "original\n")
	for _, args := range [][]string{{"init"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "initial"}} {
		out, err := tool.git(args...)
		if err != nil || out.Code != 0 {
			t.Fatalf("git %v: %+v %v", args, out, err)
		}
	}
	out, err := tool.git("rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out.Stdout)
}

func TestCaptureProvenanceSurvivesAnInterruptedUnpublishedPass(t *testing.T) {
	tool := newTool(t, testConfig())
	head := evidenceRepo(t, tool)
	// The marker is committed to disk before a runner can leave partial shots.
	if err := tool.captureProvenance(1, false); err != nil {
		t.Fatal(err)
	}
	before, err := tool.State(StateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s := before.Data.(StateData); s.Next.Stage != "capture" || !s.Next.Resume {
		t.Fatalf("cutoff before first shot: %+v", s)
	}
	dir := stagePass(t, tool)
	var marker captureEvidence
	if _, err := readJSON(filepath.Join(dir, "capture.json"), &marker); err != nil || marker.HeadSHA != head {
		t.Fatalf("marker %+v: %v", marker, err)
	}
	if err := tool.captureProvenance(1, true); err != nil {
		t.Fatal(err)
	}
	res, err := tool.State(StateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := res.Data.(StateData)
	if s.CapturedHeadSHA != head || !s.SourceUnchanged || s.Next.Stage != "capture" || !s.Next.Resume {
		t.Fatalf("interrupted pass %+v", s)
	}
	writeFile(t, filepath.Join(tool.Root, "app.ts"), "changed\n")
	if err := tool.captureProvenance(1, true); err == nil {
		t.Fatal("changed source resumed old evidence")
	}
	if _, err := readJSON(filepath.Join(dir, "capture.json"), &marker); err != nil || marker.HeadSHA != head {
		t.Fatal("original revision was replaced")
	}
}

// A batch is complete once every shot screen is read; an unreviewed entry is a
// capture defect for the backlog and never holds the batch open, while an
// unread shot screen still does.
func TestUnshotScreensNeverHoldABatchOpen(t *testing.T) {
	tool := newTool(t, testConfig())
	evidenceRepo(t, tool)
	if err := tool.captureProvenance(1, false); err != nil {
		t.Fatal(err)
	}
	writePass(t, tool, 1, []shot{
		{order: 1, id: "tasks", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 1},
		{order: 2, id: "task-detail", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 1},
		{order: 3, id: "task-ghost", area: "tasks", vp: "phone", theme: "light", status: "unreachable"},
	})
	basis, err := tool.reviewBasis(1)
	if err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join(tool.passAbs(1), "review/raw/tasks-1.json")
	for _, c := range []struct {
		name, basis, body string
		done              bool
	}{
		{"unshot screen unreviewed and unread", basis, `"screensRead":["tasks","task-detail"],"unreviewed":["task-ghost (unreachable: no shot)"]`, true},
		{"read shot screen unreviewed", basis, `"screensRead":["tasks","task-detail"],"unreviewed":["task-detail (recipe-failed on the dock button)"]`, true},
		{"read shot screen's shot unreviewed", basis, `"screensRead":["tasks","task-detail"],"unreviewed":["task-detail@phone.light (blank)"]`, true},
		{"unread shot screen unreviewed", basis, `"screensRead":["tasks"],"unreviewed":["task-detail (blank)","task-ghost (unreachable)"]`, false},
		{"shot screen unread", basis, `"screensRead":["tasks"],"unreviewed":["task-ghost (unreachable)"]`, false},
		{"screen read outside the batch", basis, `"screensRead":["tasks","task-detail","admin"]`, false},
		{"stale basis", "stale", `"screensRead":["tasks","task-detail"]`, false},
	} {
		writeFile(t, raw, `{"batch":"tasks-1","basis":"`+c.basis+`",`+c.body+`}`)
		ids, err := tool.rawBatchIDs(1)
		if err != nil {
			t.Fatal(err)
		}
		if ids["tasks-1"] != c.done {
			t.Errorf("%s: done=%v, want %v", c.name, ids["tasks-1"], c.done)
		}
	}
	// The completed batch still hands its unreviewed screen to the backlog,
	// also when shot entries name the screen's only ok shot.
	for _, entry := range []string{"task-detail (recipe-failed)", "task-detail@phone.light (blank)", "task-detail@phone (blank)"} {
		writeFile(t, raw, `{"batch":"tasks-1","basis":"`+basis+`","screensRead":["tasks","task-detail"],"unreviewed":["`+entry+`"]}`)
		merged, err := tool.MergeReview(MergeReviewOptions{Pass: 1})
		if err != nil {
			t.Fatal(err)
		}
		if md := merged.Data.(MergeReviewData); len(md.Left) != 0 || !slices.Contains(md.Unreviewed, "task-detail") || md.Reviewed != 1 {
			t.Errorf("merge of a complete batch with %q unreviewed: %+v", entry, md)
		}
	}
}

// v2Raw writes a v2 raw for batch: the screens it read, with the digests the
// batches verb gave them, and one finding per screen.
func v2Raw(t *testing.T, tool *Tool, pass int, file, batch string, digests map[string]string, screens ...string) {
	t.Helper()
	read, findings := map[string]string{}, []map[string]any{}
	for _, s := range screens {
		read[s] = digests[s]
		findings = append(findings, map[string]any{"screen": s, "severity": "polish", "title": "Off " + s, "acceptance": "x", "files": []string{s + ".ts"}})
	}
	body, err := json.Marshal(map[string]any{"batch": batch, "area": "tasks", "screens": read, "findings": findings})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tool.reviewDir(pass), "raw", file+".json"), string(body))
}

// A v2 raw is judged screen by screen: it completes its batch at the screens'
// current digests, and a retaken PNG reopens only its own screen — the raw's
// word on the other screen still counts.
func TestAV2RawIsJudgedScreenByScreen(t *testing.T) {
	tool := newTool(t, testConfig())
	evidenceRepo(t, tool)
	if err := tool.captureProvenance(1, false); err != nil {
		t.Fatal(err)
	}
	writePass(t, tool, 1, []shot{
		{order: 1, id: "tasks", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 1},
		{order: 2, id: "task-detail", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 1},
	})
	res, err := tool.Batches(BatchesOptions{Pass: 1})
	if err != nil {
		t.Fatal(err)
	}
	b := res.Data.(BatchesData)
	if len(b.Batches) != 1 || len(b.Batches[0].Digests) != 2 || b.Batches[0].Digests["tasks"] == b.Batches[0].Digests["task-detail"] {
		t.Fatalf("v2 batches: %+v", b)
	}
	v2Raw(t, tool, 1, "tasks-1", "tasks-1", b.Batches[0].Digests, "tasks", "task-detail")
	if done, err := tool.rawBatchIDs(1); err != nil || !done["tasks-1"] {
		t.Fatalf("a raw that read every screen at its digest: %v %v", done, err)
	}
	// A --resume retake rewrites task-detail's PNG.
	writeFile(t, filepath.Join(tool.passAbs(1), "shots", "task-detail", "phone.light.png"), "retaken")
	if done, err := tool.rawBatchIDs(1); err != nil || done["tasks-1"] {
		t.Fatalf("the retaken screen still counted: %v %v", done, err)
	}
	merged, err := tool.MergeReview(MergeReviewOptions{Pass: 1})
	if err != nil {
		t.Fatal(err)
	}
	m := merged.Data.(MergeReviewData)
	draft, err := LoadBacklog(filepath.Join(tool.reviewDir(1), "backlog.draft.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(draft.Reviewed, []string{"tasks"}) || !slices.Equal(m.Unreviewed, []string{"task-detail"}) || !slices.Equal(m.Left, []string{"tasks-1"}) ||
		len(draft.Findings) != 1 || draft.Findings[0].Screen != "tasks" {
		t.Errorf("merge after the retake: %+v, draft %+v", m, draft)
	}
}

// Two raws that each read part of a batch complete it together, whatever
// they are named: the split parts of a batch need no hand-stamped basis.
func TestPartialV2RawsCompleteTheirBatchTogether(t *testing.T) {
	tool := newTool(t, testConfig())
	evidenceRepo(t, tool)
	if err := tool.captureProvenance(1, false); err != nil {
		t.Fatal(err)
	}
	writePass(t, tool, 1, []shot{
		{order: 1, id: "tasks", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 1},
		{order: 2, id: "task-detail", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 2},
		{order: 3, id: "task-ghost", area: "tasks", vp: "phone", theme: "light", status: "unreachable"},
	})
	res, err := tool.Batches(BatchesOptions{Pass: 1})
	if err != nil {
		t.Fatal(err)
	}
	digests := res.Data.(BatchesData).Batches[0].Digests
	v2Raw(t, tool, 1, "tasks-1a", "tasks-1", digests, "tasks")
	if done, err := tool.rawBatchIDs(1); err != nil || done["tasks-1"] {
		t.Fatalf("half a batch completed it: %v %v", done, err)
	}
	v2Raw(t, tool, 1, "tasks-1b", "tasks-1b", digests, "task-detail")
	if done, err := tool.rawBatchIDs(1); err != nil || !done["tasks-1"] {
		t.Fatalf("both parts: %v %v", done, err)
	}
	merged, err := tool.MergeReview(MergeReviewOptions{Pass: 1})
	if err != nil {
		t.Fatal(err)
	}
	if m := merged.Data.(MergeReviewData); m.Reviewed != 2 || m.Findings != 2 || len(m.Left) != 0 || !slices.Equal(m.Unreviewed, []string{"task-ghost"}) {
		t.Errorf("merge of the parts: %+v", m)
	}
}

// A previous finding's verdict counts only from a screen the raw judged: shot
// entries naming every ok shot leave it unjudged, one of two shots does not.
func TestMergeReviewTakesNoVerdictFromAScreenWithEveryShotUnreviewed(t *testing.T) {
	previous := &Backlog{V: 1, Pass: 1, Findings: []Finding{{Key: "old", Screen: "task-detail", Area: "tasks", Severity: "broken", Status: "open", Title: "Old", Files: []string{"a.ts"}, Acceptance: "x"}}}
	var raw rawReview
	if err := json.Unmarshal([]byte(`{"batch":"tasks-1","basis":"b","screensRead":["tasks","task-detail"],"acceptance":[{"key":"old","verdict":"met"}],"unreviewed":["task-detail@phone.light (blank)"]}`), &raw); err != nil {
		t.Fatal(err)
	}
	batches := BatchesFile{Batches: []Batch{{ID: "tasks-1", Area: "tasks", Screens: []string{"tasks", "task-detail"}}}}
	for _, c := range []struct {
		name   string
		shots  []string
		judged bool
	}{
		{"only ok shot unreviewed", []string{"task-detail@phone.light"}, false},
		{"judged at its other shot", []string{"task-detail@phone.light", "task-detail@phone.dark"}, true},
	} {
		b, unjudged, _, unreviewed := MergeReview(2, previous, []rawReview{raw}, batches, map[string][]string{"tasks": {"tasks@phone.light"}, "task-detail": c.shots}, nil)
		if slices.Contains(b.Reviewed, "task-detail") != c.judged || slices.Contains(unreviewed, "task-detail") == c.judged ||
			(b.Findings[0].Status == "met") != c.judged || slices.Contains(unjudged, "old") == c.judged {
			t.Errorf("%s: reviewed %v, unreviewed %v, status %s, unjudged %v", c.name, b.Reviewed, unreviewed, b.Findings[0].Status, unjudged)
		}
	}
}

func TestPassEvidenceRejectsStaleReviewsCheckpointsAndScoreboard(t *testing.T) {
	tool := newTool(t, testConfig())
	head := evidenceRepo(t, tool)
	if err := tool.captureProvenance(1, false); err != nil {
		t.Fatal(err)
	}
	dir := stagePass(t, tool)
	basis, err := tool.reviewBasis(1)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "review/raw/tasks-1.json"), `{"batch":"tasks-1","basis":"`+basis+`","screensRead":["tasks","task-detail"]}`)
	raw, err := tool.rawBatchIDs(1)
	if err != nil || !raw["tasks-1"] {
		t.Fatalf("current review %v: %v", raw, err)
	}
	writeFile(t, filepath.Join(dir, "review/raw/tasks-1.json"), `{"batch":"tasks-1","basis":"`+basis+`","screensRead":["tasks"]}`)
	raw, err = tool.rawBatchIDs(1)
	if err != nil || raw["tasks-1"] {
		t.Fatal("incomplete review admitted")
	}
	writeFile(t, filepath.Join(dir, "review/raw/tasks-1.json"), `{"batch":"tasks-1","basis":"`+basis+`","screensRead":["tasks"],"unreviewed":["task-detail (blank)"],"findings":[{"screen":"tasks","area":"tasks","severity":"broken","title":"Partial finding","acceptance":"fixed","files":["app.ts"]}]}`)
	merged, err := tool.MergeReview(MergeReviewOptions{Pass: 1})
	if err != nil {
		t.Fatal(err)
	}
	md := merged.Data.(MergeReviewData)
	if md.Findings != 1 || md.Reviewed != 1 || !slices.Contains(md.Left, "tasks-1") || !slices.Contains(md.Unreviewed, "task-detail") {
		t.Fatalf("partial review lost: %+v", md)
	}
	cp := Checkpoint{Basis: basis, Key: "fix", Status: "done", Commit: head, FileDigests: map[string]string{"app.ts": digest("original\n", 64)}, APIChanges: []string{"additive API"}}
	if err := writeJSON(filepath.Join(dir, "fix/fix.json"), cp); err != nil {
		t.Fatal(err)
	}
	cps, _, err := tool.loadCheckpoints(1)
	if err != nil || len(cps) != 1 {
		t.Fatalf("checkpoint %v: %v", cps, err)
	}
	writeFile(t, filepath.Join(dir, "publish/boards.json"), `[{"area":"tasks","url":"board"},{"area":"admin","url":"board2"}]`)
	writeFile(t, filepath.Join(dir, "review/backlog.json"), `{"v":1,"pass":1,"reviewed":[],"findings":[]}`)
	writeFile(t, filepath.Join(dir, "review/basis.json"), `{"basis":"`+basis+`"}`)
	state := func() StateData {
		t.Helper()
		res, err := tool.State(StateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return res.Data.(StateData)
	}
	writeFile(t, filepath.Join(dir, "scoreboard.json"), "{}")
	writeFile(t, filepath.Join(dir, "scoreboard.md"), "score")
	s := state()
	if s.ScoreboardCurrent {
		t.Fatal("synthesis alone proved scoreboard completion")
	}
	writeFile(t, filepath.Join(dir, "review/scoreboard-receipt.json"), `{"basis":"`+s.ScoreboardBasis+`","posted":[{"area":"tasks","url":"board"},{"area":"admin","url":"board2"}]}`)
	if !state().ScoreboardCurrent {
		t.Fatal("matching scoreboard receipt not admitted")
	}
	writeFile(t, filepath.Join(dir, "review/backlog.json"), `{"v":1,"pass":1,"reviewed":["tasks"],"findings":[]}`)
	if state().ScoreboardCurrent {
		t.Fatal("stale scoreboard admitted")
	}
	writeFile(t, filepath.Join(dir, "fix/recovery.json"), `{"lane":"prim-1","kind":"primitives","dirs":["."],"keys":["fix"]}`)
	if s := state(); s.Next.Stage != "fix" || s.Recovery == nil || s.Checkpoints.Total != 1 {
		t.Fatalf("recovery lost behind completed checkpoints: %+v", s)
	}
	writeFile(t, filepath.Join(tool.Root, "app.ts"), "reverted\n")
	cps, _, err = tool.loadCheckpoints(1)
	if err != nil || len(cps) != 0 {
		t.Fatal("reverted source kept a checkpoint")
	}
	writeFile(t, filepath.Join(dir, "shots/tasks/phone.light.png"), "new pixels")
	if state().HasBacklog {
		t.Fatal("new pixels kept the cached backlog")
	}
	for _, file := range []string{"", filepath.Join(dir, "review/backlog.json")} {
		if _, err := tool.Scoreboard(ScoreboardOptions{Pass: 1, Backlog: file}); diagCode(err) != DiagBacklogInvalid {
			t.Fatalf("stale scoreboard %q: %v", file, err)
		}
	}
}

// A pass's evidence is immutable: with provenance the manifest and spec parts
// of its basis are read at the captured revision, so a rig repair or a spec
// edit between review and verify keeps the reviewed pass current. Shots still
// move it; a legacy pass still follows the tree.
func TestReviewBasisReadsTheManifestAtTheCapturedRevision(t *testing.T) {
	untouched := map[bool]string{}
	for _, strict := range []bool{true, false} {
		cfg := testConfig()
		cfg.Spec = "docs/ui-spec.md"
		tool := newTool(t, cfg)
		writeFile(t, filepath.Join(tool.Root, "tests/ui-loop/screens/tasks.ts"), "recipe v1\n")
		writeFile(t, filepath.Join(tool.Root, "tests/ui-loop/vendor/capture.spec.ts"), "harness\n")
		writeFile(t, filepath.Join(tool.Root, cfg.Spec), "rules v1\n")
		head := evidenceRepo(t, tool)
		if strict {
			if err := tool.captureProvenance(1, false); err != nil {
				t.Fatal(err)
			}
		}
		dir := stagePass(t, tool)
		basis := func() string {
			t.Helper()
			b, err := tool.reviewBasis(1)
			if err != nil {
				t.Fatal(err)
			}
			return b
		}
		captured := basis()
		untouched[strict] = captured
		writeFile(t, filepath.Join(dir, "review/backlog.json"), `{"v":1,"pass":1,"reviewed":["tasks","admin"],"findings":[]}`)
		writeFile(t, filepath.Join(dir, "review/basis.json"), `{"basis":"`+captured+`"}`)
		if err := writeJSON(filepath.Join(dir, "fix/skip.json"), Checkpoint{Basis: captured, Key: "skip", Status: "skipped", Commit: head}); err != nil {
			t.Fatal(err)
		}

		writeFile(t, filepath.Join(tool.Root, "tests/ui-loop/screens/tasks.ts"), "recipe v2 (repaired)\n")
		writeFile(t, filepath.Join(tool.Root, "tests/ui-loop/screens/added.ts"), "new recipe\n")
		for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "rig repair"}} {
			if out, err := tool.git(args...); err != nil || out.Code != 0 {
				t.Fatalf("git %v: %+v %v", args, out, err)
			}
		}
		res, err := tool.State(StateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		s := res.Data.(StateData)
		// A legacy pass never checks its backlog's basis, so only a strict one can lose it.
		if repaired := basis(); (repaired == captured) != strict || s.ReviewBasis != repaired || (strict && !s.HasBacklog) {
			t.Fatalf("strict=%v: rig repair moved the basis %v (state %s, hasBacklog %v)", strict, repaired != captured, s.ReviewBasis, s.HasBacklog)
		}
		if !strict {
			continue
		}
		// A skip judges the app, so the rig repair keeps it; an app edit does not.
		if s.Checkpoints.Total != 1 || len(res.Diagnostics) != 0 {
			t.Fatalf("rig repair dropped the skip: %+v %v", s.Checkpoints, res.Diagnostics)
		}
		writeFile(t, filepath.Join(tool.Root, "app.ts"), "changed\n")
		if cps, _, err := tool.loadCheckpoints(1); err != nil || len(cps) != 0 {
			t.Fatalf("an app edit kept the skip: %v %v", cps, err)
		}
		writeFile(t, filepath.Join(dir, "shots/tasks/phone.light.png"), "new pixels")
		shot := basis()
		if shot == captured {
			t.Fatal("a shot change kept the basis")
		}
		writeFile(t, filepath.Join(tool.Root, cfg.Spec), "rules v2\n")
		if basis() != shot {
			t.Fatal("a spec edit after capture moved the basis")
		}
	}
	// An untouched tree hashes alike either way (vendor/ skipped in both), so a
	// receipt written before this rule stays current.
	if untouched[true] != untouched[false] {
		t.Fatalf("committed manifest basis %s, working-tree basis %s", untouched[true], untouched[false])
	}
}

// Spec, docs and rig commits never stale a pass: state lists them as drift
// (the spec by rule) and keeps sourceUnchanged and the basis; only an app
// change stales it.
func TestStateStalesAPassOnlyOnAppDrift(t *testing.T) {
	cfg := testConfig()
	cfg.Spec = "docs/ui-spec.md"
	tool := newTool(t, cfg)
	spec := filepath.Join(tool.Root, cfg.Spec)
	writeFile(t, spec, "# UI spec\n\n## Density\n\n- PWF-D01 Spacing sits on the 4 px grid.\n  Half steps only inside primitives.\n- PWF-D02 Rows are 40 px.\n\n## Tone\n\nCalm, no exclamation marks.\n")
	writeFile(t, filepath.Join(tool.Root, "tests/ui-loop/screens/tasks.ts"), "recipe v1\n")
	evidenceRepo(t, tool)
	if err := tool.captureProvenance(1, false); err != nil {
		t.Fatal(err)
	}
	stagePass(t, tool)
	captured, err := tool.reviewBasis(1)
	if err != nil {
		t.Fatal(err)
	}
	commit := func(msg string) {
		t.Helper()
		for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", msg}} {
			if out, err := tool.git(args...); err != nil || out.Code != 0 {
				t.Fatalf("git %v: %+v %v", args, out, err)
			}
		}
	}
	state := func() StateData {
		t.Helper()
		res, err := tool.State(StateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return res.Data.(StateData)
	}
	writeFile(t, spec, "# UI spec\n\n## Density\n\n- PWF-D01 Spacing sits on the 4 px grid.\n  Half steps nowhere.\n- PWF-D02 Rows are 44 px (see PWF-B04).\n\n## Tone\n\nCalm.\n")
	writeFile(t, filepath.Join(tool.Root, "docs/notes.md"), "a decision\n")
	writeFile(t, filepath.Join(tool.Root, "tests/ui-loop/screens/tasks.ts"), "recipe v2\n")
	commit("spec, docs and rig")
	s := state()
	if s.Drift == nil || !s.SourceUnchanged || s.ReviewBasis != captured || len(s.Drift.App) != 0 ||
		!slices.Equal(s.Drift.Rig, []string{"tests/ui-loop/screens/tasks.ts"}) || !slices.Equal(s.Drift.Spec, []string{"PWF-B04", "PWF-D01", "PWF-D02", "Tone"}) {
		t.Fatalf("spec, docs and rig staled the pass: unchanged %v, basis moved %v, drift %+v", s.SourceUnchanged, s.ReviewBasis != captured, s.Drift)
	}
	writeFile(t, filepath.Join(tool.Root, "apps/portal/tasks.ts"), "fixed\n")
	commit("app")
	if s = state(); s.SourceUnchanged || !slices.Equal(s.Drift.App, []string{"apps/portal/tasks.ts"}) || s.Drift.AppTotal != 1 {
		t.Fatalf("an app commit kept the pass: unchanged %v, drift %+v", s.SourceUnchanged, s.Drift)
	}
}

// A captured revision this clone lacks cannot pin the manifest: the basis falls
// back to the working tree, and state says so instead of quietly reporting the
// pass unreviewed after the next rig change. Its drift cannot be weighed
// either (null), so the pass is shot again rather than routed as clean.
func TestStateWarnsWhenTheCapturedRevisionIsMissing(t *testing.T) {
	tool := newTool(t, testConfig())
	writeFile(t, filepath.Join(tool.Root, "tests/ui-loop/screens/tasks.ts"), "recipe v1\n")
	evidenceRepo(t, tool)
	if err := tool.captureProvenance(1, false); err != nil {
		t.Fatal(err)
	}
	dir := stagePass(t, tool)
	pinned, err := tool.reviewBasis(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(dir, "capture.json"), captureEvidence{HeadSHA: strings.Repeat("ab", 20)}); err != nil {
		t.Fatal(err)
	}
	res, err := tool.State(StateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	warned := slices.ContainsFunc(res.Diagnostics, func(d runxDiagnostic) bool {
		return d.Code == DiagCaptureRevisionMissing && d.Severity == "warning"
	})
	if s := res.Data.(StateData); s.ReviewBasis != pinned || !warned {
		t.Fatalf("missing revision: basis moved %v, diagnostics %v", s.ReviewBasis != pinned, res.Diagnostics)
	}
	if s := res.Data.(StateData); s.Drift != nil || s.SourceUnchanged || s.Next.Stage != "capture" || s.Next.Resume || s.Next.Only == nil || len(s.Next.Only) != 0 ||
		!strings.Contains(s.Next.Reason, DiagCaptureRevisionMissing) {
		t.Fatalf("missing revision routed as weighed: drift %+v, unchanged %v, next %+v", s.Drift, s.SourceUnchanged, s.Next)
	}
}

// A config root below the repo's top judges the whole repo: capture refuses
// an uncommitted change in the library beside it, and state lists that
// change, tracked or untracked, with its ../.
func TestAConfigRootBelowTheTopJudgesTheWholeRepo(t *testing.T) {
	top := t.TempDir()
	root := filepath.Join(top, "web")
	tool, err := New(root, filepath.Join(root, "vybava.config.json"), testConfig(), "1.2.3", nil)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, ".gitignore"), ".ui-loop/\n.vitrinka/\n")
	writeFile(t, filepath.Join(top, "shared/lib.ts"), "original\n")
	for _, args := range [][]string{{"init"}, {"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "initial"}} {
		if out, err := tool.git(append([]string{"-C", top}, args...)...); err != nil || out.Code != 0 {
			t.Fatalf("git %v: %+v %v", args, out, err)
		}
	}
	if err := tool.captureProvenance(1, false); err != nil {
		t.Fatal(err)
	}
	stagePass(t, tool)
	writeFile(t, filepath.Join(top, "shared/lib.ts"), "dirty\n")
	writeFile(t, filepath.Join(top, "shared/new.ts"), "untracked\n")
	if err := tool.captureProvenance(2, false); diagCode(err) != DiagSelectionInvalid {
		t.Fatalf("capture beside an uncommitted library change: %v", err)
	}
	res, err := tool.State(StateOptions{Pass: 1})
	if err != nil {
		t.Fatal(err)
	}
	if s := res.Data.(StateData); s.Drift == nil || s.SourceUnchanged || !slices.Equal(s.Drift.App, []string{"../shared/lib.ts", "../shared/new.ts"}) {
		t.Fatalf("the library beside the root: unchanged %v, drift %+v", s.SourceUnchanged, s.Drift)
	}
}

// uiLoop.source decides what stales a pass and uiLoop.primitives is state's
// default, both relayed as the effective values; --primitives overrides it.
// An unpublished pass whose tree changed cannot resume, so it is shot again.
func TestStateWeighsDriftByTheConfiguredSourceAndPrimitives(t *testing.T) {
	cfg := testConfig()
	cfg.Source, cfg.Primitives = []string{"apps"}, []string{"apps/ui"}
	tool := newTool(t, cfg)
	evidenceRepo(t, tool)
	if err := tool.captureProvenance(1, false); err != nil {
		t.Fatal(err)
	}
	stagePass(t, tool)
	writeFile(t, filepath.Join(tool.Root, "apps/portal/tasks.ts"), "fixed\n")
	writeFile(t, filepath.Join(tool.Root, "app.ts"), "outside the source\n")
	state := func(o StateOptions) StateData {
		t.Helper()
		res, err := tool.State(o)
		if err != nil {
			t.Fatal(err)
		}
		return res.Data.(StateData)
	}
	s := state(StateOptions{})
	if !slices.Equal(s.Config.Source, []string{"apps"}) || !slices.Equal(s.Config.Primitives, []string{"apps/ui"}) ||
		s.SourceUnchanged || !slices.Equal(s.Drift.App, []string{"apps/portal/tasks.ts"}) || s.Drift.AppTotal != 1 {
		t.Fatalf("config %+v, unchanged %v, drift %+v", s.Config, s.SourceUnchanged, s.Drift)
	}
	if s.Next.Stage != "capture" || s.Next.Resume || s.Next.Only == nil || len(s.Next.Only) != 0 {
		t.Errorf("an unpublished pass whose tree changed: %+v", s.Next)
	}
	if s = state(StateOptions{Primitives: []string{"libs/ui-lib"}}); !slices.Equal(s.Config.Primitives, []string{"libs/ui-lib"}) {
		t.Errorf("--primitives did not override uiLoop.primitives: %v", s.Config.Primitives)
	}
}

// gitAt runs git in tool's repo as a test identity, committing at
// 1e9+at seconds when at > 0, and returns its trimmed stdout.
func gitAt(t *testing.T, tool *Tool, at int, args ...string) string {
	t.Helper()
	var env []string
	if at > 0 {
		date := fmt.Sprintf("@%d +0000", 1_000_000_000+at)
		env = []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
	}
	out, err := RealExec(context.Background(), Cmd{Dir: tool.Root, Env: env, Args: append([]string{"git", "-c", "user.name=Test", "-c", "user.email=test@example.test"}, args...)})
	if err != nil || out.Code != 0 {
		t.Fatalf("git %v: %+v %v", args, out, err)
	}
	return strings.TrimSpace(out.Stdout)
}

func commitFile(t *testing.T, tool *Tool, at int, file, body string) string {
	t.Helper()
	writeFile(t, filepath.Join(tool.Root, file), body)
	gitAt(t, tool, 0, "add", "-A")
	gitAt(t, tool, at, "commit", "-m", file)
	return gitAt(t, tool, 0, "rev-parse", "HEAD")
}

// factsPerCommit is checkpointFacts for an open checkpoint at each of
// commits, failing where it differs from merge-base and sourceUnchanged asked
// per commit.
func factsPerCommit(t *testing.T, tool *Tool, name string, commits []string) commitFacts {
	t.Helper()
	var cps []Checkpoint
	for _, c := range commits {
		cps = append(cps, Checkpoint{Basis: "b", Key: c, Status: "blocked", Commit: c})
	}
	facts, err := tool.checkpointFacts("b", cps)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range commits {
		mb, err := tool.git("merge-base", "--is-ancestor", c, "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		unchanged := false
		if mb.Code == 0 {
			if unchanged, err = tool.sourceUnchanged(c, tool.Config.Dir); err != nil {
				t.Fatal(err)
			}
		}
		if facts.ancestor[c] != (mb.Code == 0) || facts.unchanged[c] != unchanged {
			t.Errorf("%s: %s ancestor %v unchanged %v; git per commit: ancestor %v unchanged %v", name, c[:7], facts.ancestor[c], facts.unchanged[c], mb.Code == 0, unchanged)
		}
	}
	return facts
}

// checkpointFacts asks git about every checkpoint commit at once; each answer
// must be merge-base's and sourceUnchanged's for that commit alone, also for
// a working tree that undoes the commits since one (equal change sets).
func TestCheckpointFactsAnswerAsGitDoesPerCommit(t *testing.T) {
	tool := newTool(t, testConfig())
	first := evidenceRepo(t, tool)
	second := commitFile(t, tool, 0, "app.ts", "changed\n")
	rig := commitFile(t, tool, 0, "tests/ui-loop/screens/tasks.ts", "recipe v1\n")
	side := gitAt(t, tool, 0, "commit-tree", rig+"^{tree}", "-m", "off HEAD's line")
	commits := []string{first, second, rig, side, strings.Repeat("ab", 20)}
	for _, step := range []struct {
		name, file, body string
		unchanged        []string
	}{
		{"a clean tree (a rig commit is ignored)", "", "", []string{second, rig}},
		{"an uncommitted app edit", "app.ts", "dirty\n", nil},
		{"a tree that undoes the commits since the first", "app.ts", "original\n", []string{first}},
		{"an untracked rig file", "tests/ui-loop/screens/admin.ts", "recipe\n", []string{first}},
		{"an untracked app file", "new.ts", "app\n", nil},
	} {
		if step.file != "" {
			writeFile(t, filepath.Join(tool.Root, step.file), step.body)
		}
		facts := factsPerCommit(t, tool, step.name, commits)
		for _, c := range commits {
			if facts.unchanged[c] != slices.Contains(step.unchanged, c) {
				t.Errorf("%s: %s unchanged %v", step.name, c[:7], facts.unchanged[c])
			}
		}
	}
}

// Histories where asking git once could answer otherwise than merge-base and
// sourceUnchanged per commit: rev-list stops walking early past skewed commit
// dates, plumbing diff-tree ignores diff.ignoreSubmodules, and rev-list fails
// outright on an unreadable object off HEAD's line.
func TestCheckpointFactsHoldOnAwkwardHistories(t *testing.T) {
	t.Run("skewed commit dates behind a merge", func(t *testing.T) {
		tool := newTool(t, testConfig())
		evidenceRepo(t, tool)
		base := gitAt(t, tool, 0, "rev-parse", "HEAD")
		fixed := commitFile(t, tool, 5000, "app.ts", "fixed\n")
		for i := range 10 {
			commitFile(t, tool, 1001+i, "tests/ui-loop/screens/tasks.ts", fmt.Sprintf("recipe %d\n", i))
		}
		side := gitAt(t, tool, 3000, "commit-tree", base+"^{tree}", "-p", base, "-m", "side")
		gitAt(t, tool, 6000, "merge", "--no-ff", "-m", "merge", side)
		if facts := factsPerCommit(t, tool, "skewed", []string{fixed}); !facts.ancestor[fixed] || !facts.unchanged[fixed] {
			t.Fatalf("lost the checkpoint commit: %+v", facts)
		}
	})
	t.Run("a gitlink bump under diff.ignoreSubmodules=all", func(t *testing.T) {
		tool := newTool(t, testConfig())
		evidenceRepo(t, tool)
		if err := os.Mkdir(filepath.Join(tool.Root, "lib"), 0o755); err != nil {
			t.Fatal(err)
		}
		var commits []string
		for i, sha := range []string{strings.Repeat("1", 40), strings.Repeat("2", 40)} {
			gitAt(t, tool, 0, "update-index", "--add", "--cacheinfo", "160000,"+sha+",lib")
			gitAt(t, tool, 0, "commit", "-m", fmt.Sprintf("lib %d", i))
			commits = append(commits, gitAt(t, tool, 0, "rev-parse", "HEAD"))
		}
		gitAt(t, tool, 0, "config", "diff.ignoreSubmodules", "all")
		if facts := factsPerCommit(t, tool, "gitlink", commits); !facts.unchanged[commits[0]] {
			t.Fatalf("a hidden gitlink bump staled the checkpoint: %+v", facts)
		}
	})
	t.Run("an unreadable object off HEAD's line", func(t *testing.T) {
		tool := newTool(t, testConfig())
		first := evidenceRepo(t, tool)
		second := commitFile(t, tool, 0, "app.ts", "changed\n")
		x1 := gitAt(t, tool, 0, "commit-tree", first+"^{tree}", "-p", first, "-m", "x1")
		x2 := gitAt(t, tool, 0, "commit-tree", first+"^{tree}", "-p", x1, "-m", "x2")
		if err := os.Remove(filepath.Join(tool.Root, ".git", "objects", x1[:2], x1[2:])); err != nil {
			t.Fatal(err)
		}
		if facts := factsPerCommit(t, tool, "unreadable", []string{first, second, x2}); !facts.ancestor[first] || !facts.ancestor[second] {
			t.Fatalf("one broken commit hid the others: %+v", facts)
		}
	})
}
