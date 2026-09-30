package polishkit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
)

// RunVersion is run.json's version. Bump it on a breaking change of RunFile
// or Cell; a pass written by another version answers `run-version` and is
// recreated with `run init --force`.
const RunVersion = 1

// Cell kinds and verdicts.
const (
	CellChrome = "chrome"
	CellMatrix = "matrix"

	VerdictPending = "pending"
	VerdictPass    = "pass"
	VerdictFail    = "fail"
	VerdictSkip    = "skip"
)

// Verdicts is the closed verdict vocabulary a cell can be set to.
var Verdicts = []string{VerdictPass, VerdictFail, VerdictSkip}

// RunFile is <out>/pass-<n>/run.json: the plan snapshot, the lanes and
// screens in scope and the cell table the pass fills.
type RunFile struct {
	V         int      `json:"v"`
	Pass      int      `json:"pass"`
	PassDir   string   `json:"passDir"`
	CreatedAt string   `json:"createdAt"`
	Vybava    string   `json:"vybava"`
	Plan      PlanData `json:"plan"`
	Lanes     []Lane   `json:"lanes"`
	Screens   []Screen `json:"screens"`
	Cells     []Cell   `json:"cells"`
}

// Cell is one judgement the pass owes: a chrome cell (lane x screen x
// theme x nav x text size, shot by `shoot`) or a matrix cell (lane x flow x
// adverse-condition tier, added by `run add-cell`). The verdict is always
// the agent's; the applet only records it.
type Cell struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Lane     string `json:"lane"`
	Screen   string `json:"screen,omitempty"`
	Flow     string `json:"flow,omitempty"`
	Tier     string `json:"tier,omitempty"`
	Theme    string `json:"theme,omitempty"`
	Nav      string `json:"nav,omitempty"`
	TextSize string `json:"textSize,omitempty"`
	Verdict  string `json:"verdict"`
	// Shot is pass-dir-relative when the file lives under it, else absolute.
	Shot    string `json:"shot,omitempty"`
	Note    string `json:"note,omitempty"`
	Finding string `json:"finding,omitempty"`
}

// InitOptions are run init's flags.
type InitOptions struct {
	Pass      int
	Plan      PlanOptions
	Lanes     []string
	Screens   []string
	Force     bool
	Intensity string
}

// InitData is what run init reports.
type InitData struct {
	Pass     int      `json:"pass"`
	PassDir  string   `json:"passDir"`
	RunFile  string   `json:"runFile"`
	Existing bool     `json:"existing"`
	Cells    int      `json:"cells"`
	Lanes    []string `json:"lanes"`
	Screens  []string `json:"screens"`
}

var passDirRe = regexp.MustCompile(`^pass-(\d+)$`)

// Passes lists the pass numbers under <out>, ascending.
func (t *Tool) Passes() []int {
	entries, err := os.ReadDir(t.OutDir())
	if err != nil {
		return nil
	}
	var passes []int
	for _, e := range entries {
		if m := passDirRe.FindStringSubmatch(e.Name()); m != nil && e.IsDir() {
			n, _ := strconv.Atoi(m[1])
			passes = append(passes, n)
		}
	}
	sort.Ints(passes)
	return passes
}

func (t *Tool) latestPass() int {
	passes := t.Passes()
	if len(passes) == 0 {
		return 0
	}
	return passes[len(passes)-1]
}

func (t *Tool) nextPass() int { return t.latestPass() + 1 }

// PassDir is <out>/pass-<n>, absolute.
func (t *Tool) PassDir(pass int) string { return filepath.Join(t.OutDir(), "pass-"+strconv.Itoa(pass)) }

// resolvePass: 0 means the latest; a pass with no run.json is pass-missing.
func (t *Tool) resolvePass(pass int) (int, error) {
	if pass < 0 {
		return 0, diag(DiagUsage, "--pass must be a positive pass number", "polish-kit status --json")
	}
	if pass == 0 {
		pass = t.latestPass()
		if pass == 0 {
			return 0, diag(DiagPassMissing, "no pass under "+t.Config.Out, fmt.Sprintf("polish-kit run init --pass 1 --json"))
		}
	}
	if _, err := os.Stat(filepath.Join(t.PassDir(pass), "run.json")); err != nil {
		return 0, diag(DiagPassMissing, fmt.Sprintf("%s has no run.json", t.PassDir(pass)), fmt.Sprintf("polish-kit run init --pass %d --json", pass))
	}
	return pass, nil
}

// LoadRun reads a pass's run.json (0 = the latest).
func (t *Tool) LoadRun(pass int) (*RunFile, error) {
	pass, err := t.resolvePass(pass)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(t.PassDir(pass), "run.json"))
	if err != nil {
		return nil, err
	}
	var probe struct {
		V int `json:"v"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, diag(DiagRunVersion, fmt.Sprintf("pass %d run.json does not parse: %v", pass, err), fmt.Sprintf("polish-kit run init --pass %d --force --json", pass))
	}
	if probe.V != RunVersion {
		return nil, diag(DiagRunVersion, fmt.Sprintf("pass %d run.json is v%d, this polish-kit writes v%d", pass, probe.V, RunVersion), fmt.Sprintf("polish-kit run init --pass %d --force --json", pass))
	}
	var run RunFile
	if err := json.Unmarshal(b, &run); err != nil {
		return nil, diag(DiagRunVersion, fmt.Sprintf("pass %d run.json does not decode: %v", pass, err), fmt.Sprintf("polish-kit run init --pass %d --force --json", pass))
	}
	run.Pass = pass
	run.PassDir = t.PassDir(pass)
	return &run, nil
}

// SaveRun writes run.json atomically (temp file + rename).
func (t *Tool) SaveRun(run *RunFile) error {
	if err := os.MkdirAll(run.PassDir, 0o755); err != nil {
		return err
	}
	if run.Cells == nil {
		run.Cells = []Cell{}
	}
	b, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return err
	}
	final := filepath.Join(run.PassDir, "run.json")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// Init writes <out>/pass-<n>/run.json with the plan snapshot and the chrome
// cell table. An existing pass is returned as is unless Force.
func (t *Tool) Init(ctx context.Context, opts InitOptions) (Result, error) {
	pass := opts.Pass
	if pass < 0 {
		return Result{}, diag(DiagUsage, "--pass must be a positive pass number", "polish-kit run init --pass 1 --json")
	}
	if pass == 0 {
		pass = t.nextPass()
	}
	runFile := filepath.Join(t.PassDir(pass), "run.json")
	if _, err := os.Stat(runFile); err == nil && !opts.Force {
		run, err := t.LoadRun(pass)
		if err != nil {
			return Result{}, err
		}
		data := initData(run, true)
		return Result{Data: data, Lines: initLines(data), Next: t.initNext(run)}, nil
	}
	planRes, err := t.Plan(ctx, opts.Plan)
	if err != nil {
		return Result{}, err
	}
	plan := planRes.Data.(PlanData)
	lanes, err := t.selectLanes(plan, opts.Lanes)
	if err != nil {
		return Result{}, err
	}
	screens, err := t.selectScreens(plan, opts.Screens)
	if err != nil {
		return Result{}, err
	}
	run := &RunFile{
		V: RunVersion, Pass: pass, PassDir: t.PassDir(pass), CreatedAt: t.Now().UTC().Format("2006-01-02T15:04:05Z"),
		Vybava: t.Version, Plan: plan, Lanes: lanes, Screens: screens, Cells: ChromeCells(lanes, screens),
	}
	if opts.Force {
		// A recreated pass keeps its shots; the cell table starts pending.
		_ = os.Remove(runFile)
	}
	if err := t.ensureOut(); err != nil {
		return Result{}, err
	}
	if err := t.SaveRun(run); err != nil {
		return Result{}, err
	}
	data := initData(run, false)
	return Result{Data: data, Lines: initLines(data), Next: t.initNext(run)}, nil
}

// ensureOut creates the run root with a self-ignoring .gitignore.
func (t *Tool) ensureOut() error {
	if err := os.MkdirAll(t.OutDir(), 0o755); err != nil {
		return err
	}
	ignore := filepath.Join(t.OutDir(), ".gitignore")
	if _, err := os.Stat(ignore); errors.Is(err, os.ErrNotExist) {
		return os.WriteFile(ignore, []byte("*\n"), 0o644)
	}
	return nil
}

func (t *Tool) selectLanes(plan PlanData, ids []string) ([]Lane, error) {
	var lanes []Lane
	if len(ids) == 0 {
		for _, id := range plan.Lanes {
			l, _ := t.Config.Lane(id)
			lanes = append(lanes, l)
		}
		return lanes, nil
	}
	for _, id := range ids {
		l, ok := t.Config.Lane(id)
		if !ok {
			return nil, diag(DiagUnknownLane, fmt.Sprintf("lane %q is not declared (lanes: %s)", id, strings.Join(t.Config.LaneIDs(nil), ", ")), "polish-kit lanes --json")
		}
		lanes = append(lanes, l)
	}
	return lanes, nil
}

func (t *Tool) selectScreens(plan PlanData, ids []string) ([]Screen, error) {
	var screens []Screen
	if len(ids) == 0 {
		ids = plan.ScreensTouched
	}
	for _, id := range ids {
		s, ok := t.Config.Screen(id)
		if !ok {
			return nil, diag(DiagUnknownScreen, fmt.Sprintf("screen %q is not declared", id), "polish-kit run init --pass "+strconv.Itoa(t.nextPass())+" --json")
		}
		screens = append(screens, s)
	}
	return screens, nil
}

// ChromeCells is the cell table: lanes x screens of the lane's target x
// themes x nav (android) x text sizes (default first), in that order.
func ChromeCells(lanes []Lane, screens []Screen) []Cell {
	cells := []Cell{}
	for _, l := range lanes {
		navs := []string{""}
		if l.Kind.IsAndroid() {
			navs = l.Nav
		}
		sizes := append([]string{""}, l.TextSizes...)
		for _, s := range screens {
			if s.Target != l.Target {
				continue
			}
			for _, theme := range l.Themes {
				for _, nav := range navs {
					for _, size := range sizes {
						c := Cell{Kind: CellChrome, Lane: l.ID, Screen: s.ID, Theme: theme, Nav: nav, TextSize: size, Verdict: VerdictPending}
						c.ID = c.chromeID()
						cells = append(cells, c)
					}
				}
			}
		}
	}
	return cells
}

// chromeID: <lane>--<screen>--<theme>[--<nav>][--<text>].
func (c Cell) chromeID() string {
	parts := []string{c.Lane, c.Screen, c.Theme}
	if c.Nav != "" {
		parts = append(parts, c.Nav)
	}
	if c.TextSize != "" {
		parts = append(parts, c.TextSize)
	}
	return strings.Join(parts, "--")
}

// ShotFile is the shot path a chrome cell owns, pass-dir-relative.
func (c Cell) ShotFile() string {
	name := c.Screen + "--" + c.Theme
	if c.Nav != "" {
		name += "--" + c.Nav
	}
	if c.TextSize != "" {
		name += "--" + c.TextSize
	}
	return filepath.ToSlash(filepath.Join("shots", c.Lane, name+".png"))
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

// Slug lower-cases and kebabs a title for an id.
func Slug(s string) string {
	return strings.Trim(slugRe.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// AddCellOptions are run add-cell's flags.
type AddCellOptions struct {
	Pass int
	Kind string
	Lane string
	Flow string
	Tier string
}

// AddCell appends a matrix cell (idempotent by id).
func (t *Tool) AddCell(opts AddCellOptions) (Result, error) {
	if opts.Kind != CellMatrix {
		return Result{}, diag(DiagUsage, "add-cell takes --kind matrix (chrome cells come from run init)", "polish-kit run add-cell --kind matrix --lane <id> --flow \"<title>\" --tier \"<tier>\" --json")
	}
	if strings.TrimSpace(opts.Flow) == "" || strings.TrimSpace(opts.Tier) == "" {
		return Result{}, diag(DiagUsage, "add-cell needs --flow and --tier", "polish-kit run add-cell --kind matrix --lane "+opts.Lane+" --flow \"<title>\" --tier \"<tier>\" --json")
	}
	run, err := t.LoadRun(opts.Pass)
	if err != nil {
		return Result{}, err
	}
	if _, ok := t.Config.Lane(opts.Lane); !ok {
		return Result{}, diag(DiagUnknownLane, fmt.Sprintf("lane %q is not declared (lanes: %s)", opts.Lane, strings.Join(t.Config.LaneIDs(nil), ", ")), "polish-kit lanes --json")
	}
	cell := Cell{Kind: CellMatrix, Lane: opts.Lane, Flow: opts.Flow, Tier: opts.Tier, Verdict: VerdictPending}
	cell.ID = strings.Join([]string{opts.Lane, "matrix", Slug(opts.Flow), Slug(opts.Tier)}, "--")
	existing := slices.IndexFunc(run.Cells, func(c Cell) bool { return c.ID == cell.ID })
	if existing >= 0 {
		cell = run.Cells[existing]
	} else {
		run.Cells = append(run.Cells, cell)
		if err := t.SaveRun(run); err != nil {
			return Result{}, err
		}
	}
	return Result{Data: cell, Lines: []string{cell.ID + "  " + cell.Verdict}, Next: []string{
		fmt.Sprintf("polish-kit cell %s pass --pass %d --json", cell.ID, run.Pass),
		fmt.Sprintf("polish-kit cell %s fail --shot <path> --note \"<what>\" --pass %d --json", cell.ID, run.Pass),
	}}, nil
}

// CellOptions are the cell verb's flags.
type CellOptions struct {
	Pass    int
	ID      string
	Verdict string
	Shot    string
	Note    string
	Finding string
}

// SetCell records a verdict; fail needs a shot.
func (t *Tool) SetCell(opts CellOptions) (Result, error) {
	if !slices.Contains(Verdicts, opts.Verdict) {
		return Result{}, diag(DiagUsage, fmt.Sprintf("verdict %q is not pass, fail or skip", opts.Verdict), "polish-kit cell "+opts.ID+" pass --json")
	}
	run, err := t.LoadRun(opts.Pass)
	if err != nil {
		return Result{}, err
	}
	i := slices.IndexFunc(run.Cells, func(c Cell) bool { return c.ID == opts.ID })
	if i < 0 {
		return Result{}, diag(DiagCellUnknown, fmt.Sprintf("pass %d has no cell %q", run.Pass, opts.ID), fmt.Sprintf("polish-kit status --pass %d --json", run.Pass))
	}
	cell := &run.Cells[i]
	if opts.Shot != "" {
		abs := opts.Shot
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(t.Cwd, abs)
		}
		if _, err := os.Stat(abs); err != nil {
			return Result{}, diag(DiagShotRequired, fmt.Sprintf("--shot %s does not exist", opts.Shot), fmt.Sprintf("polish-kit cell %s %s --shot <existing file> --json", cell.ID, opts.Verdict))
		}
		cell.Shot = t.passRel(run, abs)
	}
	if opts.Verdict == VerdictFail && cell.Shot == "" {
		return Result{}, diag(DiagShotRequired, "a fail verdict needs the screenshot that shows it", fmt.Sprintf("polish-kit cell %s fail --shot <path> --note \"<what>\" --json", cell.ID))
	}
	cell.Verdict = opts.Verdict
	if opts.Note != "" {
		cell.Note = opts.Note
	}
	if opts.Finding != "" {
		cell.Finding = opts.Finding
	}
	if err := t.SaveRun(run); err != nil {
		return Result{}, err
	}
	next := []string{fmt.Sprintf("polish-kit status --pass %d --json", run.Pass)}
	if pending := pendingCells(run); len(pending) > 0 {
		next = append([]string{cellCommand(pending[0], run.Pass)}, next...)
	} else {
		next = append(next, fmt.Sprintf("polish-kit report --pass %d --json", run.Pass))
	}
	return Result{Data: *cell, Lines: []string{cell.ID + "  " + cell.Verdict}, Next: next}, nil
}

// passRel makes a shot path pass-dir-relative when it lives under it.
func (t *Tool) passRel(run *RunFile, abs string) string {
	if rel, err := filepath.Rel(run.PassDir, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return abs
}

// ShotPath resolves a cell's shot to an absolute path.
func (run *RunFile) ShotPath(c Cell) string {
	if c.Shot == "" {
		return ""
	}
	if filepath.IsAbs(c.Shot) {
		return c.Shot
	}
	return filepath.Join(run.PassDir, filepath.FromSlash(c.Shot))
}

func pendingCells(run *RunFile) []Cell {
	var out []Cell
	for _, c := range run.Cells {
		if c.Verdict == VerdictPending {
			out = append(out, c)
		}
	}
	return out
}

func cellCommand(c Cell, pass int) string {
	if c.Shot != "" {
		return fmt.Sprintf("polish-kit cell %s pass|fail --pass %d --json  # shot %s", c.ID, pass, c.Shot)
	}
	return fmt.Sprintf("polish-kit cell %s pass|fail --shot <path> --pass %d --json", c.ID, pass)
}

// StatusData is the status verb's payload.
type StatusData struct {
	Pass    int                       `json:"pass"`
	Total   int                       `json:"total"`
	Verdict map[string]int            `json:"verdict"`
	Lanes   map[string]map[string]int `json:"lanes"`
	Kinds   map[string]map[string]int `json:"kinds"`
	Pending []string                  `json:"pending"`
}

// Status counts a pass's cells and lists the pending ones.
func (t *Tool) Status(pass int) (Result, error) {
	run, err := t.LoadRun(pass)
	if err != nil {
		return Result{}, err
	}
	data := StatusData{Pass: run.Pass, Total: len(run.Cells), Verdict: map[string]int{}, Lanes: map[string]map[string]int{}, Kinds: map[string]map[string]int{}, Pending: []string{}}
	for _, v := range append([]string{VerdictPending}, Verdicts...) {
		data.Verdict[v] = 0
	}
	var next []string
	for _, c := range run.Cells {
		data.Verdict[c.Verdict]++
		bump(data.Lanes, c.Lane, c.Verdict)
		bump(data.Kinds, c.Kind, c.Verdict)
		if c.Verdict == VerdictPending {
			data.Pending = append(data.Pending, c.ID)
			next = append(next, cellCommand(c, run.Pass))
		}
	}
	if len(next) == 0 {
		next = []string{fmt.Sprintf("polish-kit report --pass %d --json", run.Pass)}
	}
	rows := [][]string{{"LANE", "PENDING", "PASS", "FAIL", "SKIP"}}
	for _, id := range sortedKeys(data.Lanes) {
		v := data.Lanes[id]
		rows = append(rows, []string{id, itoa(v[VerdictPending]), itoa(v[VerdictPass]), itoa(v[VerdictFail]), itoa(v[VerdictSkip])})
	}
	lines := append([]string{fmt.Sprintf("pass %d: %d cells, %d pending, %d pass, %d fail, %d skip", run.Pass, data.Total, data.Verdict[VerdictPending], data.Verdict[VerdictPass], data.Verdict[VerdictFail], data.Verdict[VerdictSkip])}, table(rows)...)
	return Result{Data: data, Lines: lines, Next: next}, nil
}

func bump(m map[string]map[string]int, key, verdict string) {
	if m[key] == nil {
		m[key] = map[string]int{VerdictPending: 0, VerdictPass: 0, VerdictFail: 0, VerdictSkip: 0}
	}
	m[key][verdict]++
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func initData(run *RunFile, existing bool) InitData {
	d := InitData{Pass: run.Pass, PassDir: run.PassDir, RunFile: filepath.Join(run.PassDir, "run.json"), Existing: existing, Cells: len(run.Cells), Lanes: []string{}, Screens: []string{}}
	for _, l := range run.Lanes {
		d.Lanes = append(d.Lanes, l.ID)
	}
	for _, s := range run.Screens {
		d.Screens = append(d.Screens, s.ID)
	}
	return d
}

func initLines(d InitData) []string {
	state := "created"
	if d.Existing {
		state = "existing"
	}
	return []string{fmt.Sprintf("pass %d %s: %d cells, lanes %s, screens %s", d.Pass, state, d.Cells, strings.Join(d.Lanes, ","), strings.Join(d.Screens, ","))}
}

func (t *Tool) initNext(run *RunFile) []string {
	var next []string
	for _, l := range run.Lanes {
		if l.Kind.IsShootable() {
			next = append(next, fmt.Sprintf("polish-kit shoot %s --pass %d --json", l.ID, run.Pass))
		}
	}
	next = append(next, fmt.Sprintf("polish-kit run add-cell --kind matrix --lane %s --flow \"<title>\" --tier \"<tier>\" --pass %d --json", firstLane(run), run.Pass))
	next = append(next, fmt.Sprintf("polish-kit status --pass %d --json", run.Pass))
	return next
}

func firstLane(run *RunFile) string {
	if len(run.Lanes) == 0 {
		return "<lane>"
	}
	return run.Lanes[0].ID
}

// table renders aligned columns for a human.
func table(rows [][]string) []string {
	var sb strings.Builder
	w := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		fmt.Fprintln(w, strings.Join(r, "\t"))
	}
	_ = w.Flush()
	return strings.Split(strings.TrimRight(sb.String(), "\n"), "\n")
}
