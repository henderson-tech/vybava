package uiloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// shot is a record fixture: a shot with a viewport capture of `bytes` bytes
// and, when full > 0, a full companion.
type shot struct {
	order               int
	id, area, vp, theme string
	status              string
	bytes, full         int
	defects             map[string]int
	distinct            map[string][]LintKey
}

func writePass(t *testing.T, tool *Tool, pass int, shots []shot) {
	t.Helper()
	dir := tool.passAbs(pass)
	for _, s := range shots {
		sd := filepath.Join(dir, "shots", s.id)
		if err := os.MkdirAll(sd, 0o755); err != nil {
			t.Fatal(err)
		}
		base := s.vp + "." + s.theme
		rec := map[string]any{
			"v": 1, "pass": pass, "order": s.order, "id": s.id, "app": "portal", "area": s.area, "kind": "page",
			"title": strings.ToUpper(s.id[:1]) + s.id[1:], "route": "#/" + s.id, "url": "http://127.0.0.1:5500/portal/#/" + s.id,
			"viewport": s.vp, "theme": s.theme, "status": s.status, "size": map[string]int{"width": 390, "height": 844},
			"files": map[string]any{"viewport": nil, "full": nil}, "consoleErrors": []string{}, "sourceFiles": []string{"apps/portal/" + s.id + ".ts"},
			"lint": map[string]any{"defects": s.defects, "info": map[string]int{}},
		}
		if s.distinct != nil {
			rec["lint"].(map[string]any)["distinct"] = s.distinct
		}
		files := rec["files"].(map[string]any)
		if s.bytes > 0 {
			files["viewport"] = base + ".png"
			if err := os.WriteFile(filepath.Join(sd, base+".png"), make([]byte, s.bytes), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if s.full > 0 {
			files["full"] = base + ".full.png"
			if err := os.WriteFile(filepath.Join(sd, base+".full.png"), make([]byte, s.full), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		b, _ := json.Marshal(rec)
		if err := os.WriteFile(filepath.Join(sd, base+".json"), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSplitIsDeterministicAndHonoursBothLimits(t *testing.T) {
	cfg := testConfig()
	cfg.Publish = Publish{MaxFiles: 3, MaxBytes: 1000}
	tool := newTool(t, cfg)
	var shots []shot
	// admin before tasks in the manifest; the config's area order must still win.
	shots = append(shots, shot{order: 0, id: "users", area: "admin", vp: "desktop", theme: "dark", status: "ok", bytes: 100})
	for i := 1; i <= 5; i++ {
		// The third shot's pair would overflow 3 files: it opens chunk 2 whole.
		full := 0
		if i == 3 {
			full = 100
		}
		shots = append(shots, shot{order: i, id: fmt.Sprintf("task-%d", i), area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 100, full: full})
	}
	shots = append(shots,
		shot{order: 6, id: "big-a", area: "tasks", vp: "desktop", theme: "light", status: "ok", bytes: 600},
		shot{order: 7, id: "big-b", area: "tasks", vp: "desktop", theme: "light", status: "ok", bytes: 600},
		shot{order: 8, id: "dark-only", area: "tasks", vp: "phone", theme: "dark", status: "ok", bytes: 10},
		shot{order: 9, id: "gone", area: "tasks", vp: "phone", theme: "light", status: "unreachable"},
	)
	writePass(t, tool, 1, shots)

	res, err := tool.Split(SplitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	plan := res.Data.(Plan)
	var got []string
	for _, s := range plan.Sets {
		var files []string
		for _, f := range s.Files {
			files = append(files, filepath.Base(filepath.Dir(f.Path))+"/"+filepath.Base(f.Path))
		}
		got = append(got, fmt.Sprintf("%s %d/%d %s", s.Key, s.Chunk, s.Chunks, strings.Join(files, ",")))
	}
	want := []string{
		"ui-polish-p1-tasks-phone-light-1 1/3 task-1/phone.light.png,task-2/phone.light.png",
		"ui-polish-p1-tasks-phone-light-2 2/3 task-3/phone.light.png,task-3/phone.light.full.png,task-4/phone.light.png",
		"ui-polish-p1-tasks-phone-light-3 3/3 task-5/phone.light.png",
		"ui-polish-p1-tasks-phone-dark-1 1/1 dark-only/phone.dark.png",
		"ui-polish-p1-tasks-desktop-light-1 1/2 big-a/desktop.light.png",
		"ui-polish-p1-tasks-desktop-light-2 2/2 big-b/desktop.light.png",
		"ui-polish-p1-admin-desktop-dark-1 1/1 users/desktop.dark.png",
	}
	if !slices.Equal(got, want) {
		t.Errorf("plan:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, s := range plan.Sets {
		if len(s.Files) > 3 || s.Bytes > 1000 {
			t.Errorf("%s breaks a limit: %d files, %d bytes", s.Key, len(s.Files), s.Bytes)
		}
	}
	if !slices.Equal(plan.Skipped, []string{"gone@phone.light (unreachable)"}) {
		t.Errorf("skipped: %v", plan.Skipped)
	}
	full := plan.Sets[1].Files[1]
	if full.Label != "P1-TASK-3-PHONE-LIGHT-FULL" || full.Route != "/portal/#/task-3" || full.Viewport != "390x844@2" || !strings.HasPrefix(full.Note, "full content · pass 1 · clean") {
		t.Errorf("full companion: %+v", full)
	}

	again, err := tool.Split(SplitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(plan)
	b, _ := json.Marshal(again.Data.(Plan))
	if string(a) != string(b) {
		t.Error("split is not deterministic")
	}

	if k := fit(strings.Repeat("area-", 20), "-12", maxKey); len(k) > maxKey || !strings.HasSuffix(k, "-12") || k == fit(strings.Repeat("area-", 19)+"x", "-12", maxKey) {
		t.Errorf("fit must cap, keep the suffix and stay unique: %q", k)
	}
}

func TestPublishRetriesThenHalves(t *testing.T) {
	cfg := testConfig()
	tool := newTool(t, cfg)
	writePass(t, tool, 1, []shot{
		{order: 0, id: "a", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10},
		{order: 1, id: "b", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10},
		{order: 2, id: "c", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10},
	})
	tool.LookPath = func(string) (string, error) { return "/bin/vitrinka", nil }
	tool.Sleep = func(time.Duration) {}
	pushes := map[string]int{}
	tailDown := true // the tail half fails on the first run, then recovers
	var captures []string
	tool.Exec = func(_ context.Context, c Cmd) (CmdOut, error) {
		args := c.Args[1:]
		root := args[slices.Index(args, "--root")+1]
		switch strings.Join(args[:2], " ") {
		case "board init":
			return CmdOut{}, os.WriteFile(filepath.Join(root, ".vitrinka"), []byte(`{}`), 0o644)
		case "board capture":
			if _, err := os.Stat(filepath.Join(root, ".vitrinka")); err == nil {
				t.Error("capture ran with the descriptor in place: it would fire a push per shot")
			}
			captures = append(captures, filepath.Base(root)+" "+args[slices.Index(args, "--label")+1])
			return CmdOut{}, nil
		case "board push":
			key := filepath.Base(root)
			pushes[key]++
			if key == "ui-polish-p1-tasks-phone-light-1" || (key == "ui-polish-p1-tasks-phone-light-1b" && tailDown) {
				return CmdOut{Code: 1, Stderr: "push timed out after 30s"}, nil
			}
			return CmdOut{Stdout: `{"v":1,"ok":true,"data":{"url":"https://app.vitrinka.ai/w/fixit/boards/` + key + `","files":2}}`}, nil
		}
		t.Fatalf("unexpected command %v", c.Args)
		return CmdOut{}, nil
	}
	res, err := tool.Publish(context.Background(), PublishOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sets := res.Data.(map[string]any)["sets"].([]PublishedSet)
	var got []string
	for _, s := range sets {
		got = append(got, fmt.Sprintf("%s %s %d %v", s.Key, s.Status, s.Files, s.HalvedInto))
	}
	want := []string{
		"ui-polish-p1-tasks-phone-light-1 halved 3 [ui-polish-p1-tasks-phone-light-1a ui-polish-p1-tasks-phone-light-1b]",
		"ui-polish-p1-tasks-phone-light-1a pushed 2 []",
		"ui-polish-p1-tasks-phone-light-1b failed 1 []",
	}
	if !slices.Equal(got, want) {
		t.Errorf("publish:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if pushes["ui-polish-p1-tasks-phone-light-1"] != 3 || pushes["ui-polish-p1-tasks-phone-light-1b"] != 3 {
		t.Errorf("3 tries per set, a half is never halved again: %v", pushes)
	}
	if len(res.Diagnostics) != 1 || !strings.Contains(res.Diagnostics[0].Fix, "--sets ui-polish-p1-tasks-phone-light-1b") {
		t.Errorf("the failed half is retried by its own key: %+v", res.Diagnostics)
	}
	if sets[1].URL != "https://app.vitrinka.ai/w/fixit/boards/ui-polish-p1-tasks-phone-light-1a" {
		t.Errorf("url from the --json envelope: %q", sets[1].URL)
	}
	if len(captures) != 6 || captures[5] != "ui-polish-p1-tasks-phone-light-1b P1-C-PHONE-LIGHT" {
		t.Errorf("captures: %v", captures)
	}

	// Retrying by the half's key pushes that half only; the pushed head is left alone.
	tailDown = false
	before := len(captures)
	res, err = tool.Publish(context.Background(), PublishOptions{Sets: []string{"ui-polish-p1-tasks-phone-light-1b"}})
	if err != nil {
		t.Fatal(err)
	}
	got = got[:0]
	for _, s := range res.Data.(map[string]any)["sets"].([]PublishedSet) {
		got = append(got, s.Key+" "+s.Status)
	}
	if !slices.Equal(got, []string{"ui-polish-p1-tasks-phone-light-1 halved", "ui-polish-p1-tasks-phone-light-1b pushed"}) || len(res.Diagnostics) != 0 {
		t.Errorf("retry: %v %+v", got, res.Diagnostics)
	}
	if len(captures) != before {
		t.Errorf("the tail was already adopted; only its push is retried: %v", captures[before:])
	}
	// --force re-pushes the named half, never the whole parent.
	res, _ = tool.Publish(context.Background(), PublishOptions{Sets: []string{"ui-polish-p1-tasks-phone-light-1b"}, Force: true})
	got = got[:0]
	for _, s := range res.Data.(map[string]any)["sets"].([]PublishedSet) {
		got = append(got, fmt.Sprintf("%s %s %d", s.Key, s.Status, s.Files))
	}
	if !slices.Equal(got, []string{"ui-polish-p1-tasks-phone-light-1 halved 3", "ui-polish-p1-tasks-phone-light-1b pushed 1"}) {
		t.Errorf("forced half retry: %v", got)
	}
	// A plain re-run finds both halves pushed and does nothing.
	res, _ = tool.Publish(context.Background(), PublishOptions{})
	for _, s := range res.Data.(map[string]any)["sets"].([]PublishedSet) {
		if s.Status != "halved" && s.Status != "skipped" {
			t.Errorf("re-run touched %s (%s)", s.Key, s.Status)
		}
	}
	// Recapture the tail with different pixels: preserve the two-board layout,
	// refreshing only the changed half rather than reviving the failed parent.
	headPushes := pushes["ui-polish-p1-tasks-phone-light-1a"]
	parentPushes := pushes["ui-polish-p1-tasks-phone-light-1"]
	if err := os.WriteFile(filepath.Join(tool.passAbs(1), "shots", "c", "phone.light.png"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = tool.Publish(context.Background(), PublishOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pushes["ui-polish-p1-tasks-phone-light-1a"] != headPushes || pushes["ui-polish-p1-tasks-phone-light-1"] != parentPushes {
		t.Fatalf("recapture touched the unchanged head or revived its parent: %v", pushes)
	}
	if len(res.Diagnostics) != 0 {
		t.Fatalf("tail recapture failed: %+v", res.Diagnostics)
	}
}

func TestPublishResumesAcknowledgedSetsAfterCancellation(t *testing.T) {
	cfg := testConfig()
	cfg.Publish = Publish{MaxFiles: 1, MaxBytes: 1000}
	tool := newTool(t, cfg)
	writePass(t, tool, 1, []shot{
		{order: 0, id: "a", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10},
		{order: 1, id: "b", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	captures, pushes := 0, map[string]int{}
	tool.LookPath = func(string) (string, error) { return "/bin/vitrinka", nil }
	tool.Exec = func(_ context.Context, c Cmd) (CmdOut, error) {
		args := c.Args[1:]
		root := args[slices.Index(args, "--root")+1]
		switch strings.Join(args[:2], " ") {
		case "board init":
			return CmdOut{}, os.WriteFile(filepath.Join(root, descriptor), []byte(`{}`), 0o644)
		case "board capture":
			captures++
		case "board push":
			key := filepath.Base(root)
			pushes[key]++
			if len(pushes) == 1 {
				cancel()
			}
			return CmdOut{Stdout: `{"ok":true,"data":{"url":"https://app.vitrinka.ai/w/fixit/boards/` + key + `"}}`}, nil
		default:
			t.Fatalf("unexpected command %v", args)
		}
		return CmdOut{}, nil
	}
	if _, err := tool.Publish(ctx, PublishOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cutoff, got %v", err)
	}
	body, err := os.ReadFile(filepath.Join(tool.passAbs(1), "publish", "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	var index PublishIndex
	if err := json.Unmarshal(body, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Sets) != 1 || index.Sets[0].Status != "pushed" || index.Sets[0].URL == "" {
		t.Fatalf("acknowledgement was not saved before cutoff: %+v", index)
	}
	for range 2 {
		if _, err := tool.Publish(context.Background(), PublishOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if captures != 2 || len(pushes) != 2 {
		t.Fatalf("resume duplicated capture/upload: captures=%d pushes=%v", captures, pushes)
	}
	for key, count := range pushes {
		if count != 1 {
			t.Errorf("%s uploaded %d times", key, count)
		}
	}
}

func TestPublishInvalidatesReceiptWhenImageContentsChange(t *testing.T) {
	tool := newTool(t, testConfig())
	writePass(t, tool, 1, []shot{{order: 0, id: "a", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10}})
	tool.LookPath = func(string) (string, error) { return "/bin/vitrinka", nil }
	captures, pushes := 0, 0
	tool.Exec = func(_ context.Context, c Cmd) (CmdOut, error) {
		args := c.Args[1:]
		root := args[slices.Index(args, "--root")+1]
		switch strings.Join(args[:2], " ") {
		case "board init":
			return CmdOut{}, os.WriteFile(filepath.Join(root, descriptor), []byte(`{}`), 0o644)
		case "board capture":
			captures++
		case "board push":
			pushes++
			return CmdOut{Stdout: `{"ok":true,"data":{"url":"https://app.vitrinka.ai/w/fixit/boards/a"}}`}, nil
		}
		return CmdOut{}, nil
	}
	if _, err := tool.Publish(context.Background(), PublishOptions{}); err != nil {
		t.Fatal(err)
	}
	// Same filename and size, different pixels: both receipt and adoption ledger must refresh.
	file := filepath.Join(tool.passAbs(1), "shots", "a", "phone.light.png")
	if err := os.WriteFile(file, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := tool.Publish(context.Background(), PublishOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if captures != 2 || pushes != 2 {
		t.Fatalf("changed contents were not refreshed exactly once: %d captures, %d pushes", captures, pushes)
	}
}

func TestPublishDoesNotAcceptAnErrorEnvelopeAsAcknowledged(t *testing.T) {
	tool := newTool(t, testConfig())
	writePass(t, tool, 1, []shot{{order: 0, id: "a", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10}})
	tool.LookPath = func(string) (string, error) { return "/bin/vitrinka", nil }
	tool.Exec = func(_ context.Context, c Cmd) (CmdOut, error) {
		args := c.Args[1:]
		root := args[slices.Index(args, "--root")+1]
		if args[1] == "init" {
			return CmdOut{}, os.WriteFile(filepath.Join(root, descriptor), []byte(`{}`), 0o644)
		}
		return CmdOut{Stdout: `{"ok":false,"data":{"url":"https://app.vitrinka.ai/w/fixit/boards/a"}}`}, nil
	}
	res, err := tool.Publish(context.Background(), PublishOptions{Retries: 1})
	if err != nil {
		t.Fatal(err)
	}
	sets := res.Data.(map[string]any)["sets"].([]PublishedSet)
	if len(sets) != 1 || sets[0].Status != "failed" || len(res.Diagnostics) != 1 {
		t.Fatalf("error response became a successful receipt: %+v", res)
	}
}

func TestScoreboardMathAndDelta(t *testing.T) {
	tool := newTool(t, testConfig())
	writePass(t, tool, 1, []shot{
		{order: 0, id: "tasks", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 1, defects: map[string]int{"grid": 5, "contrast": 2}},
		{order: 1, id: "task-detail", area: "tasks", vp: "phone", theme: "light", status: "recipe-failed", bytes: 1},
		{order: 2, id: "users", area: "admin", vp: "desktop", theme: "dark", status: "ok", bytes: 1, defects: map[string]int{"grid": 1}},
	})
	backlog := `{"v":1,"pass":1,"findings":[
		{"key":"k1","screen":"tasks","area":"tasks","severity":"polish","status":"open","title":"t","files":["a.ts"],"acceptance":"x"},
		{"key":"k2","screen":"tasks","area":"tasks","severity":"broken","status":"open","title":"t","files":["a.ts"],"acceptance":"x"},
		{"key":"k3","screen":"task-detail","area":"tasks","severity":"needs-work","status":"met","title":"t","files":[],"acceptance":"x"},
		{"key":"k4","screen":"users","area":"admin","severity":"needs-work","status":"partly","title":"t","files":["u.ts"],"acceptance":"x"}]}`
	review := filepath.Join(tool.passAbs(1), "review")
	if err := os.MkdirAll(review, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(review, "backlog.json"), []byte(backlog), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Scoreboard(ScoreboardOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sb := res.Data.(Scoreboard)
	row := func(sb Scoreboard, area string) AreaScore {
		for _, a := range sb.Areas {
			if a.Area == area {
				return a
			}
		}
		t.Fatalf("no area %s", area)
		return AreaScore{}
	}
	tasks := row(sb, "tasks")
	if sb.Areas[0].Area != "tasks" || tasks.Screens != 2 || tasks.Broken != 1 || tasks.Clean != 1 || tasks.Polish != 0 ||
		tasks.Open != 2 || tasks.Met != 1 || tasks.LintDefects != 7 || tasks.NotOk != 1 || tasks.Lint["grid"] != 5 {
		t.Errorf("tasks row: %+v", tasks)
	}
	if admin := row(sb, "admin"); admin.NeedsWork != 1 || admin.Partly != 1 || admin.Clean != 0 {
		t.Errorf("admin row: %+v", admin)
	}
	if sb.Totals.Screens != 3 || sb.Totals.LintDefects != 8 || sb.Delta != nil {
		t.Errorf("totals: %+v", sb.Totals)
	}

	writePass(t, tool, 2, []shot{
		{order: 0, id: "tasks", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 1, defects: map[string]int{"grid": 2}},
		{order: 1, id: "task-detail", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 1},
	})
	if _, err := tool.Scoreboard(ScoreboardOptions{Pass: 2, Backlog: filepath.Join(review, "backlog.json")}); diagCode(err) != DiagBacklogInvalid {
		t.Errorf("pass 1's backlog must not score pass 2: %v", err)
	}
	res, err = tool.Scoreboard(ScoreboardOptions{Pass: 2})
	if err != nil {
		t.Fatal(err)
	}
	d := res.Data.(Scoreboard).Delta
	if d == nil || d.Pass != 1 || d.Reviewed || d.Totals.LintDefects != -6 || d.Totals.NotOk != -1 || d.Totals.Lint["contrast"] != -2 || d.Totals.Lint["grid"] != -4 {
		t.Fatalf("delta: %+v", d)
	}
	md, _ := os.ReadFile(filepath.Join(tool.passAbs(2), "scoreboard.md"))
	if !strings.Contains(string(md), "| **all** | 2 | — |") || !strings.Contains(string(md), "`grid` 2 (−4)") {
		t.Errorf("markdown:\n%s", md)
	}

	bad := `{"v":1,"pass":1,"findings":[{"key":"k","screen":"s","area":"a","severity":"major","status":"open","title":"t","files":[],"acceptance":""}],"extra":1}`
	file := filepath.Join(t.TempDir(), "b.json")
	if err := os.WriteFile(file, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBacklog(file); diagCode(err) != DiagBacklogInvalid || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("unknown backlog keys are rejected: %v", err)
	}
	var b Backlog
	_ = json.Unmarshal([]byte(bad), &b)
	problems := strings.Join(b.Validate(), "\n")
	for _, want := range []string{`severity "major"`, "title and acceptance", "names the files"} {
		if !strings.Contains(problems, want) {
			t.Errorf("backlog Validate misses %q:\n%s", want, problems)
		}
	}
}

func TestRunPrintWritesRunFileAndReusesAnUnshotPass(t *testing.T) {
	cfg := testConfig()
	cfg.Apps["portal"] = App{BaseURL: "http://127.0.0.1:5500", Env: "UILOOP_TEST_PORTAL", Viewports: []string{"phone"}, Themes: []string{"dark"}}
	tool := newTool(t, cfg)
	if _, err := tool.Sync(false); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tool.Root, "tests/ui-loop/project.ts"), []byte("export default {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("UILOOP_TEST_PORTAL", "http://10.8.0.10:21782")
	opts := RunOptions{Print: true, Selection: Selection{Only: []string{"tasks*"}, Viewports: []string{"phone"}}}
	res, err := tool.Run(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(*RunData)
	if data.Pass != 1 || data.Executed || data.Command != `UILOOP_ROOT="$PWD" UILOOP_RUN="$PWD/.ui-loop/pass-1/run.json" pnpm exec playwright test -c "$PWD/tests/ui-loop/vendor/playwright.config.ts"` {
		t.Errorf("run data: %+v", data)
	}
	var run RunFile
	b, _ := os.ReadFile(filepath.Join(tool.Root, ".ui-loop/pass-1/run.json"))
	if err := json.Unmarshal(b, &run); err != nil {
		t.Fatal(err)
	}
	if run.V != RunVersion || run.Apps["portal"].BaseURL != "http://10.8.0.10:21782" || run.Viewports["phone"].Insets.Top != 59 ||
		!slices.Equal(run.Selection.Only, []string{"tasks*"}) || run.Selection.Themes == nil || run.Lint.Grid != 4 || run.Workers != 2 {
		t.Errorf("run.json: %s", b)
	}
	// Nothing was shot into pass 1, so the next run reuses it; a shot moves the next run on.
	if res, _ := tool.Run(context.Background(), opts); res.Data.(*RunData).Pass != 1 {
		t.Error("an unshot pass is reused")
	}
	writePass(t, tool, 1, []shot{{order: 0, id: "tasks", area: "tasks", vp: "phone", theme: "dark", status: "ok", bytes: 1}})
	if res, _ := tool.Run(context.Background(), opts); res.Data.(*RunData).Pass != 2 {
		t.Error("a shot pass is never overwritten by a new run")
	}
	opts.Wrap = "devbox run -- {cmd}"
	res, _ = tool.Run(context.Background(), opts)
	if cmd := res.Data.(*RunData).Command; !strings.HasPrefix(cmd, `devbox run -- 'UILOOP_ROOT="$PWD"`) {
		t.Errorf("wrapped command: %s", cmd)
	}
	if _, err := tool.Run(context.Background(), RunOptions{Selection: Selection{Apps: []string{"designer"}}}); diagCode(err) != DiagSelectionInvalid {
		t.Errorf("an unknown app is SELECTION_INVALID: %v", err)
	}
}

func TestScoreboardCountsUniqueDefectsAndRepeatedOffenders(t *testing.T) {
	tool := newTool(t, testConfig())
	sidebar := LintKey{Path: "nav.sidebar > a.item", Detail: "paddingTop:6"}
	writePass(t, tool, 1, []shot{
		{order: 0, id: "tasks", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 1, defects: map[string]int{"grid": 3},
			distinct: map[string][]LintKey{"grid": {sidebar, {Path: "main > h1", Detail: "marginTop:10"}}}},
		{order: 0, id: "tasks", area: "tasks", vp: "phone", theme: "dark", status: "ok", bytes: 1, defects: map[string]int{"grid": 1},
			distinct: map[string][]LintKey{"grid": {sidebar}}},
		{order: 1, id: "users", area: "admin", vp: "desktop", theme: "dark", status: "ok", bytes: 1, defects: map[string]int{"grid": 1, "contrast": 1},
			distinct: map[string][]LintKey{"grid": {sidebar}, "contrast": {{Path: "td", Detail: "3.1:1 < 4.5:1"}}}},
	})
	res, err := tool.Scoreboard(ScoreboardOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sb := res.Data.(Scoreboard)
	// Raw sums count the sidebar once per shot; unique counts it once per pass (and never sum areas).
	if !sb.UniqueKnown || sb.Totals.LintDefects != 6 || sb.Totals.LintDefectsUnique != 3 || sb.Totals.LintUnique["grid"] != 2 || sb.Areas[0].LintDefectsUnique != 2 {
		t.Errorf("unique totals: %+v / areas %+v", sb.Totals, sb.Areas)
	}
	if len(sb.Offenders) != 1 || sb.Offenders[0] != (Offender{Rule: "grid", Path: sidebar.Path, Detail: sidebar.Detail, Screens: 2, Shots: 3}) {
		t.Errorf("offenders: %+v", sb.Offenders)
	}
	md, _ := os.ReadFile(filepath.Join(tool.passAbs(1), "scoreboard.md"))
	if !strings.Contains(string(md), "| **all** | 2 | — | — | — | — | — | 6 | 3 |") || !strings.Contains(string(md), "`grid` 5, 2 unique") ||
		!strings.Contains(string(md), "· 2 screens, 3 shots") {
		t.Errorf("markdown:\n%s", md)
	}

	// A record from a harness before the distinct keys makes the unique columns unknown, never zero.
	writePass(t, tool, 2, []shot{{order: 0, id: "tasks", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 1, defects: map[string]int{"grid": 1}}})
	res, err = tool.Scoreboard(ScoreboardOptions{Pass: 2})
	if err != nil {
		t.Fatal(err)
	}
	if sb := res.Data.(Scoreboard); sb.UniqueKnown || sb.Delta == nil || sb.Delta.UniqueKnown {
		t.Errorf("old records: uniqueKnown %v, delta %+v", sb.UniqueKnown, sb.Delta)
	}
}

func TestExplicitPassZeroIsRefused(t *testing.T) {
	if err := CheckPassFlag(0, true); diagCode(err) != DiagSelectionInvalid || !strings.Contains(err.Error(), "numbered from 1") {
		t.Errorf("--pass 0 must be refused, got %v", err)
	}
	if err := CheckPassFlag(0, false); err != nil {
		t.Errorf("an absent --pass is the default, got %v", err)
	}
}

func TestCheckWarnsWhenADevboxRecipeSyncsTheOutDir(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "pwf-ui")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	tool, err := New(root, filepath.Join(root, "vybava.config.json"), testConfig(), "1.2.3", nil)
	if err != nil {
		t.Fatal(err)
	}
	write := func(file, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ignores := `"` + strings.Join(DevboxSyncIgnores(".ui-loop"), `", "`) + `"`
	// The repo's own recipe ignores everything; the sibling workspace's does not.
	write(filepath.Join(root, "devbox.yaml"), "apps:\n  designer:\n    sync: .\n    sync_ignores: [node_modules, "+ignores+"]\n")
	write(filepath.Join(parent, "compose", "devbox.yaml"), "apps:\n  pwf-ui:\n    source_only: true\n    sync: sibling:pwf-ui\n    sync_ignores: [.env, /.ui-loop/*/shots]\n  other:\n    sync: sibling:elsewhere\n")
	gaps := tool.devboxSyncGaps()
	if len(gaps) != 1 || gaps[0].App != "pwf-ui" || gaps[0].Recipe != "../compose/devbox.yaml" || slices.Contains(gaps[0].Missing, "/.ui-loop/*/shots") ||
		!slices.Contains(gaps[0].Missing, "/.ui-loop/*/.auth") {
		t.Fatalf("gaps: %+v", gaps)
	}
	// A worktree patch beside the recipe adds its ignores.
	write(filepath.Join(parent, "compose", "devbox.worktree.yaml"), "apps:\n  pwf-ui:\n    sync_ignores: ["+ignores+"]\n")
	if gaps := tool.devboxSyncGaps(); len(gaps) != 0 {
		t.Errorf("the patch's ignores count: %+v", gaps)
	}
}
