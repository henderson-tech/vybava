package uiloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// RunVersion is run.json's version (harness/run.ts RUN_VERSION).
const RunVersion = 1

// RunFile is <passDir>/run.json (harness/run.ts RunFile): everything the
// capture needs, so a container with the repo but no vybava can run it.
type RunFile struct {
	V         int                 `json:"v"`
	Pass      int                 `json:"pass"`
	PassDir   string              `json:"passDir"`
	Dir       string              `json:"dir"`
	AppMap    string              `json:"appMap"`
	Vybava    string              `json:"vybava"`
	CreatedAt string              `json:"createdAt"`
	Areas     []string            `json:"areas"`
	Apps      map[string]App      `json:"apps"`
	Viewports map[string]Viewport `json:"viewports"`
	Selection Selection           `json:"selection"`
	Lint      RunLint             `json:"lint"`
	BuildWait int                 `json:"buildWait"`
	Workers   int                 `json:"workers"`
}

// Selection narrows a run; empty lists mean "everything".
type Selection struct {
	Apps        []string `json:"apps"`
	Only        []string `json:"only"`
	Viewports   []string `json:"viewports"`
	Themes      []string `json:"themes"`
	Destructive bool     `json:"destructive"`
	Resume      bool     `json:"resume"`
}

// RunLint is the lint config with defaults filled.
type RunLint struct {
	Grid        int       `json:"grid"`
	TouchTarget int       `json:"touchTarget"`
	Off         []string  `json:"off"`
	Ramp        []float64 `json:"ramp"`
	// Allow: rule id → selectors whose hits count as info (never nil).
	Allow map[string][]string `json:"allow"`
}

// RunOptions are the run verb's flags.
type RunOptions struct {
	Selection Selection
	// Pass: 0 = the next pass (the latest with Resume).
	Pass      int
	Workers   int
	BuildWait int
	// Print only prints the command.
	Print bool
	// Wrap runs the command through another, e.g. "devbox run -- {cmd}";
	// {cmd} is replaced by the single-quoted command.
	Wrap string
}

// RunData is what run reports.
type RunData struct {
	Pass     int            `json:"pass"`
	PassDir  string         `json:"passDir"`
	RunFile  string         `json:"runFile"`
	Command  string         `json:"command"`
	Executed bool           `json:"executed"`
	ExitCode int            `json:"exitCode,omitempty"`
	Shots    int            `json:"shots,omitempty"`
	ByStatus map[string]int `json:"byStatus,omitempty"`
}

var passDirRe = regexp.MustCompile(`^pass-(\d+)$`)

// Passes lists the pass numbers under <out>, ascending.
func (t *Tool) Passes() ([]int, error) {
	entries, err := os.ReadDir(t.abs(t.Config.Out))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var passes []int
	for _, e := range entries {
		if m := passDirRe.FindStringSubmatch(e.Name()); m != nil && e.IsDir() {
			n, _ := strconv.Atoi(m[1])
			passes = append(passes, n)
		}
	}
	sort.Ints(passes)
	return passes, nil
}

// PassDir is the repo-relative <out>/pass-<n>.
func (t *Tool) PassDir(n int) string { return path.Join(t.Config.Out, fmt.Sprintf("pass-%d", n)) }

// CheckPassFlag refuses an explicit --pass below 1. Passes are numbered from
// 1 (pass-<n>) and 0 is the options' "not given", so `--pass 0` used to fall
// through to the next (or latest) pass without a word.
func CheckPassFlag(n int, given bool) error {
	if given && n < 1 {
		return diag(DiagSelectionInvalid, fmt.Sprintf("--pass %d names no pass: passes are numbered from 1", n),
			"omit --pass for the next pass (the latest with --resume, split, publish and scoreboard)")
	}
	return nil
}

// ResolvePass picks a pass: explicit n, else the latest (latest=true) or the next.
func (t *Tool) ResolvePass(n int, latest bool) (int, error) {
	if n < 0 {
		return 0, diag(DiagSelectionInvalid, "--pass must be positive", "")
	}
	if n > 0 {
		return n, nil
	}
	passes, err := t.Passes()
	if err != nil {
		return 0, err
	}
	if len(passes) == 0 {
		if latest {
			return 0, diag(DiagPassMissing, "no pass under "+t.Config.Out, "vybava ui-loop run")
		}
		return 1, nil
	}
	last := passes[len(passes)-1]
	if latest {
		return last, nil
	}
	// A pass that holds no shots yet (a --print whose command never ran, or
	// one killed before its first shot) is reused, never skipped.
	if !t.hasShots(last) {
		return last, nil
	}
	return last + 1, nil
}

func (t *Tool) hasShots(n int) bool {
	_, err := os.Stat(filepath.Join(t.passAbs(n), "shots"))
	return err == nil
}

// resolveShotPass picks an explicit pass, else the latest that holds shots.
func (t *Tool) resolveShotPass(n int) (int, error) {
	if n != 0 {
		return t.ResolvePass(n, true)
	}
	passes, err := t.Passes()
	if err != nil {
		return 0, err
	}
	for i := len(passes) - 1; i >= 0; i-- {
		if t.hasShots(passes[i]) {
			return passes[i], nil
		}
	}
	return 0, diag(DiagPassMissing, "no pass under "+t.Config.Out+" holds shots", "vybava ui-loop run")
}

func (t *Tool) validateSelection(s Selection) error {
	var problems []string
	for _, a := range s.Apps {
		if _, ok := t.Config.Apps[a]; !ok {
			problems = append(problems, fmt.Sprintf("unknown app %q (%s)", a, strings.Join(t.Config.AppNames(), ", ")))
		}
	}
	known := t.Config.ResolvedViewports()
	for _, v := range s.Viewports {
		if _, ok := known[v]; !ok {
			problems = append(problems, fmt.Sprintf("unknown viewport %q", v))
		}
	}
	for _, th := range s.Themes {
		if th != "light" && th != "dark" {
			problems = append(problems, fmt.Sprintf("theme %q is not light or dark", th))
		}
	}
	if len(problems) > 0 {
		return diag(DiagSelectionInvalid, strings.Join(problems, "; "), "")
	}
	return nil
}

// shellQuote single-quotes s for sh.
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// CaptureCommand is the shell command that runs one pass from the repo root.
// "$PWD" expands where it runs — the Mac or a container with the repo at
// another path — so the same line works in both.
func (t *Tool) CaptureCommand(passDir string) string {
	return fmt.Sprintf(`UILOOP_ROOT="$PWD" UILOOP_RUN="$PWD/%s/run.json" %s -c "$PWD/%s/vendor/playwright.config.ts"`,
		passDir, strings.TrimSpace(t.Config.Runner), t.Config.Dir)
}

// Run writes the pass's run.json and runs (or prints) the capture.
func (t *Tool) Run(ctx context.Context, o RunOptions) (Result, error) {
	if err := t.validateSelection(o.Selection); err != nil {
		return Result{}, err
	}
	vendor, err := t.Vendor()
	if err != nil {
		return Result{}, err
	}
	if !vendor.Clean {
		return Result{Data: vendor}, diag(DiagVendorDrift, "vendor differs from this vybava's harness — a pass must run the harness this binary describes", "vybava ui-loop sync")
	}
	if ok, err := t.projectExists(); err != nil {
		return Result{}, err
	} else if !ok {
		return Result{}, diag(DiagProjectMissing, t.Config.Dir+"/project.ts does not exist", "vybava ui-loop init")
	}
	pass, err := t.ResolvePass(o.Pass, o.Selection.Resume)
	if err != nil {
		return Result{}, err
	}
	passDir := t.PassDir(pass)
	if o.Selection.Resume {
		if _, err := os.Stat(t.abs(passDir)); err != nil {
			return Result{}, diag(DiagPassMissing, passDir+" does not exist — nothing to resume", "drop --resume")
		}
	}
	if o.Workers <= 0 {
		o.Workers = 2
	}
	if o.BuildWait <= 0 {
		o.BuildWait = 300
	}
	c := t.Config
	apps := map[string]App{}
	for name, app := range c.Apps {
		if app.Env != "" {
			if v := os.Getenv(app.Env); v != "" {
				app.BaseURL = v
			}
		}
		apps[name] = app
	}
	sel := o.Selection
	for _, list := range []*[]string{&sel.Apps, &sel.Only, &sel.Viewports, &sel.Themes} {
		if *list == nil {
			*list = []string{}
		}
	}
	off := c.Lint.Off
	if off == nil {
		off = []string{}
	}
	ramp := c.Lint.Ramp
	if ramp == nil {
		ramp = []float64{}
	}
	allow := c.Lint.Allow
	if allow == nil {
		allow = map[string][]string{}
	}
	run := RunFile{
		V: RunVersion, Pass: pass, PassDir: passDir, Dir: c.Dir, AppMap: c.AppMap, Vybava: t.Version,
		CreatedAt: t.Now().UTC().Format("2006-01-02T15:04:05Z"),
		Areas:     c.Areas, Apps: apps, Viewports: c.ResolvedViewports(), Selection: sel,
		Lint:      RunLint{Grid: c.Lint.Grid, TouchTarget: c.Lint.TouchTarget, Off: off, Ramp: ramp, Allow: allow},
		BuildWait: o.BuildWait, Workers: o.Workers,
	}
	runFile := path.Join(passDir, "run.json")
	if err := os.MkdirAll(t.abs(passDir), 0o755); err != nil {
		return Result{}, err
	}
	b, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(t.abs(runFile), append(b, '\n'), 0o644); err != nil {
		return Result{}, err
	}
	command := t.CaptureCommand(passDir)
	if o.Wrap != "" {
		if !strings.Contains(o.Wrap, "{cmd}") {
			return Result{}, diag(DiagSelectionInvalid, "--wrap must contain {cmd}", `--wrap "devbox run -- {cmd}"`)
		}
		command = strings.ReplaceAll(o.Wrap, "{cmd}", shellQuote(command))
	}
	data := &RunData{Pass: pass, PassDir: passDir, RunFile: runFile, Command: command}
	res := Result{Data: data}
	if o.Print {
		res.Next = []string{
			"run it from " + t.Root + ": " + command,
			"on a Devbox: devbox run -- " + shellQuote(t.CaptureCommand(passDir)),
		}
		return res, nil
	}
	out, err := t.Exec(ctx, Cmd{Dir: t.Root, Args: []string{"sh", "-c", command}, Stream: t.Log})
	if err != nil {
		return res, err
	}
	data.Executed, data.ExitCode = true, out.Code
	records, err := LoadRecords(t.abs(passDir))
	if err != nil {
		return res, err
	}
	data.Shots, data.ByStatus = len(records), map[string]int{}
	for _, r := range records {
		data.ByStatus[r.Status]++
	}
	if notOK := len(records) - data.ByStatus["ok"]; notOK > 0 {
		res.Diagnostics = append(res.Diagnostics, info(DiagShotsNotOk, fmt.Sprintf("%d of %d shots are not ok (see %s/report.md)", notOK, len(records), passDir), ""))
	}
	next := fmt.Sprintf("vybava ui-loop split --pass %d --json", pass)
	if out.Code != 0 {
		res.Diagnostics = append(res.Diagnostics, errDiag(DiagRunFailed, fmt.Sprintf("the capture exited %d (harness errors; recipe failures are results)", out.Code),
			fmt.Sprintf("vybava ui-loop run --resume --pass %d", pass)))
		res.Next = []string{fmt.Sprintf("vybava ui-loop run --resume --pass %d", pass), next}
		return res, nil
	}
	res.Next = []string{next, fmt.Sprintf("vybava ui-loop scoreboard --pass %d --json", pass)}
	return res, nil
}

// SplitList parses a comma-separated flag.
func SplitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" && !slices.Contains(out, part) {
			out = append(out, part)
		}
	}
	return out
}

// abs path of a pass directory.
func (t *Tool) passAbs(n int) string { return filepath.Join(t.Root, filepath.FromSlash(t.PassDir(n))) }
