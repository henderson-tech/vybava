package uiloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"
)

// LoopConfig is the JSON handed to check.ts and render-app-map.ts in
// UILOOP_CONFIG (harness/run.ts LoopConfig).
type LoopConfig struct {
	Dir       string              `json:"dir"`
	AppMap    string              `json:"appMap"`
	Areas     []string            `json:"areas"`
	Apps      map[string]App      `json:"apps"`
	Viewports map[string]Viewport `json:"viewports"`
}

func (t *Tool) loopConfig() LoopConfig {
	c := t.Config
	return LoopConfig{Dir: c.Dir, AppMap: c.AppMap, Areas: c.Areas, Apps: c.Apps, Viewports: c.ResolvedViewports()}
}

// ManifestCheck is check.ts's answer.
type ManifestCheck struct {
	// Status: ok | problems | skipped | failed.
	Status   string   `json:"status"`
	Screens  int      `json:"screens"`
	Captures int      `json:"captures"`
	Problems []string `json:"problems"`
	Reason   string   `json:"reason,omitempty"`
}

// AppMapCheck is render-app-map.ts --check's answer.
type AppMapCheck struct {
	// Status: ok | stale | skipped | failed.
	Status string `json:"status"`
	File   string `json:"file"`
	Reason string `json:"reason,omitempty"`
}

// CheckData is what check reports, one part per concern.
type CheckData struct {
	Root     string        `json:"root"`
	Config   string        `json:"config"`
	TSRunner string        `json:"tsRunner"`
	Vendor   VendorReport  `json:"vendor"`
	Project  bool          `json:"project"`
	Manifest ManifestCheck `json:"manifest"`
	AppMap   AppMapCheck   `json:"appMap"`
	// DevboxSync: Devbox apps that sync this repo without the capture's sync_ignores.
	DevboxSync []SyncGap `json:"devboxSync"`
}

const tsTimeout = 3 * time.Minute

// tsScript runs one vendored script through the repo's tsRunner.
func (t *Tool) tsScript(script string, args ...string) (CmdOut, error) {
	cfg, err := json.Marshal(t.loopConfig())
	if err != nil {
		return CmdOut{}, err
	}
	argv := append(strings.Fields(t.Config.TSRunner), t.Config.Dir+"/vendor/"+script)
	argv = append(argv, args...)
	return t.Exec(context.Background(), Cmd{
		Dir:     t.Root,
		Env:     []string{"UILOOP_CONFIG=" + string(cfg), "UILOOP_ROOT=" + t.Root},
		Args:    argv,
		Timeout: tsTimeout,
	})
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

func excerpt(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 1200 {
		s = "…" + s[len(s)-1200:]
	}
	return s
}

func (t *Tool) projectExists() (bool, error) {
	_, err := os.Stat(t.abs(t.Config.Dir + "/project.ts"))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Check reports vendor drift, the project file, manifest validity and the
// app map's freshness — each separately. skipTS skips the two parts that
// need the repo's tsRunner.
func (t *Tool) Check(skipTS bool) (Result, error) {
	data := CheckData{
		Root: t.Root, Config: t.ConfigPath, TSRunner: t.Config.TSRunner,
		Manifest: ManifestCheck{Status: "skipped", Problems: []string{}},
		AppMap:   AppMapCheck{Status: "skipped", File: t.Config.AppMap},
	}
	res := Result{Data: &data}
	vendor, err := t.Vendor()
	if err != nil {
		return res, err
	}
	data.Vendor = vendor
	var drift, edited []string
	for _, f := range vendor.Files {
		switch f.State {
		case "ok":
		case "edited":
			edited = append(edited, f.File)
		default:
			drift = append(drift, f.File+" ("+f.State+")")
		}
	}
	if len(edited) > 0 {
		res.Diagnostics = append(res.Diagnostics, warn(DiagVendorEdited, "vendored files edited in the repo: "+strings.Join(edited, ", "),
			"move the change into Výbava (internal/uiloop/harness), then `vybava ui-loop sync --force`"))
	}
	if len(drift) > 0 || len(edited) > 0 {
		res.Diagnostics = append(res.Diagnostics, errDiag(DiagVendorDrift, "vendor differs from this vybava's harness: "+strings.Join(append(drift, edited...), ", "), "vybava ui-loop sync"))
	}
	if data.Project, err = t.projectExists(); err != nil {
		return res, err
	}
	if !data.Project {
		res.Diagnostics = append(res.Diagnostics, errDiag(DiagProjectMissing, t.Config.Dir+"/project.ts does not exist", "vybava ui-loop init"))
	}
	switch {
	case skipTS:
		data.Manifest.Reason, data.AppMap.Reason = "--no-ts", "--no-ts"
	case !vendor.Clean || !data.Project:
		data.Manifest.Reason, data.AppMap.Reason = "vendor or project.ts not ready", "vendor or project.ts not ready"
	default:
		t.checkManifest(&data, &res)
		t.checkAppMap(&data, &res)
	}
	data.DevboxSync = t.devboxSyncGaps()
	for _, g := range data.DevboxSync {
		res.Diagnostics = append(res.Diagnostics, syncGapDiag(g))
	}
	for _, d := range res.Diagnostics {
		if d.Fix != "" && d.Severity == "error" {
			res.Next = append(res.Next, d.Fix)
		}
	}
	if len(res.Next) == 0 {
		res.Next = []string{"vybava ui-loop run --print --json"}
	}
	return res, nil
}

func (t *Tool) checkManifest(data *CheckData, res *Result) {
	out, err := t.tsScript("check.ts")
	if err != nil {
		data.Manifest.Status, data.Manifest.Reason = "failed", err.Error()
		res.Diagnostics = append(res.Diagnostics, errDiag(DiagTSRunnerFailed, "check.ts: "+err.Error(), "set uiLoop.tsRunner to a runner the repo has"))
		return
	}
	var parsed struct {
		Screens  int      `json:"screens"`
		Captures int      `json:"captures"`
		Problems []string `json:"problems"`
	}
	if jerr := json.Unmarshal([]byte(lastLine(out.Stdout)), &parsed); jerr != nil || (out.Code != 0 && len(parsed.Problems) == 0) {
		data.Manifest.Status, data.Manifest.Reason = "failed", excerpt(out.Stderr+"\n"+out.Stdout)
		res.Diagnostics = append(res.Diagnostics, errDiag(DiagTSRunnerFailed, fmt.Sprintf("check.ts exited %d: %s", out.Code, excerpt(out.Stderr+"\n"+out.Stdout)),
			"fix the error above (project.ts or a screens file), or set uiLoop.tsRunner"))
		return
	}
	data.Manifest.Screens, data.Manifest.Captures = parsed.Screens, parsed.Captures
	if parsed.Problems == nil {
		parsed.Problems = []string{}
	}
	data.Manifest.Problems = parsed.Problems
	data.Manifest.Status = "ok"
	if len(parsed.Problems) > 0 {
		data.Manifest.Status = "problems"
		res.Diagnostics = append(res.Diagnostics, errDiag(DiagManifestInvalid, fmt.Sprintf("%d manifest problems: %s", len(parsed.Problems), strings.Join(parsed.Problems, "; ")),
			"fix the screens under "+t.Config.Dir+"/screens"))
	}
}

func (t *Tool) checkAppMap(data *CheckData, res *Result) {
	out, err := t.tsScript("render-app-map.ts", "--check")
	switch {
	case err != nil:
		data.AppMap.Status, data.AppMap.Reason = "failed", err.Error()
		res.Diagnostics = append(res.Diagnostics, errDiag(DiagTSRunnerFailed, "render-app-map.ts: "+err.Error(), "set uiLoop.tsRunner to a runner the repo has"))
	case out.Code == 0:
		data.AppMap.Status = "ok"
	case strings.Contains(out.Stderr, "is stale"):
		data.AppMap.Status = "stale"
		res.Diagnostics = append(res.Diagnostics, errDiag(DiagAppMapStale, t.Config.AppMap+" differs from the manifest", "vybava ui-loop map"))
	default:
		data.AppMap.Status, data.AppMap.Reason = "failed", excerpt(out.Stderr+"\n"+out.Stdout)
		res.Diagnostics = append(res.Diagnostics, errDiag(DiagTSRunnerFailed, fmt.Sprintf("render-app-map.ts exited %d: %s", out.Code, excerpt(out.Stderr+"\n"+out.Stdout)),
			"fix the error above, or set uiLoop.tsRunner"))
	}
}

// MapData is what map reports.
type MapData struct {
	File   string `json:"file"`
	Output string `json:"output"`
}

// Map renders the app map through the repo's tsRunner.
func (t *Tool) Map() (Result, error) {
	vendor, err := t.Vendor()
	if err != nil {
		return Result{}, err
	}
	if !vendor.Clean {
		return Result{Data: vendor}, diag(DiagVendorDrift, "vendor differs from this vybava's harness", "vybava ui-loop sync")
	}
	out, err := t.tsScript("render-app-map.ts")
	if err != nil {
		return Result{}, diag(DiagTSRunnerFailed, err.Error(), "set uiLoop.tsRunner to a runner the repo has")
	}
	if out.Code != 0 {
		return Result{}, diag(DiagTSRunnerFailed, fmt.Sprintf("render-app-map.ts exited %d: %s", out.Code, excerpt(out.Stderr+"\n"+out.Stdout)), "fix the error above")
	}
	return Result{Data: MapData{File: t.Config.AppMap, Output: lastLine(out.Stdout)}, Next: []string{"vybava ui-loop check --json"}}, nil
}
