package uiloop

import (
	"encoding/json"
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

// A previous finding's verdict counts only from a screen the raw judged: shot
// entries naming every ok shot leave it unjudged, one of two shots does not.
func TestMergeReviewTakesNoVerdictFromAScreenWithEveryShotUnreviewed(t *testing.T) {
	previous := &Backlog{V: 1, Pass: 1, Findings: []Finding{{Key: "old", Screen: "task-detail", Area: "tasks", Severity: "broken", Status: "open", Title: "Old", Files: []string{"a.ts"}, Acceptance: "x"}}}
	var raw rawReview
	if err := json.Unmarshal([]byte(`{"batch":"tasks-1","basis":"b","screensRead":["tasks","task-detail"],"acceptance":[{"key":"old","verdict":"met"}],"unreviewed":["task-detail@phone.light (blank)"]}`), &raw); err != nil {
		t.Fatal(err)
	}
	batches := []Batch{{ID: "tasks-1", Area: "tasks", Screens: []string{"tasks", "task-detail"}}}
	for _, c := range []struct {
		name   string
		shots  []string
		judged bool
	}{
		{"only ok shot unreviewed", []string{"task-detail@phone.light"}, false},
		{"judged at its other shot", []string{"task-detail@phone.light", "task-detail@phone.dark"}, true},
	} {
		b, unjudged, _, unreviewed := MergeReview(2, previous, []rawReview{raw}, batches, map[string][]string{"tasks": {"tasks@phone.light"}, "task-detail": c.shots})
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
