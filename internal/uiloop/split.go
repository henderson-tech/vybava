package uiloop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// PlanFile is one capture to adopt into its area's set.
type PlanFile struct {
	// Path is relative to the pass directory.
	Path string `json:"path"`
	// Stamp is the record's capturedAt: a --resume retake keeps the path
	// but changes the stamp, so publish adopts the new image.
	Stamp string `json:"stamp"`
	Shot  string `json:"shot"`
	Full  bool   `json:"full"`
	Label string `json:"label"`
	Title string `json:"title"`
	Note  string `json:"note"`
	Route string `json:"route"`
	URL   string `json:"url,omitempty"`
	State string `json:"state"`
	// Viewport and Theme name the file's section (board capture --device and
	// the head of --state); Size is its CSS viewport and scale (--viewport).
	Viewport string   `json:"viewport"`
	Theme    string   `json:"theme"`
	Size     string   `json:"size"`
	Src      []string `json:"src"`
}

// BoardSection is one viewport × theme block of a set's board: the labels of its
// captures, in set order.
type BoardSection struct {
	Title    string   `json:"title"` // "<viewport> · <theme>"
	Viewport string   `json:"viewport"`
	Theme    string   `json:"theme"`
	Labels   []string `json:"labels"`
}

// Set is one vitrinka set, and so one board: every image capture of one area
// in one pass, grouped by viewport then theme (config order), then screen order.
type Set struct {
	Key      string         `json:"key"`
	Title    string         `json:"title"`
	Area     string         `json:"area"`
	Sections []BoardSection `json:"sections"`
	Files    []PlanFile     `json:"files"`
}

// Plan is split's deterministic output: the same pass always produces the
// same sets, keys and order.
type Plan struct {
	V       int      `json:"v"`
	Pass    int      `json:"pass"`
	PassDir string   `json:"passDir"`
	Project string   `json:"project"`
	Sets    []Set    `json:"sets"`
	Skipped []string `json:"skipped"`
	// Notes are the shots published as text, per area in config order.
	Notes []AreaNotes `json:"notes"`
}

// PlanVersion is plan.json's v: 2 is one set per area (1 chunked area ×
// viewport × theme).
const PlanVersion = 2

// imageStatuses are the shot statuses published as images. Every other one
// (recipe-failed, unreachable, error) is a note: a recipe-failed shot is a
// picture of wherever the recipe died, usually the same sign-in page.
var imageStatuses = []string{"ok", "theme-mismatch", "build-error"}

// ShotNote is a shot listed as text instead of uploaded.
type ShotNote struct {
	ID       string `json:"id"`
	Viewport string `json:"viewport"`
	Theme    string `json:"theme"`
	Status   string `json:"status"`
	Step     string `json:"step,omitempty"`
	Error    string `json:"error,omitempty"`
}

// AreaNotes are one area's text-only shots, in manifest order.
type AreaNotes struct {
	Area  string     `json:"area"`
	Shots []ShotNote `json:"shots"`
}

// SplitOptions narrow a split.
type SplitOptions struct {
	Pass  int
	Areas []string
}

const (
	// maxKey: vitrinka set keys cap at 64.
	maxKey   = 64
	maxLabel = 40
)

// setFileCap is MaxSetFiles; a test lowers it.
var setFileCap = MaxSetFiles

// areaKey is the set key of an area: ONE set, so one board, per area across
// every pass. A set key is a board in vitrinka; each pass adds its shots and
// its sections to the same board instead of minting a board per pass.
func areaKey(prefix string, area string) string {
	return fit(fmt.Sprintf("%s-%s", prefix, area), "", maxKey)
}

func digest(s string, n int) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])[:n]
}

// fit shortens s to max characters, keeping suffix whole and staying unique
// through a digest of the full string.
func fit(s, suffix string, max int) string {
	if len(s)+len(suffix) <= max {
		return s + suffix
	}
	tag := digest(s, 6)
	head := strings.TrimRight(s[:max-len(suffix)-len(tag)-1], "-")
	return head + "-" + tag + suffix
}

func orderOf(list []string, v string) int {
	if i := slices.Index(list, v); i >= 0 {
		return i
	}
	return len(list)
}

// note is a shot's caption: its status and lint verdict, worst first.
func note(pass int, r Record) string {
	prefix := fmt.Sprintf("pass %d · ", pass)
	if r.Status == "recipe-failed" && r.Failure != nil {
		step := r.Failure.Step
		if r.Failure.StepIndex != nil {
			step = fmt.Sprintf("step %d %s", *r.Failure.StepIndex, step)
		}
		return prefix + "RECIPE FAILED at " + step + " — " + r.Failure.Error
	}
	var parts []string
	if r.Status != "ok" {
		detail := r.Status
		if r.Failure != nil && r.Failure.Error != "" {
			detail += ": " + r.Failure.Error
		}
		parts = append(parts, detail)
	}
	if r.Lint != nil {
		rules := make([]string, 0, len(r.Lint.Defects))
		for rule := range r.Lint.Defects {
			rules = append(rules, rule)
		}
		sort.Slice(rules, func(i, j int) bool {
			a, b := r.Lint.Defects[rules[i]], r.Lint.Defects[rules[j]]
			return a > b || (a == b && rules[i] < rules[j])
		})
		for _, rule := range rules {
			parts = append(parts, fmt.Sprintf("%s %d", rule, r.Lint.Defects[rule]))
		}
	}
	if n := len(r.ConsoleErrors); n > 0 {
		parts = append(parts, fmt.Sprintf("console errors %d", n))
	}
	if len(parts) == 0 {
		return prefix + "clean"
	}
	return prefix + strings.Join(parts, " · ")
}

// Split plans the vitrinka sets of a pass: one per area, so one board per
// area per pass, holding every viewport × theme of it in sections. vitrinka
// syncs a set file by file (its per-file door), so a set is bounded only by
// ingest.MaxSetFiles; an area above that is refused (SET_TOO_LARGE). The plan
// is written to <passDir>/publish/plan.json.
func (t *Tool) Split(o SplitOptions) (Result, error) {
	pass, err := t.resolveShotPass(o.Pass)
	if err != nil {
		return Result{}, err
	}
	plan, diags, err := t.plan(pass, o.Areas)
	if err != nil {
		return Result{}, err
	}
	b, err := json.MarshalIndent(plan, "", "  ")
	if err != nil {
		return Result{}, err
	}
	file := filepath.Join(t.passAbs(pass), "publish", "plan.json")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(file, append(b, '\n'), 0o644); err != nil {
		return Result{}, err
	}
	return Result{Data: plan, Diagnostics: diags, Next: []string{fmt.Sprintf("vybava ui-loop publish --pass %d --json", pass)}}, nil
}

func (t *Tool) plan(pass int, areas []string) (Plan, []runxDiagnostic, error) {
	passDir := t.passAbs(pass)
	if _, err := os.Stat(passDir); err != nil {
		return Plan{}, nil, diag(DiagPassMissing, t.PassDir(pass)+" does not exist", "vybava ui-loop run")
	}
	records, err := LoadRecords(passDir)
	if err != nil {
		return Plan{}, nil, err
	}
	return t.planRecords(pass, records, areas)
}

// sortAreas orders area names as the config lists them, the rest by name.
func sortAreas(order, areas []string) {
	sort.Slice(areas, func(i, j int) bool {
		if x, y := orderOf(order, areas[i]), orderOf(order, areas[j]); x != y {
			return x < y
		}
		return areas[i] < areas[j]
	})
}

// planRecords plans the given records of a pass (publish --follow passes
// only the final ones).
func (t *Tool) planRecords(pass int, records []Record, areas []string) (Plan, []runxDiagnostic, error) {
	c := t.Config
	passDir := t.passAbs(pass)
	if len(records) == 0 {
		return Plan{}, nil, diag(DiagPassMissing, t.PassDir(pass)+" holds no shot records", fmt.Sprintf("vybava ui-loop run --resume --pass %d", pass))
	}
	plan := Plan{V: PlanVersion, Pass: pass, PassDir: t.PassDir(pass), Project: c.Vitrinka.Project, Sets: []Set{}, Skipped: []string{}, Notes: []AreaNotes{}}
	diags := c.Deprecations()

	byArea := map[string][]Record{}
	notes := map[string][]ShotNote{}
	for _, r := range records {
		if len(areas) > 0 && !slices.Contains(areas, r.Area) {
			continue
		}
		if !slices.Contains(imageStatuses, r.Status) {
			n := ShotNote{ID: r.ID, Viewport: r.Viewport, Theme: r.Theme, Status: r.Status}
			if r.Failure != nil {
				n.Step, n.Error = r.Failure.Step, r.Failure.Error
				if r.Failure.StepIndex != nil && n.Step != "" {
					n.Step = fmt.Sprintf("step %d %s", *r.Failure.StepIndex, n.Step)
				}
			}
			notes[r.Area] = append(notes[r.Area], n)
			continue
		}
		if r.Files.Viewport == "" {
			plan.Skipped = append(plan.Skipped, r.Key()+" ("+r.Status+")")
			continue
		}
		byArea[r.Area] = append(byArea[r.Area], r)
	}
	planned := make([]string, 0, len(byArea))
	for a := range byArea {
		planned = append(planned, a)
	}
	sortAreas(c.Areas, planned)
	vpOrder, themeOrder := c.ViewportOrder(), c.ThemeOrder()
	for _, area := range planned {
		// Viewport, then theme, in config order; the stable sort keeps the
		// records' screen order inside each section.
		rs := byArea[area]
		sort.SliceStable(rs, func(i, j int) bool {
			a, b := rs[i], rs[j]
			if x, y := orderOf(vpOrder, a.Viewport), orderOf(vpOrder, b.Viewport); x != y {
				return x < y
			}
			if a.Viewport != b.Viewport {
				return a.Viewport < b.Viewport
			}
			if x, y := orderOf(themeOrder, a.Theme), orderOf(themeOrder, b.Theme); x != y {
				return x < y
			}
			return a.Theme < b.Theme
		})
		s := Set{
			Key:   areaKey(c.Vitrinka.BoardPrefix, area),
			Title: fmt.Sprintf("%s · %s", c.Vitrinka.BoardPrefix, area),
			Area:  area, Sections: []BoardSection{}, Files: []PlanFile{},
		}
		for _, r := range rs {
			unit, err := t.planFiles(pass, passDir, r)
			if err != nil {
				return Plan{}, nil, err
			}
			if n := len(s.Sections); n == 0 || s.Sections[n-1].Viewport != r.Viewport || s.Sections[n-1].Theme != r.Theme {
				s.Sections = append(s.Sections, BoardSection{Title: fmt.Sprintf("Pass %d · %s · %s", pass, r.Viewport, r.Theme), Viewport: r.Viewport, Theme: r.Theme, Labels: []string{}})
			}
			sec := &s.Sections[len(s.Sections)-1]
			for _, f := range unit {
				sec.Labels = append(sec.Labels, f.Label)
			}
			s.Files = append(s.Files, unit...)
		}
		// The root is shared by every pass, so the files other passes adopted
		// into it count too; +1: the set's manifest.json counts toward the cap.
		earlier, err := t.adoptedByOtherPasses(pass, s.Key)
		if err != nil {
			return Plan{}, nil, err
		}
		if earlier+len(s.Files)+1 > setFileCap {
			diags = append(diags, errDiag(DiagSetTooLarge,
				fmt.Sprintf("area %s holds %d captures this pass and %d from other passes; a vitrinka set holds at most %d files, its manifest included", area, len(s.Files), earlier, setFileCap),
				"split the area into smaller ones in the uiLoop config, then re-run the pass's split"))
			plan.Skipped = append(plan.Skipped, s.Key+" (too many files)")
			continue
		}
		plan.Sets = append(plan.Sets, s)
	}
	noted := make([]string, 0, len(notes))
	for a := range notes {
		noted = append(noted, a)
	}
	sortAreas(c.Areas, noted)
	for _, a := range noted {
		plan.Notes = append(plan.Notes, AreaNotes{Area: a, Shots: notes[a]})
	}
	return plan, diags, nil
}

func (t *Tool) planFiles(pass int, passDir string, r Record) ([]PlanFile, error) {
	vp := t.Config.ResolvedViewports()[r.Viewport]
	w, h := r.Size.Width, r.Size.Height
	if w == 0 {
		w, h = vp.Width, vp.Height
	}
	state := r.Theme
	if r.As != "" {
		state += " · as " + r.As
	}
	if len(t.Config.Apps) > 1 {
		state += " · " + r.App
	}
	// --route is the app route (path, query, hash); --url keeps the full address.
	route := r.Route
	if u, err := url.Parse(r.URL); err == nil && r.URL != "" {
		route = u.RequestURI()
		if u.Fragment != "" {
			route += "#" + u.Fragment
		}
	}
	if route == "" {
		route = "/"
	}
	src := r.SourceFiles
	if src == nil {
		src = []string{}
	}
	label := strings.ToUpper(fmt.Sprintf("p%d-%s-%s-%s", pass, r.ID, r.Viewport, r.Theme))
	mk := func(name string, full bool) (PlanFile, error) {
		rel := path.Join(r.Dir, name)
		if _, err := os.Stat(filepath.Join(passDir, filepath.FromSlash(rel))); err != nil {
			return PlanFile{}, fmt.Errorf("%s: %w", rel, err)
		}
		f := PlanFile{
			Path: rel, Stamp: r.CapturedAt, Shot: r.Key(), Full: full,
			Label: fit(label, "", maxLabel), Title: fmt.Sprintf("%s · %s · %s", r.Title, r.Viewport, r.Theme),
			Note: note(pass, r), Route: route, URL: r.URL, State: state,
			Viewport: r.Viewport, Theme: r.Theme, Size: fmt.Sprintf("%dx%d@%d", w, h, DPR), Src: src,
		}
		if full {
			f.Label = fit(label, "-FULL", maxLabel)
			f.Title += " · full"
			f.Note = "full content · " + f.Note
		}
		return f, nil
	}
	out := []PlanFile{}
	f, err := mk(r.Files.Viewport, false)
	if err != nil {
		return nil, err
	}
	out = append(out, f)
	if r.Files.Full != "" {
		f, err := mk(r.Files.Full, true)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, nil
}
