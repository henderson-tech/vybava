package uiloop

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// A failing check, or an app (probed at its env var's address) serving
// Vite's error page, fails capture but only warns review, which judges shots.
func TestDoctorFailsOnAFailingCheckAndWarnsAnotherStagesApp(t *testing.T) {
	red := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html><title>Error</title><script type="module">const { ErrorOverlay } = await import("/@vite/client")</script>`)
	}))
	defer red.Close()
	cfg := testConfig()
	cfg.Apps["portal"] = App{BaseURL: "http://127.0.0.1:1", Env: "UI_LOOP_DOCTOR_PORTAL_URL", Viewports: []string{"phone"}, Themes: []string{"dark"}}
	t.Setenv("UI_LOOP_DOCTOR_PORTAL_URL", red.URL)
	tool := newTool(t, cfg)
	doctor := func(stage string) (DoctorData, map[string]string) {
		t.Helper()
		res, err := tool.Doctor(context.Background(), DoctorOptions{For: stage})
		if err != nil {
			t.Fatal(err)
		}
		d := res.Data.(DoctorData)
		status := map[string]string{}
		for _, c := range d.Checks {
			status[c.ID] = c.Status
		}
		return d, status
	}

	d, status := doctor("capture")
	if d.OK || status["check"] != DoctorFail || d.Contract != StateContract || d.Vybava != "1.2.3" {
		t.Fatalf("no project.ts fails check, so the doctor: %+v", d)
	}
	if _, status := doctor("review"); status["check"] != DoctorWarn {
		t.Errorf("review runs no harness, so check only warns: %+v", status)
	}

	if _, err := tool.Init(); err != nil {
		t.Fatal(err)
	}
	tool.Exec = func(_ context.Context, c Cmd) (CmdOut, error) {
		if strings.HasSuffix(c.Args[len(c.Args)-1], "/check.ts") {
			return CmdOut{Stdout: `{"screens":1,"captures":1,"problems":[]}`}, nil
		}
		return CmdOut{}, nil // render-app-map.ts --check: current
	}
	writePass(t, tool, 1, []shot{{order: 0, id: "tasks", area: "tasks", vp: "phone", theme: "dark", status: "ok", bytes: 1}})
	d, status = doctor("capture")
	if d.OK || status["check"] != DoctorOK || status["apps"] != DoctorFail || status["pass"] != DoctorOK ||
		status["workspace"] != DoctorSkip || status["signin"] != DoctorSkip || !strings.Contains(d.Checks[2].Detail, "Vite's error page") ||
		!strings.Contains(d.Checks[2].Detail, red.URL+" ($UI_LOOP_DOCTOR_PORTAL_URL)") {
		t.Fatalf("a red dev server fails capture: %+v", d)
	}
	d, status = doctor("review")
	if !d.OK || status["apps"] != DoctorWarn {
		t.Errorf("review does not need the app: %+v", d)
	}
}

// A shot-less pass is resumed only while the source matches its capture.json
// revision, as state's next says; once HEAD moves, --resume is refused, and a
// plain run reuses the pass (ResolvePass).
func TestDoctorResumesAShotlessPassOnlyWhileItsRevisionHolds(t *testing.T) {
	tool := newTool(t, testConfig())
	evidenceRepo(t, tool)
	if err := tool.captureProvenance(1, false); err != nil {
		t.Fatal(err)
	}
	fix := func() string {
		t.Helper()
		row, err := tool.doctorPass("capture")
		if err != nil {
			t.Fatal(err)
		}
		if row.Status != DoctorWarn {
			t.Fatalf("a shot-less pass warns: %+v", row)
		}
		return row.Fix
	}
	if f := fix(); f != "vybava ui-loop run --resume --pass 1" {
		t.Errorf("unchanged source resumes, got %q", f)
	}
	writeFile(t, filepath.Join(tool.Root, "app.ts"), "changed\n")
	for _, args := range [][]string{{"add", "."}, {"-c", "user.name=Test", "-c", "user.email=test@example.test", "commit", "-m", "move"}} {
		if out, err := tool.git(args...); err != nil || out.Code != 0 {
			t.Fatalf("git %v: %+v %v", args, out, err)
		}
	}
	if f := fix(); f != "vybava ui-loop run" {
		t.Errorf("a moved HEAD reuses the pass with a plain run, got %q", f)
	}
}
