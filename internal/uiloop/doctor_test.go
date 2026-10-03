package uiloop

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A failing check fails the doctor for every stage; an app serving Vite's
// error page fails capture but only warns review, which judges shots.
func TestDoctorFailsOnAFailingCheckAndWarnsAnotherStagesApp(t *testing.T) {
	red := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `<!DOCTYPE html><title>Error</title><script type="module">const { ErrorOverlay } = await import("/@vite/client")</script>`)
	}))
	defer red.Close()
	cfg := testConfig()
	cfg.Apps["portal"] = App{BaseURL: red.URL, Viewports: []string{"phone"}, Themes: []string{"dark"}}
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

	d, status := doctor("review")
	if d.OK || status["check"] != DoctorFail || d.Contract != StateContract || d.Vybava != "1.2.3" {
		t.Fatalf("no project.ts fails check, so the doctor: %+v", d)
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
		status["workspace"] != DoctorSkip || status["signin"] != DoctorSkip || !strings.Contains(d.Checks[2].Detail, "Vite's error page") {
		t.Fatalf("a red dev server fails capture: %+v", d)
	}
	d, status = doctor("review")
	if !d.OK || status["apps"] != DoctorWarn {
		t.Errorf("review does not need the app: %+v", d)
	}
}
