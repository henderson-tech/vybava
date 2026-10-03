package uiloop

// doctor.go owns `ui-loop doctor`: one preflight before a review-loop stage,
// every check a row with its fix, so a stage stops on a broken setup (a red
// dev server, a drifted vendor, an empty pass) instead of spending a capture
// or a review on it.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Doctor check statuses.
const (
	DoctorOK   = "ok"
	DoctorWarn = "warn"
	DoctorFail = "fail"
	// DoctorSkip: the check cannot be made from what the config holds yet.
	DoctorSkip = "skip"
)

// DoctorStages are the stages --for takes.
var DoctorStages = []string{"capture", "review", "fix", "verify"}

// DoctorCheck is one row of doctor's checks.
type DoctorCheck struct {
	ID     string `json:"id"`
	Status string `json:"status"` // ok | warn | fail | skip
	Detail string `json:"detail"`
	Fix    string `json:"fix"`
	// diags are the row's diagnostics, at the row's severity.
	diags []runxDiagnostic
}

// DoctorData is `ui-loop doctor`'s data: OK is false iff a check failed.
type DoctorData struct {
	OK       bool          `json:"ok"`
	Vybava   string        `json:"vybava"`
	Contract int           `json:"contract"`
	Checks   []DoctorCheck `json:"checks"`
}

// DoctorOptions are doctor's flags.
type DoctorOptions struct {
	// For is the stage to preflight; empty holds every check to every stage.
	For string
}

// appProbeTimeout bounds one app's answer.
const appProbeTimeout = 5 * time.Second

// Doctor runs every check. A failing check the --for stage does not need
// (app reachability for review or fix) warns instead.
func (t *Tool) Doctor(ctx context.Context, o DoctorOptions) (Result, error) {
	if o.For != "" && !slices.Contains(DoctorStages, o.For) {
		return Result{}, diag(DiagSelectionInvalid, fmt.Sprintf("--for %q is not one of %s", o.For, strings.Join(DoctorStages, ", ")), "vybava ui-loop doctor --for capture --json")
	}
	check, err := t.doctorCheck()
	if err != nil {
		return Result{}, err
	}
	pass, err := t.doctorPass(o.For)
	if err != nil {
		return Result{}, err
	}
	data := DoctorData{OK: true, Vybava: t.Version, Contract: StateContract, Checks: []DoctorCheck{
		check,
		{ID: "contract", Status: DoctorOK, Detail: fmt.Sprintf("ui-loop state contract %d (vybava %s)", StateContract, t.Version),
			Fix: "brew upgrade --cask vybava"},
		neededBy(t.doctorApps(ctx, o.For), o.For, "capture", "verify"),
		pass,
		{ID: "workspace", Status: DoctorSkip, Detail: "not checked: the Devbox workspace and its hold are not in the config yet (they arrive with uiLoop.capture)",
			Fix: "devbox status --json names the workspace serving the apps; devbox hold <workspace> --for 4h keeps it from parking"},
		{ID: "signin", Status: DoctorSkip, Detail: "not checked: per-persona sign-in is not probed yet (it arrives with `ui-loop run --probe`)",
			Fix: "sign in as each persona project.ts logs in as, in the running app; a capture records a failed sign-in as recipe-failed"},
	}}
	var res Result
	for _, c := range data.Checks {
		data.OK = data.OK && c.Status != DoctorFail
		res.Diagnostics = append(res.Diagnostics, c.diags...)
		for _, d := range c.diags {
			if d.Fix != "" && d.Severity != "info" && !slices.Contains(res.Next, d.Fix) {
				res.Next = append(res.Next, d.Fix)
			}
		}
	}
	res.Data = data
	if len(res.Next) == 0 {
		res.Next = []string{"vybava ui-loop state --json"}
	}
	return res, nil
}

// doctorCommand reruns the doctor for stage.
func doctorCommand(stage string) string {
	if stage == "" {
		return "vybava ui-loop doctor --json"
	}
	return "vybava ui-loop doctor --for " + stage + " --json"
}

// neededBy turns a failing check into a warning when --for names a stage
// that does not need it; without --for every stage needs it.
func neededBy(c DoctorCheck, stage string, stages ...string) DoctorCheck {
	if c.Status != DoctorFail || stage == "" || slices.Contains(stages, stage) {
		return c
	}
	c.Status = DoctorWarn
	c.Detail += fmt.Sprintf(" (the %s stage does not need it)", stage)
	diags := make([]runxDiagnostic, len(c.diags))
	for i, d := range c.diags {
		if d.Severity == "error" {
			d.Severity = "warning"
		}
		diags[i] = d
	}
	c.diags = diags
	return c
}

// doctorCheck folds `check` in: its errors fail the row, its warnings warn
// it, and its diagnostics travel with their own codes.
func (t *Tool) doctorCheck() (DoctorCheck, error) {
	res, err := t.Check(false)
	if err != nil {
		return DoctorCheck{}, err
	}
	data := res.Data.(*CheckData)
	row := DoctorCheck{ID: "check", Status: DoctorOK, diags: res.Diagnostics,
		Detail: fmt.Sprintf("vendor clean, manifest %s (%d screens), app map %s", data.Manifest.Status, data.Manifest.Screens, data.AppMap.Status)}
	var details, fixes []string
	for _, sev := range []string{"error", "warning"} {
		for _, d := range res.Diagnostics {
			if d.Severity != sev {
				continue
			}
			if row.Status == DoctorOK {
				row.Status = map[string]string{"error": DoctorFail, "warning": DoctorWarn}[sev]
			}
			details = append(details, d.Code+": "+d.Detail)
			if d.Fix != "" && !slices.Contains(fixes, d.Fix) {
				fixes = append(fixes, d.Fix)
			}
		}
	}
	if len(details) > 0 {
		row.Detail, row.Fix = strings.Join(details, "; "), strings.Join(fixes, "; ")
	}
	return row, nil
}

// doctorApps asks each app's base URL, resolved as the capture resolves it,
// for a 2xx/3xx answer that is not a dev server's error page. A redirect is
// an answer; it is not followed.
func (t *Tool) doctorApps(ctx context.Context, stage string) DoctorCheck {
	client := &http.Client{Timeout: appProbeTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	row := DoctorCheck{ID: "apps", Status: DoctorOK}
	var details, fixes []string
	for _, name := range t.Config.AppNames() {
		app := t.Config.Apps[name]
		base, from := app.baseURLHere()
		problem, red := probeApp(ctx, client, base)
		detail := fmt.Sprintf("%s %s (%s)", name, base, from)
		if problem == "" {
			details = append(details, detail+" answers")
			continue
		}
		detail += ": " + problem
		fix := fmt.Sprintf("start %s's dev server so %s answers", name, base)
		if red {
			fix = fmt.Sprintf("fix the build error %s's dev server shows at %s", name, base)
		}
		if app.Env != "" {
			fix += fmt.Sprintf(", or export %s=<the address it answers on>", app.Env)
		}
		fix += ", then " + doctorCommand(stage)
		row.Status = DoctorFail
		details, fixes = append(details, detail), append(fixes, fix)
		row.diags = append(row.diags, errDiag(DiagAppUnreachable, detail, fix))
	}
	row.Detail, row.Fix = strings.Join(details, "; "), strings.Join(fixes, "; ")
	return row
}

// devServerPages are bodies a dev server answers with instead of the app; a
// status alone would call a red build reachable. red marks a build error.
var devServerPages = []struct {
	marker, what string
	red          bool
}{
	{"vite-error-overlay", "the Vite error overlay", true},
	{"ErrorOverlay", "Vite's error page", true},
	{"✘ [ERROR]", "an Angular CLI / esbuild compile error", true},
	{"Failed to compile", "a webpack compile error", true},
	{"Cannot GET", "`Cannot GET` (nothing serves this path: a first build that failed, or the wrong port)", false},
}

// probeApp GETs url once. problem is empty when it answers 2xx/3xx with a
// body that is no dev-server error page; red marks a build error.
func probeApp(ctx context.Context, client *http.Client, url string) (problem string, red bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err.Error(), false
	}
	resp, err := client.Do(req)
	if err != nil {
		return err.Error(), false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Sprintf("answers %d, but its body breaks off: %v", resp.StatusCode, err), false
	}
	for _, p := range devServerPages {
		if bytes.Contains(body, []byte(p.marker)) {
			return fmt.Sprintf("answers %d with %s", resp.StatusCode, p.what), p.red
		}
	}
	if resp.StatusCode >= 400 {
		return fmt.Sprintf("answers %d", resp.StatusCode), false
	}
	return "", false
}

// doctorPass reads the newest pass. A shot-less one warns whatever the
// stage: `run` reuses it rather than skipping it (ResolvePass), and `state`
// reads it as the latest pass. No pass with shots at all fails the stages
// that judge one (review, fix, verify); capture starts one.
func (t *Tool) doctorPass(stage string) (DoctorCheck, error) {
	row := DoctorCheck{ID: "pass", Status: DoctorOK}
	passes, err := t.Passes()
	if err != nil {
		return row, err
	}
	judges := stage == "review" || stage == "fix" || stage == "verify"
	if len(passes) == 0 {
		row.Detail = fmt.Sprintf("no pass under %s yet: vybava ui-loop run starts pass-1", t.Config.Out)
		if judges {
			row.Status, row.Fix = DoctorFail, "vybava ui-loop run"
			row.Detail = fmt.Sprintf("no pass under %s: the %s stage judges a captured pass", t.Config.Out, stage)
			row.diags = []runxDiagnostic{errDiag(DiagPassMissing, row.Detail, row.Fix)}
		}
		return row, nil
	}
	last := passes[len(passes)-1]
	dir := t.PassDir(last)
	if t.hasShots(last) {
		screens, err := os.ReadDir(filepath.Join(t.passAbs(last), "shots"))
		if err != nil {
			return row, err
		}
		row.Detail = fmt.Sprintf("%s has shots of %d screens", dir, len(screens))
		return row, nil
	}
	row.Status = DoctorWarn
	row.Detail = fmt.Sprintf("%s holds no shots yet (a --print whose command never ran, or a capture killed before its first shot): the next run reuses it, never skips it, and state reads it as the latest pass", dir)
	row.Fix = fmt.Sprintf("vybava ui-loop run --resume --pass %d", last)
	prev := 0
	for _, p := range passes[:len(passes)-1] {
		if t.hasShots(p) {
			prev = p
		}
	}
	if prev > 0 {
		row.Fix += fmt.Sprintf(", or delete %s when it is a stray --print and %s is the pass to carry on", dir, t.PassDir(prev))
	} else if judges {
		row.Status = DoctorFail
		row.Detail = fmt.Sprintf("no pass under %s holds shots, and the %s stage judges a captured pass; ", t.Config.Out, stage) + row.Detail
	}
	sev := map[string]string{DoctorWarn: "warning", DoctorFail: "error"}[row.Status]
	row.diags = []runxDiagnostic{{Code: DiagPassMissing, Severity: sev, Detail: row.Detail, Fix: row.Fix}}
	return row, nil
}
