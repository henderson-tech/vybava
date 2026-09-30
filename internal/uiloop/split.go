package uiloop

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io/fs"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// PlanFile is one capture to adopt into a set.
type PlanFile struct {
	// Path is relative to the pass directory.
	Path string `json:"path"`
	Shot string `json:"shot"`
	Full bool   `json:"full"`
	// Bytes is the upload size: the WebP `board capture` made of the file
	// once it is adopted (Measured), else estimateUpload's bound.
	Bytes    int64    `json:"bytes"`
	Measured bool     `json:"measured,omitempty"`
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
	// Notes are the shots published as text, per area in config order.
	Notes []AreaNotes `json:"notes"`
}

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

// webpBytesPerPixel bounds the WebP `vitrinka board capture` makes of a
// PNG. PNG bytes predict it badly: on pwf-ui pass 1 (1,382 adoptions) the
// WebP was 7 % of a noisy 4 MB desktop PNG and 93 % of a small flat one,
// while WebP bytes per source pixel stayed at p50 0.035, p99 0.089, max
// 0.136. min(PNG bytes, 0.1 × pixels) came out at p99 0.89 of the real size.
const webpBytesPerPixel = 0.1

// estimateUpload bounds a PNG's upload size before it is adopted; a file
// whose header does not decode counts at its own size.
func estimateUpload(file string, pngBytes int64) int64 {
	f, err := os.Open(file)
	if err != nil {
		return pngBytes
	}
	defer f.Close()
	cfg, err := png.DecodeConfig(f)
	if err != nil {
		return pngBytes
	}
	return min(pngBytes, int64(float64(cfg.Width)*float64(cfg.Height)*webpBytesPerPixel))
}

// adoptedState is what earlier publishes adopted: the set each file went
// into (the ledgers) and the WebP bytes each set root holds per label (its
// manifest). The plan keeps an adopted file in its set — `board push` only
// adds, so a file that moved would sit on two boards — and sizes it by what
// was really uploaded.
type adoptedState struct {
	set   map[string]string           // pass-relative file → set key
	bytes map[string]map[string]int64 // set key → shot label → WebP bytes
}

// loadAdopted reads every ledger under <passDir>/publish (and one an older
// run left inside its root).
func loadAdopted(passDir string) (adoptedState, error) {
	st := adoptedState{set: map[string]string{}, bytes: map[string]map[string]int64{}}
	pub := filepath.Join(passDir, "publish")
	ledgers, err := filepath.Glob(filepath.Join(pub, "adopted", "*"))
	if err != nil {
		return st, err
	}
	legacy, err := filepath.Glob(filepath.Join(pub, "sets", "*", adoptedLedger))
	if err != nil {
		return st, err
	}
	var keys []string
	for _, l := range append(ledgers, legacy...) {
		key := filepath.Base(l)
		if key == adoptedLedger {
			key = filepath.Base(filepath.Dir(l))
		}
		if err := st.read(pub, key, l); err != nil {
			return st, err
		}
		keys = append(keys, key)
	}
	for _, k := range keys {
		if err := st.measure(pub, k); err != nil {
			return st, err
		}
	}
	return st, nil
}

// read folds one ledger in; a file in two ledgers keeps the smaller key, so
// the answer never depends on the glob's order.
func (st adoptedState) read(pub, key, ledger string) error {
	b, err := os.ReadFile(ledger)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		if line == "" {
			continue
		}
		if was, ok := st.set[line]; !ok || key < was {
			st.set[line] = key
		}
	}
	return nil
}

// measure reads a set root's manifest: label → the adopted WebP's size.
func (st adoptedState) measure(pub, key string) error {
	root := filepath.Join(pub, "sets", key)
	b, err := os.ReadFile(filepath.Join(root, "manifest.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var m struct {
		Shots []struct {
			File  string `json:"file"`
			Label string `json:"label"`
		} `json:"shots"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("%s: %w", filepath.Join(root, "manifest.json"), err)
	}
	sizes := map[string]int64{}
	for _, s := range m.Shots {
		if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(s.File))); err == nil {
			sizes[s.Label] = fi.Size()
		}
	}
	st.bytes[key] = sizes
	return nil
}

// refresh re-reads one set after an adoption into it.
func (st adoptedState) refresh(passDir, key string) error {
	pub := filepath.Join(passDir, "publish")
	ledger, err := ledgerFor(filepath.Join(pub, "sets", key))
	if err != nil {
		return err
	}
	if _, err := os.Stat(ledger); err == nil {
		if err := st.read(pub, key, ledger); err != nil {
			return err
		}
	}
	return st.measure(pub, key)
}

// chunkOf is the chunk of the group keyed base that a set key names (its
// own `-<n>` key, or a halved `-<n>a` / `-<n>b`), else 0.
func chunkOf(base, key string) int {
	k := key
	if strings.HasSuffix(k, "a") || strings.HasSuffix(k, "b") {
		k = k[:len(k)-1]
	}
	i := strings.LastIndex(k, "-")
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(k[i+1:])
	if err != nil || n < 1 || fit(base, fmt.Sprintf("-%d", n), maxKey) != k {
		return 0
	}
	return n
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
// chunked at publish.maxFiles files and publish.maxBytes upload bytes (the
// adopted WebP where one exists, else estimateUpload). A shot's viewport and
// full captures stay in one chunk; a file already adopted stays in its set.
// The plan is written to <passDir>/publish/plan.json.
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
	st, err := loadAdopted(passDir)
	if err != nil {
		return Plan{}, nil, err
	}
	return t.planRecords(pass, records, areas, st)
}

// planRecords plans the given records of a pass (publish --follow passes
// only the final ones).
func (t *Tool) planRecords(pass int, records []Record, areas []string, st adoptedState) (Plan, []runxDiagnostic, error) {
	c := t.Config
	passDir := t.passAbs(pass)
	if len(records) == 0 {
		return Plan{}, nil, diag(DiagPassMissing, t.PassDir(pass)+" holds no shot records", fmt.Sprintf("vybava ui-loop run --resume --pass %d", pass))
	}
	plan := Plan{V: 1, Pass: pass, PassDir: t.PassDir(pass), Project: c.Vitrinka.Project, MaxFiles: c.Publish.MaxFiles, MaxBytes: c.Publish.MaxBytes, Sets: []Set{}, Skipped: []string{}, Notes: []AreaNotes{}}
	var diags []runxDiagnostic

	type group struct {
		area, viewport, theme string
		records               []Record
	}
	groups := map[string]*group{}
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
		// Greedy in manifest order, except that a shot already adopted stays
		// in its chunk: a shot that arrives late (a resume retake, a slower
		// worker) joins the open chunk instead of shifting the ones pushed.
		chunks := map[int]*Set{}
		last, cur := 0, 1
		for _, r := range g.records {
			unit, err := t.planFiles(pass, passDir, r)
			if err != nil {
				return Plan{}, nil, err
			}
			var ub int64
			pinned := 0
			for i := range unit {
				f := &unit[i]
				key, adopted := st.set[f.Path]
				if b, ok := st.bytes[key][f.Label]; adopted && ok {
					f.Bytes, f.Measured = b, true
				} else {
					f.Bytes = estimateUpload(filepath.Join(passDir, filepath.FromSlash(f.Path)), f.Bytes)
				}
				if n := chunkOf(base, key); adopted && n > 0 && pinned == 0 {
					pinned = n
				}
				ub += f.Bytes
				if f.Bytes > c.Publish.MaxBytes {
					diags = append(diags, warn(DiagFileTooLarge, fmt.Sprintf("%s uploads %d bytes, above publish.maxBytes %d", f.Path, f.Bytes, c.Publish.MaxBytes), ""))
				}
			}
			n := pinned
			if n == 0 {
				if s := chunks[cur]; s != nil && (len(s.Files)+len(unit) > c.Publish.MaxFiles || s.Bytes+ub > c.Publish.MaxBytes) {
					cur = last + 1
				}
				n = cur
			} else {
				cur = max(cur, n)
			}
			s := chunks[n]
			if s == nil {
				s = &Set{Files: []PlanFile{}}
				chunks[n] = s
			}
			s.Files = append(s.Files, unit...)
			s.Bytes += ub
			last = max(last, n)
		}
		for n := 1; n <= last; n++ {
			s := chunks[n]
			if s == nil {
				continue
			}
			s.Area, s.Viewport, s.Theme, s.Chunk, s.Chunks = g.area, g.viewport, g.theme, n, last
			s.Key = fit(base, fmt.Sprintf("-%d", n), maxKey)
			s.Title = fmt.Sprintf("%s · %s · pass %d · %s · %s", c.Vitrinka.BoardPrefix, g.area, pass, g.viewport, g.theme)
			if last > 1 {
				s.Title += fmt.Sprintf(" (%d/%d)", n, last)
			}
			plan.Sets = append(plan.Sets, *s)
		}
	}
	noted := make([]string, 0, len(notes))
	for a := range notes {
		noted = append(noted, a)
	}
	sort.Slice(noted, func(i, j int) bool {
		if x, y := orderOf(c.Areas, noted[i]), orderOf(c.Areas, noted[j]); x != y {
			return x < y
		}
		return noted[i] < noted[j]
	})
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
		st, err := os.Stat(filepath.Join(passDir, filepath.FromSlash(rel)))
		if err != nil {
			return PlanFile{}, fmt.Errorf("%s: %w", rel, err)
		}
		f := PlanFile{
			Path: rel, Shot: r.Key(), Full: full, Bytes: st.Size(),
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
