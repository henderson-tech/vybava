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
	at, failure         string // capturedAt; the recipe failure's error
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
		if s.at != "" {
			rec["capturedAt"] = s.at
		}
		if s.failure != "" {
			rec["failure"] = map[string]any{"step": "clickText", "stepIndex": 2, "error": s.failure}
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

func TestSplitPlansOneSetPerAreaSectionedByViewportAndTheme(t *testing.T) {
	cfg := testConfig()
	cfg.Publish = Publish{MaxFiles: 96} // an older config: accepted, ignored, warned about
	tool := newTool(t, cfg)
	// Manifest order mixes viewports and themes, and admin comes first; the
	// config's area, viewport and theme order must still win.
	writePass(t, tool, 1, []shot{
		{order: 0, id: "users", area: "admin", vp: "desktop", theme: "dark", status: "ok", bytes: 100},
		{order: 1, id: "list", area: "tasks", vp: "desktop", theme: "light", status: "ok", bytes: 100},
		{order: 2, id: "list", area: "tasks", vp: "phone", theme: "dark", status: "ok", bytes: 100},
		{order: 3, id: "list", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 100, full: 100},
		{order: 4, id: "detail", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 100},
		{order: 5, id: "gone", area: "tasks", vp: "phone", theme: "light", status: "unreachable"},
	})
	res, err := tool.Split(SplitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	plan := res.Data.(Plan)
	var got []string
	for _, s := range plan.Sets {
		got = append(got, s.Key+" | "+s.Title)
		for _, sec := range s.Sections {
			got = append(got, "  "+sec.Title+" "+strings.Join(sec.Labels, ","))
		}
	}
	want := []string{
		"ui-polish-tasks | ui-polish · tasks",
		"  Pass 1 · phone · light P1-LIST-PHONE-LIGHT,P1-LIST-PHONE-LIGHT-FULL,P1-DETAIL-PHONE-LIGHT",
		"  Pass 1 · phone · dark P1-LIST-PHONE-DARK",
		"  Pass 1 · desktop · light P1-LIST-DESKTOP-LIGHT",
		"ui-polish-admin | ui-polish · admin",
		"  Pass 1 · desktop · dark P1-USERS-DESKTOP-DARK",
	}
	if !slices.Equal(got, want) {
		t.Errorf("plan:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// The files follow the sections, so the manifest a publish builds does too.
	var labels []string
	for _, f := range plan.Sets[0].Files {
		labels = append(labels, f.Label)
	}
	if strings.Join(labels, ",") != "P1-LIST-PHONE-LIGHT,P1-LIST-PHONE-LIGHT-FULL,P1-DETAIL-PHONE-LIGHT,P1-LIST-PHONE-DARK,P1-LIST-DESKTOP-LIGHT" {
		t.Errorf("file order: %v", labels)
	}
	full := plan.Sets[0].Files[1]
	if full.Route != "/portal/#/list" || full.Viewport != "phone" || full.Theme != "light" || full.Size != "390x844@2" || !strings.HasPrefix(full.Note, "full content · pass 1 · clean") {
		t.Errorf("full companion: %+v", full)
	}
	if len(plan.Skipped) != 0 || len(plan.Notes) != 1 || fmt.Sprint(plan.Notes[0].Shots) != "[{gone phone light unreachable  }]" {
		t.Errorf("an unreachable shot is a note, never an image: skipped %v, notes %+v", plan.Skipped, plan.Notes)
	}
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != DiagConfigDeprecated {
		t.Errorf("publish.maxFiles is ignored with a warning: %+v", res.Diagnostics)
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
	if k := areaKey("ui-polish", strings.Repeat("area-", 20)); len(k) > maxKey || k == areaKey("ui-polish", strings.Repeat("area-", 19)+"x") {
		t.Errorf("an area key must cap at 64 and stay unique: %q", k)
	}
}

func TestSplitRefusesAnAreaAboveTheSetFileCap(t *testing.T) {
	defer func(n int) { setFileCap = n }(setFileCap)
	setFileCap = 3 // two captures and the manifest fit; three do not
	tool := newTool(t, testConfig())
	writePass(t, tool, 1, []shot{
		{order: 0, id: "users", area: "admin", vp: "desktop", theme: "dark", status: "ok", bytes: 10, full: 10},
		{order: 1, id: "list", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10, full: 10},
		{order: 2, id: "detail", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10},
	})
	res, err := tool.Split(SplitOptions{})
	if err != nil {
		t.Fatal(err)
	}
	plan := res.Data.(Plan)
	if len(plan.Sets) != 1 || plan.Sets[0].Key != "ui-polish-admin" || fmt.Sprint(plan.Skipped) != "[ui-polish-tasks (too many files)]" {
		t.Errorf("the oversized area is refused, the rest planned: %v %v", plan.Sets, plan.Skipped)
	}
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != DiagSetTooLarge || res.Diagnostics[0].Severity != "error" {
		t.Errorf("diagnostics: %+v", res.Diagnostics)
	}

	// The root is shared by every pass: pass 1's two adopted admin files leave
	// no room for pass 2's one.
	ledger := filepath.Join(tool.passAbs(1), "publish", "adopted", "ui-polish-admin")
	if err := os.MkdirAll(filepath.Dir(ledger), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger, []byte("shots/users/desktop.dark.png\tT1\nshots/users/desktop.dark.png\tT2\nshots/users/desktop.dark.full.png\tT1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writePass(t, tool, 2, []shot{{order: 0, id: "users", area: "admin", vp: "desktop", theme: "dark", status: "ok", bytes: 10}})
	res, err = tool.Split(SplitOptions{Pass: 2})
	if err != nil {
		t.Fatal(err)
	}
	if plan := res.Data.(Plan); len(plan.Sets) != 0 || fmt.Sprint(plan.Skipped) != "[ui-polish-admin (too many files)]" {
		t.Errorf("other passes' files count toward the cap (a retake's path once): %v %v", plan.Sets, plan.Skipped)
	}
}

// A --resume retake keeps the path; its new capturedAt re-adopts it.
func TestPublishReadoptsARetakenShot(t *testing.T) {
	tool := newTool(t, testConfig())
	tool.LookPath = func(string) (string, error) { return "/bin/vitrinka", nil }
	v := &fakeVitrinka{t: t, pushes: map[string]int{}}
	tool.Exec = func(_ context.Context, c Cmd) (CmdOut, error) { return v.exec(c.Args[1:]), nil }
	for _, at := range []string{"2026-09-30T10:00:00.000Z", "2026-09-30T10:00:00.000Z", "2026-09-30T12:00:00.000Z"} {
		writePass(t, tool, 1, []shot{
			{order: 0, id: "a", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10, at: at},
			{order: 1, id: "b", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10, at: "2026-09-30T10:00:00.000Z"},
		})
		if _, err := tool.Publish(context.Background(), PublishOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(v.captures) != "[P1-A-PHONE-LIGHT P1-B-PHONE-LIGHT P1-A-PHONE-LIGHT]" || v.pushes["ui-polish-tasks"] != 2 {
		t.Errorf("an unchanged pass is skipped, a retake re-adopted alone: %v %v", v.captures, v.pushes)
	}
}

func TestPublishRetriesAPushAndLeavesChunkedSetsAsLegacy(t *testing.T) {
	tool := newTool(t, testConfig())
	writePass(t, tool, 1, []shot{
		{order: 0, id: "a", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10},
		{order: 1, id: "a", area: "tasks", vp: "desktop", theme: "dark", status: "ok", bytes: 10},
		{order: 2, id: "u", area: "admin", vp: "phone", theme: "light", status: "ok", bytes: 10},
	})
	// Pass 1 was published the old way: a chunk set, its ledger and its index row.
	pub := filepath.Join(tool.passAbs(1), "publish")
	old := "ui-polish-p1-tasks-phone-light-1"
	if err := os.MkdirAll(filepath.Join(pub, "adopted"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pub, "adopted", old), []byte("shots/a/phone.light.png\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	idx, _ := json.Marshal(PublishIndex{Pass: 1, Sets: []PublishedSet{{Key: old, Title: "old", Files: 1, Status: "pushed", URL: "https://app.vitrinka.ai/w/fixit/boards/" + old}}, Notes: []AreaNotes{}})
	if err := os.WriteFile(filepath.Join(pub, "index.json"), idx, 0o644); err != nil {
		t.Fatal(err)
	}

	tool.LookPath = func(string) (string, error) { return "/bin/vitrinka", nil }
	tool.Sleep = func(time.Duration) {}
	v := &fakeVitrinka{t: t, pushes: map[string]int{}}
	tasksDown := true // the tasks push fails on the first run, then recovers
	var devices []string
	tool.Exec = func(_ context.Context, c Cmd) (CmdOut, error) {
		args := c.Args[1:]
		if strings.Join(args[:2], " ") == "board capture" {
			devices = append(devices, args[slices.Index(args, "--device")+1]+" "+args[slices.Index(args, "--state")+1])
		}
		if strings.Join(args[:2], " ") == "board push" && tasksDown && strings.HasSuffix(args[slices.Index(args, "--root")+1], "ui-polish-tasks") {
			v.pushes["ui-polish-tasks"]++
			return CmdOut{Code: 1, Stderr: "push failed: 503"}, nil
		}
		return v.exec(args), nil
	}
	res, err := tool.Publish(context.Background(), PublishOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range res.Data.(map[string]any)["sets"].([]PublishedSet) {
		got = append(got, fmt.Sprintf("%s %s %d %d", s.Key, s.Status, s.Files, len(s.Sections)))
	}
	if !slices.Equal(got, []string{"ui-polish-tasks failed 2 2", "ui-polish-admin pushed 1 1"}) {
		t.Errorf("publish: %v", got)
	}
	if v.pushes["ui-polish-tasks"] != 3 || v.pushes[old] != 0 {
		t.Errorf("3 plain tries, no halving; the chunk set is never pushed: %v", v.pushes)
	}
	// The chunk ledger never re-adopts a into its old set: the area set takes every file.
	if fmt.Sprint(v.captures) != "[P1-A-PHONE-LIGHT P1-A-DESKTOP-DARK P1-U-PHONE-LIGHT]" || fmt.Sprint(devices) != "[phone light desktop dark phone light]" {
		t.Errorf("captures: %v %v", v.captures, devices)
	}
	codes := map[string]string{}
	for _, d := range res.Diagnostics {
		codes[d.Code] = d.Fix
	}
	if !strings.Contains(codes[DiagPublishFailed], "--sets ui-polish-tasks") || codes[DiagLegacySets] == "" {
		t.Errorf("diagnostics: %+v", res.Diagnostics)
	}
	var index PublishIndex
	b, _ := os.ReadFile(filepath.Join(pub, "index.json"))
	if err := json.Unmarshal(b, &index); err != nil {
		t.Fatal(err)
	}
	if len(index.Legacy) != 1 || index.Legacy[0].Key != old || index.Legacy[0].URL == "" || len(index.Sets) != 2 {
		t.Errorf("the chunk row moves to legacy: %+v", index)
	}
	if s := index.Sets[0].Sections; len(s) != 2 || s[0].Title != "Pass 1 · phone · light" || s[1].Title != "Pass 1 · desktop · dark" || fmt.Sprint(s[1].Labels) != "[P1-A-DESKTOP-DARK]" {
		t.Errorf("sections in the index: %+v", s)
	}

	// Retrying by key pushes the set again without re-adopting it.
	tasksDown = false
	v.captures = nil
	res, err = tool.Publish(context.Background(), PublishOptions{Sets: []string{"ui-polish-tasks"}})
	if err != nil {
		t.Fatal(err)
	}
	sets := res.Data.(map[string]any)["sets"].([]PublishedSet)
	if len(sets) != 1 || sets[0].Status != "pushed" || len(v.captures) != 0 || sets[0].URL != "https://app.vitrinka.ai/w/fixit/boards/ui-polish-tasks" {
		t.Errorf("retry: %+v, captures %v", sets, v.captures)
	}
	// A plain re-run finds both pushed and does nothing.
	res, _ = tool.Publish(context.Background(), PublishOptions{})
	for _, s := range res.Data.(map[string]any)["sets"].([]PublishedSet) {
		if s.Status != "skipped" {
			t.Errorf("re-run touched %s (%s)", s.Key, s.Status)
		}
	}
}

func TestPublishResumesAcknowledgedSetsAfterCancellation(t *testing.T) {
	cfg := testConfig()
	tool := newTool(t, cfg)
	writePass(t, tool, 1, []shot{
		{order: 0, id: "a", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10},
		{order: 1, id: "b", area: "admin", vp: "phone", theme: "light", status: "ok", bytes: 10},
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

func TestPublishRetriesNegativeAcknowledgement(t *testing.T) {
	tool := newTool(t, testConfig())
	p := &publisher{t: tool, ctx: context.Background(), retries: 3}
	tries, delays := 0, 0
	tool.Sleep = func(time.Duration) { delays++ }
	tool.Exec = func(context.Context, Cmd) (CmdOut, error) {
		tries++
		if tries < 3 {
			return CmdOut{Stdout: `{"ok":false,"data":{"url":"https://app.vitrinka.ai/boards/stale"}}`}, nil
		}
		return CmdOut{Stdout: `{"ok":true,"data":{"url":"https://app.vitrinka.ai/boards/recovered"}}`}, nil
	}
	url, attempts, err := p.push(tool.Root, "area")
	if err != nil || attempts != 3 || delays != 2 || url != "https://app.vitrinka.ai/boards/recovered" {
		t.Fatalf("negative acknowledgements bypassed retry: %s %d %d %v", url, attempts, delays, err)
	}
}

func TestPublishResumesPartialAdoptionWithoutLosingEarlierPasses(t *testing.T) {
	tool := newTool(t, testConfig())
	writePass(t, tool, 1, []shot{{order: 0, id: "old", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10}})
	tool.LookPath = func(string) (string, error) { return "/bin/vitrinka", nil }
	tool.Sleep = func(time.Duration) {}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	v := &fakeVitrinka{t: t, pushes: map[string]int{}}
	tool.Exec = func(_ context.Context, c Cmd) (CmdOut, error) {
		args := c.Args[1:]
		out := v.exec(args)
		if args[1] == "capture" && len(v.captures) == 2 {
			cancel()
		}
		return out, nil
	}
	if _, err := tool.Publish(context.Background(), PublishOptions{Pass: 1}); err != nil {
		t.Fatal(err)
	}
	writePass(t, tool, 2, []shot{
		{order: 0, id: "new-a", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10},
		{order: 1, id: "new-b", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10},
	})
	if _, err := tool.Publish(ctx, PublishOptions{Pass: 2}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected adoption cutoff: %v", err)
	}
	for range 2 {
		if _, err := tool.Publish(context.Background(), PublishOptions{Pass: 2}); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(v.captures) != "[P1-OLD-PHONE-LIGHT P2-NEW-A-PHONE-LIGHT P2-NEW-B-PHONE-LIGHT]" {
		t.Fatalf("resume lost or duplicated captures: %v", v.captures)
	}
	if v.pushes["ui-polish-tasks"] != 2 {
		t.Fatalf("acknowledged sets replayed: %v", v.pushes)
	}
	root := filepath.Join(filepath.Dir(tool.passAbs(2)), "sets", "ui-polish-tasks")
	b, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	if err := json.Unmarshal(b, &labels); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(labels, v.captures) {
		t.Fatalf("shared manifest lost or duplicated entries: %v", labels)
	}
	if _, err := os.Stat(filepath.Join(root, descriptor)); err != nil {
		t.Fatalf("descriptor not retained: %v", err)
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

// fakeVitrinka answers board init / capture / push like vitrinka 5.13.
// A set key is a board, so pass 2 must land on pass 1's boards: same key and
// root, its own sections — and its own ledger, since shot paths repeat.
func TestPublishPass2AddsToPass1Boards(t *testing.T) {
	tool := newTool(t, testConfig())
	tool.LookPath = func(string) (string, error) { return "/bin/vitrinka", nil }
	tool.Sleep = func(time.Duration) {}
	v := &fakeVitrinka{t: t, pushes: map[string]int{}}
	tool.Exec = func(_ context.Context, c Cmd) (CmdOut, error) { return v.exec(c.Args[1:]), nil }
	var urls []string
	for pass := 1; pass <= 2; pass++ {
		writePass(t, tool, pass, []shot{{order: 0, id: "a", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10}})
		res, err := tool.Publish(context.Background(), PublishOptions{Pass: pass})
		if err != nil {
			t.Fatal(err)
		}
		sets := res.Data.(map[string]any)["sets"].([]PublishedSet)
		if len(sets) != 1 || sets[0].Sections[0].Title != fmt.Sprintf("Pass %d · phone · light", pass) {
			t.Fatalf("pass %d sets: %+v", pass, sets)
		}
		urls = append(urls, sets[0].URL)
	}
	if urls[0] != urls[1] || v.pushes["ui-polish-tasks"] != 2 {
		t.Errorf("both passes push one board: %v %v", urls, v.pushes)
	}
	if !slices.Equal(v.captures, []string{"P1-A-PHONE-LIGHT", "P2-A-PHONE-LIGHT"}) {
		t.Errorf("pass 2 adopts its own shot despite the repeated path: %v", v.captures)
	}
}

// A full-content companion is narrower than DPR × the page viewport, so it
// passes its own CSS size (from the PNG header) as --viewport, hi-dpi kept on.
func TestPublishPassesAFullShotItsOwnViewport(t *testing.T) {
	tool := newTool(t, testConfig())
	writePass(t, tool, 1, []shot{{order: 0, id: "a", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10, full: 10}})
	fulls, _ := filepath.Glob(filepath.Join(tool.passAbs(1), "shots", "*", "*.full.png"))
	if len(fulls) != 1 {
		t.Fatalf("full files: %v", fulls)
	}
	hdr := append([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x0dIHDR"), 0, 0, 0x02, 0xee, 0, 0, 0x0b, 0xb9) // 750 × 3001
	if err := os.WriteFile(fulls[0], hdr, 0o644); err != nil {
		t.Fatal(err)
	}
	tool.LookPath = func(string) (string, error) { return "/bin/vitrinka", nil }
	v := &fakeVitrinka{t: t, pushes: map[string]int{}}
	viewports := map[string]string{}
	tool.Exec = func(_ context.Context, c Cmd) (CmdOut, error) {
		args := c.Args[1:]
		if strings.Join(args[:2], " ") == "board capture" {
			if slices.Contains(args, "--hidpi=false") {
				t.Error("a full shot keeps the hi-dpi check")
			}
			viewports[filepath.Base(args[slices.Index(args, "--file")+1])] = args[slices.Index(args, "--viewport")+1]
		}
		return v.exec(args), nil
	}
	if _, err := tool.Publish(context.Background(), PublishOptions{}); err != nil {
		t.Fatal(err)
	}
	if viewports[filepath.Base(fulls[0])] != "375x1500@2" || viewports[strings.TrimSuffix(filepath.Base(fulls[0]), ".full.png")+".png"] != "390x844@2" {
		t.Errorf("viewports: %v", viewports)
	}
}

// One refused file never aborts its set: the rest is adopted and pushed, the
// refusal is recorded and reported, and the next publish retries only it.
func TestPublishRecordsARefusedFileAndPushesTheRest(t *testing.T) {
	tool := newTool(t, testConfig())
	writePass(t, tool, 1, []shot{
		{order: 0, id: "a", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10},
		{order: 1, id: "b", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10},
	})
	tool.LookPath = func(string) (string, error) { return "/bin/vitrinka", nil }
	v := &fakeVitrinka{t: t, pushes: map[string]int{}}
	refuse := true
	tool.Exec = func(_ context.Context, c Cmd) (CmdOut, error) {
		args := c.Args[1:]
		if refuse && strings.Join(args[:2], " ") == "board capture" && args[slices.Index(args, "--label")+1] == "P1-A-PHONE-LIGHT" {
			return CmdOut{Code: 1, Stderr: "vitrinka config hidpi off"}, nil
		}
		return v.exec(args), nil
	}
	res, err := tool.Publish(context.Background(), PublishOptions{})
	if err != nil {
		t.Fatal(err)
	}
	s := res.Data.(map[string]any)["sets"].([]PublishedSet)[0]
	if s.Status != "pushed" || len(s.Refused) != 1 || !strings.HasSuffix(s.Refused[0].Path, ".png") || !strings.Contains(s.Refused[0].Error, "hidpi off") {
		t.Errorf("set: %+v", s)
	}
	if fmt.Sprint(v.captures) != "[P1-B-PHONE-LIGHT]" || v.pushes["ui-polish-tasks"] != 1 {
		t.Errorf("the rest is adopted and pushed: %v %v", v.captures, v.pushes)
	}
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != DiagPublishRefused || !strings.Contains(res.Diagnostics[0].Detail, "refused 1 of 2") {
		t.Errorf("diagnostics: %+v", res.Diagnostics)
	}
	var index PublishIndex
	b, _ := os.ReadFile(filepath.Join(tool.passAbs(1), "publish", "index.json"))
	if err := json.Unmarshal(b, &index); err != nil || len(index.Sets[0].Refused) != 1 {
		t.Errorf("index records the refusal: %v %+v", err, index.Sets)
	}

	refuse = false
	res, err = tool.Publish(context.Background(), PublishOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s := res.Data.(map[string]any)["sets"].([]PublishedSet)[0]; s.Status != "pushed" || len(s.Refused) != 0 || fmt.Sprint(v.captures) != "[P1-B-PHONE-LIGHT P1-A-PHONE-LIGHT]" {
		t.Errorf("a set with refusals is never skipped; only the refused file is retried: %+v %v", s, v.captures)
	}

	// Vitrinka failing to run at all still aborts the set.
	tool.Exec = func(context.Context, Cmd) (CmdOut, error) { return CmdOut{}, errors.New("exec: vitrinka: not found") }
	res, err = tool.Publish(context.Background(), PublishOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if s := res.Data.(map[string]any)["sets"].([]PublishedSet)[0]; s.Status != "failed" {
		t.Errorf("an exec failure aborts: %+v", s)
	}
}

type fakeVitrinka struct {
	t        *testing.T
	captures []string
	pushes   map[string]int
}

func (v *fakeVitrinka) exec(args []string) CmdOut {
	root := args[slices.Index(args, "--root")+1]
	switch strings.Join(args[:2], " ") {
	case "board init":
		if err := os.WriteFile(filepath.Join(root, ".vitrinka"), []byte(`{}`), 0o644); err != nil {
			v.t.Fatal(err)
		}
	case "board capture":
		// The descriptor stays in place: the capture fires vitrinka's detached push.
		if _, err := os.Stat(filepath.Join(root, ".vitrinka")); err != nil {
			v.t.Error("capture ran without the descriptor: no detached push would carry the shot")
		}
		label := args[slices.Index(args, "--label")+1]
		v.captures = append(v.captures, label)
		// Capture appends; keep a real mock manifest to expose duplicate adoption
		// and accidental root rebuilding across passes.
		file := filepath.Join(root, "manifest.json")
		var labels []string
		if b, err := os.ReadFile(file); err == nil {
			if err := json.Unmarshal(b, &labels); err != nil {
				v.t.Fatal(err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			v.t.Fatal(err)
		}
		b, err := json.Marshal(append(labels, label))
		if err != nil {
			v.t.Fatal(err)
		}
		if err := os.WriteFile(file, b, 0o644); err != nil {
			v.t.Fatal(err)
		}
	case "board push":
		v.pushes[filepath.Base(root)]++
		// vitrinka 5.13 refuses a root holding anything but screenshot-set content.
		entries, _ := os.ReadDir(root)
		for _, e := range entries {
			if e.Name() != ".vitrinka" && e.Name() != "manifest.json" && filepath.Ext(e.Name()) != ".webp" {
				v.t.Errorf("%s holds %s at push: board push refuses non-screenshot content", root, e.Name())
			}
		}
		return CmdOut{Stdout: `{"v":1,"ok":true,"data":{"url":"https://app.vitrinka.ai/w/fixit/boards/` + filepath.Base(root) + `"}}`}
	default:
		v.t.Fatalf("unexpected vitrinka %v", args)
	}
	return CmdOut{}
}

// copyTree copies src into dst, skipping what follow's rsync excludes.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if rel == ".auth" || rel == "playwright" {
			return filepath.SkipDir
		}
		if d.IsDir() || rel == "run.json" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Join(dst, filepath.Dir(rel)), 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dst, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestFollowPublishesOnlyNewFinalShotsAndStopsOnDoneAndIdle(t *testing.T) {
	tool := newTool(t, testConfig())
	box := newTool(t, testConfig()) // its pass dir is the capture box's
	const created = "2026-09-30T10:00:00Z"
	run, _ := json.Marshal(RunFile{V: RunVersion, Pass: 1, CreatedAt: created})
	if err := os.MkdirAll(tool.passAbs(1), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tool.passAbs(1), "run.json"), run, 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 30, 10, 5, 0, 0, time.UTC)
	tool.Now = func() time.Time { return now }
	tool.Sleep = func(d time.Duration) { now = now.Add(d) }
	tool.LookPath = func(string) (string, error) { return "/bin/x", nil }
	v := &fakeVitrinka{t: t, pushes: map[string]int{}}
	ticks := 0
	var perTick [][]string
	tool.Exec = func(_ context.Context, c Cmd) (CmdOut, error) {
		if c.Args[0] == "vitrinka" {
			return v.exec(c.Args[1:]), nil
		}
		if c.Args[0] != "rsync" || !slices.Contains(c.Args, "--exclude=/.auth/") || !slices.Contains(c.Args, "--exclude=/playwright/") || c.Args[len(c.Args)-2] != "devops:ws/pwf/pwf-ui/.ui-loop/pass-1/" {
			t.Fatalf("fetch: %v", c.Args)
		}
		ticks++
		perTick = append(perTick, v.captures)
		v.captures = nil
		switch ticks {
		case 1:
			writePass(t, box, 1, []shot{
				{order: 0, id: "a", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10, at: "2026-09-30T10:01:00.000Z"},
				{order: 1, id: "b", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10, at: "2026-09-30T10:02:00.000Z"},
				// Taken by the run before the resume: it is being retaken, so it is not
				// adopted although its status is an image status.
				{order: 2, id: "c", area: "tasks", vp: "phone", theme: "light", status: "theme-mismatch", bytes: 10, at: "2026-09-29T09:00:00.000Z"},
				{order: 3, id: "d", area: "tasks", vp: "phone", theme: "light", status: "unreachable", at: "2026-09-30T10:02:00.000Z"},
			})
			// rsync brought b's record before its PNG.
			if err := os.Remove(filepath.Join(box.passAbs(1), "shots", "b", "phone.light.png")); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(box.passAbs(1), ".auth"), 0o755); err != nil {
				t.Fatal(err)
			}
		case 2:
			writePass(t, box, 1, []shot{
				{order: 1, id: "b", area: "tasks", vp: "phone", theme: "light", status: "ok", bytes: 10, at: "2026-09-30T10:02:00.000Z"},
				{order: 2, id: "c", area: "tasks", vp: "phone", theme: "light", status: "recipe-failed", bytes: 10, at: "2026-09-30T10:04:00.000Z", failure: "no Sign in button"},
				{order: 4, id: "u", area: "admin", vp: "desktop", theme: "dark", status: "ok", bytes: 10, at: "2026-09-30T10:04:00.000Z"},
			})
			done, _ := json.Marshal(DoneFile{V: 1, Pass: 1, Run: created, Shots: 5})
			if err := os.WriteFile(filepath.Join(box.passAbs(1), "done.json"), done, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		copyTree(t, box.passAbs(1), tool.passAbs(1))
		return CmdOut{}, nil
	}
	res, err := tool.Follow(context.Background(), FollowOptions{From: "devops:ws/pwf/pwf-ui/.ui-loop/pass-1", Interval: 30 * time.Second, UntilIdle: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	perTick = append(perTick, v.captures)
	data := res.Data.(FollowData)
	// Tick 2 adopts b and u alone (a is in the ledger); c and d never become images.
	if got := fmt.Sprint(perTick); got != "[[] [P1-A-PHONE-LIGHT] [P1-B-PHONE-LIGHT P1-U-DESKTOP-DARK] [] []]" {
		t.Errorf("captures per tick: %s", got)
	}
	if !data.Done || data.Ticks != 4 || data.Final != 5 {
		t.Errorf("stops on done + a minute idle: %+v", data)
	}
	if v.pushes["ui-polish-tasks"] != 2 || v.pushes["ui-polish-admin"] != 1 || len(v.pushes) != 2 {
		t.Errorf("one push per tick per area set that grew: %v", v.pushes)
	}
	if _, err := os.Stat(filepath.Join(tool.passAbs(1), ".auth")); err == nil {
		t.Error("storage states came to the Mac")
	}
	notes := fmt.Sprint(data.Notes)
	if notes != "[{tasks [{c phone light recipe-failed step 2 clickText no Sign in button} {d phone light unreachable  }]}]" {
		t.Errorf("failures are notes in the index: %s", notes)
	}
	if len(data.Sets) != 2 || data.Sets[0].Key != "ui-polish-tasks" || data.Sets[0].Status != "pushed" || data.Sets[0].Files != 2 || data.Sets[1].Files != 1 {
		t.Errorf("sets: %+v", data.Sets)
	}
}
