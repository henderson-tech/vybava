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

// PlanFile is one capture to adopt into a set.
type PlanFile struct {
	// Path is relative to the pass directory.
	Path     string   `json:"path"`
	Shot     string   `json:"shot"`
	Full     bool     `json:"full"`
	Bytes    int64    `json:"bytes"`
	SHA256   string   `json:"sha256"`
	Label    string   `json:"label"`
	Title    string   `json:"title"`
	Note     string   `json:"note"`
	Route    string   `json:"route"`
	URL      string   `json:"url,omitempty"`
	State    string   `json:"state"`
	Viewport string   `json:"viewport"`
	Src      []string `json:"src"`
}

// Set is one vitrinka set: an area × viewport × theme chunk.
type Set struct {
	Key      string     `json:"key"`
	Title    string     `json:"title"`
	Area     string     `json:"area"`
	Viewport string     `json:"viewport"`
	Theme    string     `json:"theme"`
	Chunk    int        `json:"chunk"`
	Chunks   int        `json:"chunks"`
	Bytes    int64      `json:"bytes"`
	Files    []PlanFile `json:"files"`
}

// Plan is split's deterministic output: the same pass and limits always
// produce the same sets, keys and order.
type Plan struct {
	V        int      `json:"v"`
	Pass     int      `json:"pass"`
	PassDir  string   `json:"passDir"`
	Project  string   `json:"project"`
	MaxFiles int      `json:"maxFiles"`
	MaxBytes int64    `json:"maxBytes"`
	Sets     []Set    `json:"sets"`
	Skipped  []string `json:"skipped"`
}

// SplitOptions narrow a split.
type SplitOptions struct {
	Pass  int
	Areas []string
}

const (
	// maxKey leaves room for the "b" a halved set appends (vitrinka keys cap at 64).
	maxKey   = 62
	maxLabel = 40
)

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

// Split plans the vitrinka sets of a pass: one per area × viewport × theme,
// chunked at publish.maxFiles files and publish.maxBytes source bytes (the
// adopted WebP is smaller, so the bound is conservative). A shot's viewport
// and full captures stay in one chunk. The plan is written to
// <passDir>/publish/plan.json.
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
	c := t.Config
	passDir := t.passAbs(pass)
	if _, err := os.Stat(passDir); err != nil {
		return Plan{}, nil, diag(DiagPassMissing, t.PassDir(pass)+" does not exist", "vybava ui-loop run")
	}
	records, err := LoadRecords(passDir)
	if err != nil {
		return Plan{}, nil, err
	}
	if len(records) == 0 {
		return Plan{}, nil, diag(DiagPassMissing, t.PassDir(pass)+" holds no shot records", fmt.Sprintf("vybava ui-loop run --resume --pass %d", pass))
	}
	plan := Plan{V: 1, Pass: pass, PassDir: t.PassDir(pass), Project: c.Vitrinka.Project, MaxFiles: c.Publish.MaxFiles, MaxBytes: c.Publish.MaxBytes, Sets: []Set{}, Skipped: []string{}}
	var diags []runxDiagnostic

	type group struct {
		area, viewport, theme string
		records               []Record
	}
	groups := map[string]*group{}
	for _, r := range records {
		if len(areas) > 0 && !slices.Contains(areas, r.Area) {
			continue
		}
		if r.Files.Viewport == "" {
			plan.Skipped = append(plan.Skipped, r.Key()+" ("+r.Status+")")
			continue
		}
		k := r.Area + "\x00" + r.Viewport + "\x00" + r.Theme
		g := groups[k]
		if g == nil {
			g = &group{area: r.Area, viewport: r.Viewport, theme: r.Theme}
			groups[k] = g
		}
		g.records = append(g.records, r)
	}
	ordered := make([]*group, 0, len(groups))
	for _, g := range groups {
		ordered = append(ordered, g)
	}
	vpOrder := c.ViewportOrder()
	themes := []string{"light", "dark"}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if x, y := orderOf(c.Areas, a.area), orderOf(c.Areas, b.area); x != y {
			return x < y
		}
		if a.area != b.area {
			return a.area < b.area
		}
		if x, y := orderOf(vpOrder, a.viewport), orderOf(vpOrder, b.viewport); x != y {
			return x < y
		}
		if a.viewport != b.viewport {
			return a.viewport < b.viewport
		}
		return orderOf(themes, a.theme) < orderOf(themes, b.theme)
	})

	for _, g := range ordered {
		base := fmt.Sprintf("%s-p%d-%s-%s-%s", c.Vitrinka.BoardPrefix, pass, g.area, g.viewport, g.theme)
		var chunks []Set
		cur := Set{Files: []PlanFile{}}
		for _, r := range g.records {
			unit, err := t.planFiles(pass, passDir, r)
			if err != nil {
				return Plan{}, nil, err
			}
			var ub int64
			for _, f := range unit {
				ub += f.Bytes
				if f.Bytes > c.Publish.MaxBytes {
					diags = append(diags, warn(DiagFileTooLarge, fmt.Sprintf("%s is %d bytes, above publish.maxBytes %d", f.Path, f.Bytes, c.Publish.MaxBytes), ""))
				}
			}
			if len(cur.Files) > 0 && (len(cur.Files)+len(unit) > c.Publish.MaxFiles || cur.Bytes+ub > c.Publish.MaxBytes) {
				chunks = append(chunks, cur)
				cur = Set{Files: []PlanFile{}}
			}
			cur.Files = append(cur.Files, unit...)
			cur.Bytes += ub
		}
		chunks = append(chunks, cur)
		for i := range chunks {
			s := &chunks[i]
			s.Area, s.Viewport, s.Theme, s.Chunk, s.Chunks = g.area, g.viewport, g.theme, i+1, len(chunks)
			s.Key = fit(base, fmt.Sprintf("-%d", i+1), maxKey)
			s.Title = fmt.Sprintf("%s · %s · pass %d · %s · %s", c.Vitrinka.BoardPrefix, g.area, pass, g.viewport, g.theme)
			if len(chunks) > 1 {
				s.Title += fmt.Sprintf(" (%d/%d)", i+1, len(chunks))
			}
		}
		plan.Sets = append(plan.Sets, chunks...)
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
		body, err := os.ReadFile(filepath.Join(passDir, filepath.FromSlash(rel)))
		if err != nil {
			return PlanFile{}, fmt.Errorf("%s: %w", rel, err)
		}
		f := PlanFile{
			Path: rel, Shot: r.Key(), Full: full, Bytes: int64(len(body)), SHA256: digest(string(body), 64),
			Label: fit(label, "", maxLabel), Title: fmt.Sprintf("%s · %s · %s", r.Title, r.Viewport, r.Theme),
			Note: note(pass, r), Route: route, URL: r.URL, State: state,
			Viewport: fmt.Sprintf("%dx%d@%d", w, h, DPR), Src: src,
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
