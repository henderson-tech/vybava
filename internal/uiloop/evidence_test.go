package uiloop

import (
	"os"
	"path/filepath"
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
	writeFile(t, filepath.Join(tool.Root, "app.ts"), "reverted\n")
	cps, _, err = tool.loadCheckpoints(1)
	if err != nil || len(cps) != 0 {
		t.Fatal("reverted source kept a checkpoint")
	}
	writeFile(t, filepath.Join(dir, "shots/tasks/phone.light.png"), "new pixels")
	if state().HasBacklog {
		t.Fatal("new pixels kept the cached backlog")
	}
}
