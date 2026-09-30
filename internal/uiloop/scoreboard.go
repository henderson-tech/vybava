package uiloop

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// Backlog is the review backlog a pass's reviewers produce (the vitrinka
// review-loop writes it to <passDir>/review/backlog.json): every finding of
// this pass, including the verdicts on the previous backlog's acceptance.
type Backlog struct {
	V        int       `json:"v"`
	Pass     int       `json:"pass"`
	Findings []Finding `json:"findings"`
	// Reviewed are the screen ids a reviewer actually judged this pass. Only
	// these can be clean; a screen outside it is unreviewed, never clean.
	// nil (the key absent: an older review-loop) scores every screen without
	// an open finding as clean and warns REVIEWED_MISSING.
	Reviewed []string `json:"reviewed,omitempty"`
}

// Finding is one reviewed defect.
type Finding struct {
	// Key is stable across passes: a pass-N+1 verdict reuses its pass-N key.
	Key    string `json:"key"`
	Screen string `json:"screen"`
	Area   string `json:"area"`
	// Severity: broken | needs-work | polish.
	Severity string `json:"severity"`
	// Status: open (found this pass) | met | partly | not-met (a previous
	// finding's acceptance, verified this pass).
	Status string `json:"status"`
	Title  string `json:"title"`
	Detail string `json:"detail,omitempty"`
	// Files route the fix lanes (repo-relative).
	Files []string `json:"files"`
	// Acceptance is what the next pass checks to call it met.
	Acceptance string   `json:"acceptance"`
	Viewports  []string `json:"viewports,omitempty"`
	Themes     []string `json:"themes,omitempty"`
	// Shots are the captures it was seen on: <id>@<viewport>.<theme>.
	Shots []string `json:"shots,omitempty"`
	// Refs: board cards, spec sections, tasks.
	Refs []string `json:"refs,omitempty"`
}

// Severities worst first.
var Severities = []string{"broken", "needs-work", "polish"}

// FindingStatuses are the closed status vocabulary.
var FindingStatuses = []string{"open", "met", "partly", "not-met"}

// Open reports whether the finding still needs work.
func (f Finding) Open() bool { return f.Status != "met" }

// Validate reports every problem at once.
func (b Backlog) Validate() []string {
	var problems []string
	if b.V != 1 {
		problems = append(problems, fmt.Sprintf("v must be 1, got %d", b.V))
	}
	seen := map[string]bool{}
	for i, f := range b.Findings {
		at := fmt.Sprintf("findings[%d]", i)
		if f.Key != "" {
			at += " " + f.Key
		}
		if f.Key == "" {
			problems = append(problems, at+": key is required")
		} else if seen[f.Key] {
			problems = append(problems, at+": duplicate key")
		}
		seen[f.Key] = true
		if f.Screen == "" || f.Area == "" {
			problems = append(problems, at+": screen and area are required")
		}
		if !slices.Contains(Severities, f.Severity) {
			problems = append(problems, fmt.Sprintf("%s: severity %q is not broken|needs-work|polish", at, f.Severity))
		}
		if !slices.Contains(FindingStatuses, f.Status) {
			problems = append(problems, fmt.Sprintf("%s: status %q is not open|met|partly|not-met", at, f.Status))
		}
		if strings.TrimSpace(f.Title) == "" || strings.TrimSpace(f.Acceptance) == "" {
			problems = append(problems, at+": title and acceptance are required")
		}
		if f.Open() && len(f.Files) == 0 {
			problems = append(problems, at+": an open finding names the files a fix lane edits")
		}
	}
	for i, id := range b.Reviewed {
		if strings.TrimSpace(id) == "" {
			problems = append(problems, fmt.Sprintf("reviewed[%d]: a screen id is required", i))
		}
	}
	return problems
}

// LoadBacklog decodes a backlog strictly.
func LoadBacklog(file string) (*Backlog, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var b Backlog
	if err := dec.Decode(&b); err != nil {
		return nil, diag(DiagBacklogInvalid, file+": "+err.Error(), "fix the backlog (docs/uiloop.md has the shape)")
	}
	if problems := b.Validate(); len(problems) > 0 {
		return nil, diag(DiagBacklogInvalid, file+": "+strings.Join(problems, "; "), "fix the backlog (docs/uiloop.md has the shape)")
	}
	return &b, nil
}

// AreaScore is one row of the scoreboard.
type AreaScore struct {
	Area  string `json:"area"`
	Shots int    `json:"shots"`
	NotOk int    `json:"notOk"`
	// Screens by their worst open finding (needs a backlog).
	Screens   int `json:"screens"`
	Broken    int `json:"broken"`
	NeedsWork int `json:"needsWork"`
	Polish    int `json:"polish"`
	// Clean: judged by a reviewer (in the backlog's reviewed) with no open
	// finding. Unreviewed: no open finding and no reviewer judged it.
	Clean      int `json:"clean"`
	Unreviewed int `json:"unreviewed"`
	// Findings by status.
	Open   int `json:"open"`
	Met    int `json:"met"`
	Partly int `json:"partly"`
	NotMet int `json:"notMet"`
	// LintDefects and Lint sum every shot's hits: one sidebar defect counts
	// once per screen it is on.
	LintDefects int            `json:"lintDefects"`
	Lint        map[string]int `json:"lint"`
	// LintDefectsUnique and LintUnique count each rule + element path +
	// detail once across the area's shots (Scoreboard.UniqueKnown says
	// whether the records carried the keys).
	LintDefectsUnique int            `json:"lintDefectsUnique"`
	LintUnique        map[string]int `json:"lintUnique"`
	ConsoleErrors     int            `json:"consoleErrors"`
}

// Scoreboard is <passDir>/scoreboard.json.
type Scoreboard struct {
	V        int  `json:"v"`
	Pass     int  `json:"pass"`
	Reviewed bool `json:"reviewed"`
	// ReviewedKnown: the backlog named the screens it judged, so clean means
	// judged clean and the unreviewed column is real.
	ReviewedKnown bool        `json:"reviewedKnown"`
	Areas         []AreaScore `json:"areas"`
	Totals        AreaScore   `json:"totals"`
	// UniqueKnown: every shot with lint defects carried its distinct keys
	// (a harness before v0.23 did not), so the unique columns are real.
	UniqueKnown bool `json:"uniqueKnown"`
	// Offenders are the defects seen on the most screens (2 or more).
	Offenders []Offender `json:"offenders"`
	Delta     *Delta     `json:"delta,omitempty"`
}

// Offender is one defect repeated across screens: fix it once, where it lives.
type Offender struct {
	Rule   string `json:"rule"`
	Path   string `json:"path"`
	Detail string `json:"detail"`
	// Screens it was seen on (distinct ids) and shots (screen × viewport × theme).
	Screens int `json:"screens"`
	Shots   int `json:"shots"`
}

// MaxOffenders bounds Scoreboard.Offenders.
const MaxOffenders = 20

// Delta is this pass minus the previous one; severity columns only when both were reviewed.
type Delta struct {
	Pass     int  `json:"pass"`
	Reviewed bool `json:"reviewed"`
	// ReviewedKnown: both passes named their reviewed screens, so the
	// unreviewed delta is real.
	ReviewedKnown bool `json:"reviewedKnown"`
	// UniqueKnown: both passes carried distinct keys, so unique deltas are real.
	UniqueKnown bool        `json:"uniqueKnown"`
	Areas       []AreaScore `json:"areas"`
	Totals      AreaScore   `json:"totals"`
}

// ScoreboardOptions are the scoreboard verb's flags.
type ScoreboardOptions struct {
	Pass int
	// Backlog defaults to <passDir>/review/backlog.json when it exists.
	Backlog string
	// Previous: 0 = the pass before, -1 = none.
	Previous int
}

func newArea(area string) AreaScore {
	return AreaScore{Area: area, Lint: map[string]int{}, LintUnique: map[string]int{}}
}

func (a *AreaScore) add(b AreaScore) {
	a.Shots += b.Shots
	a.NotOk += b.NotOk
	a.Screens += b.Screens
	a.Broken += b.Broken
	a.NeedsWork += b.NeedsWork
	a.Polish += b.Polish
	a.Clean += b.Clean
	a.Unreviewed += b.Unreviewed
	a.Open += b.Open
	a.Met += b.Met
	a.Partly += b.Partly
	a.NotMet += b.NotMet
	a.LintDefects += b.LintDefects
	a.ConsoleErrors += b.ConsoleErrors
	for k, v := range b.Lint {
		a.Lint[k] += v
	}
}

// ComputeScoreboard folds a pass's records and (optionally) its backlog.
func ComputeScoreboard(pass int, areaOrder []string, records []Record, backlog *Backlog) Scoreboard {
	areas := map[string]*AreaScore{}
	get := func(name string) *AreaScore {
		if a, ok := areas[name]; ok {
			return a
		}
		a := newArea(name)
		areas[name] = &a
		return &a
	}
	screensOf := map[string]map[string]bool{}
	for _, r := range records {
		a := get(r.Area)
		a.Shots++
		if r.Status != "ok" {
			a.NotOk++
		}
		a.ConsoleErrors += len(r.ConsoleErrors)
		if r.Lint != nil {
			for k, v := range r.Lint.Defects {
				a.Lint[k] += v
				a.LintDefects += v
			}
		}
		if screensOf[r.Area] == nil {
			screensOf[r.Area] = map[string]bool{}
		}
		screensOf[r.Area][r.ID] = true
	}
	worst := map[string]int{} // area\x00screen → severity index (lower = worse)
	// judged is nil when the backlog does not say which screens were reviewed.
	var judged map[string]bool
	if backlog != nil && backlog.Reviewed != nil {
		judged = map[string]bool{}
		for _, id := range backlog.Reviewed {
			judged[id] = true
		}
	}
	if backlog != nil {
		for _, f := range backlog.Findings {
			a := get(f.Area)
			switch f.Status {
			case "open":
				a.Open++
			case "met":
				a.Met++
			case "partly":
				a.Partly++
			case "not-met":
				a.NotMet++
			}
			if screensOf[f.Area] == nil {
				screensOf[f.Area] = map[string]bool{}
			}
			screensOf[f.Area][f.Screen] = true
			if !f.Open() {
				continue
			}
			k := f.Area + "\x00" + f.Screen
			sev := slices.Index(Severities, f.Severity)
			if cur, ok := worst[k]; !ok || sev < cur {
				worst[k] = sev
			}
		}
	}
	for area, screens := range screensOf {
		a := get(area)
		a.Screens = len(screens)
		if backlog == nil {
			continue
		}
		for s := range screens {
			sev, ok := worst[area+"\x00"+s]
			switch {
			case !ok && judged != nil && !judged[s]:
				a.Unreviewed++
			case !ok:
				a.Clean++
			case sev == 0:
				a.Broken++
			case sev == 1:
				a.NeedsWork++
			default:
				a.Polish++
			}
		}
	}
	sb := Scoreboard{V: 1, Pass: pass, Reviewed: backlog != nil, ReviewedKnown: judged != nil, Totals: newArea("all"), Areas: []AreaScore{}}
	names := make([]string, 0, len(areas))
	for n := range areas {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		x, y := orderOf(areaOrder, names[i]), orderOf(areaOrder, names[j])
		return x < y || (x == y && names[i] < names[j])
	})
	for _, n := range names {
		sb.Areas = append(sb.Areas, *areas[n])
		sb.Totals.add(*areas[n])
	}
	// Unique counts never sum: a chrome defect is on every area's screens.
	for i := range sb.Areas {
		addUnique(&sb.Areas[i], records, sb.Areas[i].Area)
	}
	addUnique(&sb.Totals, records, "")
	sb.UniqueKnown = true
	for _, r := range records {
		if r.Defects() > 0 && r.Lint.Distinct == nil {
			sb.UniqueKnown = false
		}
	}
	sb.Offenders = repeatedOffenders(records, MaxOffenders)
	return sb
}

func lintKey(rule string, k LintKey) string { return rule + "\x00" + k.Path + "\x00" + k.Detail }

// addUnique counts each rule + path + detail once across the records of
// area ("" = every area).
func addUnique(a *AreaScore, records []Record, area string) {
	seen := map[string]bool{}
	for _, r := range records {
		if r.Lint == nil || (area != "" && r.Area != area) {
			continue
		}
		for rule, keys := range r.Lint.Distinct {
			for _, k := range keys {
				if key := lintKey(rule, k); !seen[key] {
					seen[key] = true
					a.LintUnique[rule]++
					a.LintDefectsUnique++
				}
			}
		}
	}
}

// repeatedOffenders are the defects seen on 2+ screens, most screens first.
func repeatedOffenders(records []Record, limit int) []Offender {
	type acc struct {
		o   Offender
		ids map[string]bool
	}
	by := map[string]*acc{}
	for _, r := range records {
		if r.Lint == nil {
			continue
		}
		for rule, keys := range r.Lint.Distinct {
			for _, k := range keys {
				key := lintKey(rule, k)
				a := by[key]
				if a == nil {
					a = &acc{o: Offender{Rule: rule, Path: k.Path, Detail: k.Detail}, ids: map[string]bool{}}
					by[key] = a
				}
				a.o.Shots++
				a.ids[r.ID] = true
				a.o.Screens = len(a.ids)
			}
		}
	}
	out := []Offender{}
	for _, a := range by {
		if a.o.Screens > 1 {
			out = append(out, a.o)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		x, y := out[i], out[j]
		if x.Screens != y.Screens {
			return x.Screens > y.Screens
		}
		if x.Shots != y.Shots {
			return x.Shots > y.Shots
		}
		return lintKey(x.Rule, LintKey{x.Path, x.Detail}) < lintKey(y.Rule, LintKey{y.Path, y.Detail})
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func minus(cur, prev AreaScore) AreaScore {
	d := newArea(cur.Area)
	d.Shots, d.NotOk, d.Screens = cur.Shots-prev.Shots, cur.NotOk-prev.NotOk, cur.Screens-prev.Screens
	d.Broken, d.NeedsWork, d.Polish, d.Clean = cur.Broken-prev.Broken, cur.NeedsWork-prev.NeedsWork, cur.Polish-prev.Polish, cur.Clean-prev.Clean
	d.Unreviewed = cur.Unreviewed - prev.Unreviewed
	d.Open, d.Met, d.Partly, d.NotMet = cur.Open-prev.Open, cur.Met-prev.Met, cur.Partly-prev.Partly, cur.NotMet-prev.NotMet
	d.LintDefects, d.ConsoleErrors = cur.LintDefects-prev.LintDefects, cur.ConsoleErrors-prev.ConsoleErrors
	for k, v := range cur.Lint {
		d.Lint[k] += v
	}
	for k, v := range prev.Lint {
		d.Lint[k] -= v
	}
	for k, v := range d.Lint {
		if v == 0 {
			delete(d.Lint, k)
		}
	}
	d.LintDefectsUnique = cur.LintDefectsUnique - prev.LintDefectsUnique
	for k, v := range cur.LintUnique {
		d.LintUnique[k] += v
	}
	for k, v := range prev.LintUnique {
		d.LintUnique[k] -= v
	}
	for k, v := range d.LintUnique {
		if v == 0 {
			delete(d.LintUnique, k)
		}
	}
	return d
}

// WithDelta attaches this-minus-previous per area (areas of either pass).
func (sb *Scoreboard) WithDelta(prev Scoreboard) {
	d := &Delta{Pass: prev.Pass, Reviewed: sb.Reviewed && prev.Reviewed, ReviewedKnown: sb.ReviewedKnown && prev.ReviewedKnown, UniqueKnown: sb.UniqueKnown && prev.UniqueKnown}
	prevBy := map[string]AreaScore{}
	for _, a := range prev.Areas {
		prevBy[a.Area] = a
	}
	seen := map[string]bool{}
	for _, a := range sb.Areas {
		p, ok := prevBy[a.Area]
		if !ok {
			p = newArea(a.Area)
		}
		d.Areas = append(d.Areas, minus(a, p))
		seen[a.Area] = true
	}
	for _, p := range prev.Areas {
		if !seen[p.Area] {
			d.Areas = append(d.Areas, minus(newArea(p.Area), p))
		}
	}
	d.Totals = minus(sb.Totals, prev.Totals)
	sb.Delta = d
}

func signed(n int) string {
	if n > 0 {
		return fmt.Sprintf("+%d", n)
	}
	if n < 0 {
		return fmt.Sprintf("−%d", -n)
	}
	return "±0"
}

// Markdown renders the scoreboard for a board callout or a PR.
func (sb Scoreboard) Markdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# UI loop scoreboard · pass %d\n\n", sb.Pass)
	delta := map[string]AreaScore{}
	if sb.Delta != nil {
		for _, a := range sb.Delta.Areas {
			delta[a.Area] = a
		}
		delta["all"] = sb.Delta.Totals
		fmt.Fprintf(&b, "Change against pass %d in brackets.\n\n", sb.Delta.Pass)
	}
	if !sb.Reviewed {
		b.WriteString("Not reviewed yet: no backlog, so only the capture and lint columns are filled.\n\n")
	} else if !sb.ReviewedKnown {
		b.WriteString("The backlog names no reviewed screens, so clean means no finding, not judged clean.\n\n")
	} else if sb.Totals.Unreviewed > 0 {
		fmt.Fprintf(&b, "%d screens were never judged by a reviewer this pass: unreviewed, not clean.\n\n", sb.Totals.Unreviewed)
	}
	cell := func(area string, v int, pick func(AreaScore) int, severity bool) string {
		if severity && !sb.Reviewed {
			return "—"
		}
		d, ok := delta[area]
		if !ok || (severity && !sb.Delta.Reviewed) {
			return fmt.Sprint(v)
		}
		return fmt.Sprintf("%d (%s)", v, signed(pick(d)))
	}
	// Unique columns need the records' distinct keys; their delta needs both passes'.
	unique := func(area string, v int, pick func(AreaScore) int) string {
		if !sb.UniqueKnown {
			return "—"
		}
		d, ok := delta[area]
		if !ok || !sb.Delta.UniqueKnown {
			return fmt.Sprint(v)
		}
		return fmt.Sprintf("%d (%s)", v, signed(pick(d)))
	}
	// Unreviewed needs the backlog's reviewed list; its delta needs both passes'.
	unreviewed := func(area string, v int) string {
		if !sb.ReviewedKnown {
			return "—"
		}
		d, ok := delta[area]
		if !ok || !sb.Delta.ReviewedKnown {
			return fmt.Sprint(v)
		}
		return fmt.Sprintf("%d (%s)", v, signed(d.Unreviewed))
	}
	b.WriteString("| Area | Screens | Broken | Needs work | Polish | Clean | Unreviewed | Open · partly · not met · met | Lint defects | Unique lint | Console errors | Shots not ok |\n")
	b.WriteString("|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|\n")
	for _, a := range append(append([]AreaScore{}, sb.Areas...), sb.Totals) {
		name := a.Area
		if name == "all" {
			name = "**all**"
		}
		findings := "—"
		if sb.Reviewed {
			findings = fmt.Sprintf("%d · %d · %d · %d", a.Open, a.Partly, a.NotMet, a.Met)
		}
		fmt.Fprintf(&b, "| %s | %d | %s | %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", name, a.Screens,
			cell(a.Area, a.Broken, func(d AreaScore) int { return d.Broken }, true),
			cell(a.Area, a.NeedsWork, func(d AreaScore) int { return d.NeedsWork }, true),
			cell(a.Area, a.Polish, func(d AreaScore) int { return d.Polish }, true),
			cell(a.Area, a.Clean, func(d AreaScore) int { return d.Clean }, true),
			unreviewed(a.Area, a.Unreviewed),
			findings,
			cell(a.Area, a.LintDefects, func(d AreaScore) int { return d.LintDefects }, false),
			unique(a.Area, a.LintDefectsUnique, func(d AreaScore) int { return d.LintDefectsUnique }),
			cell(a.Area, a.ConsoleErrors, func(d AreaScore) int { return d.ConsoleErrors }, false),
			cell(a.Area, a.NotOk, func(d AreaScore) int { return d.NotOk }, false))
	}
	rules := make([]string, 0, len(sb.Totals.Lint))
	for r := range sb.Totals.Lint {
		rules = append(rules, r)
	}
	sort.Slice(rules, func(i, j int) bool {
		x, y := sb.Totals.Lint[rules[i]], sb.Totals.Lint[rules[j]]
		return x > y || (x == y && rules[i] < rules[j])
	})
	if len(rules) > 0 {
		b.WriteString("\nLint defects by rule: ")
		for i, r := range rules {
			if i > 0 {
				b.WriteString(" · ")
			}
			fmt.Fprintf(&b, "`%s` %d", r, sb.Totals.Lint[r])
			if sb.Delta != nil {
				fmt.Fprintf(&b, " (%s)", signed(sb.Delta.Totals.Lint[r]))
			}
			if sb.UniqueKnown {
				fmt.Fprintf(&b, ", %d unique", sb.Totals.LintUnique[r])
				if sb.Delta != nil && sb.Delta.UniqueKnown {
					fmt.Fprintf(&b, " (%s)", signed(sb.Delta.Totals.LintUnique[r]))
				}
			}
		}
		b.WriteString("\n")
	}
	if len(sb.Offenders) > 0 {
		b.WriteString("\nRepeated offenders — the same defect on several screens; fix it once, where it lives:\n\n")
		for i, o := range sb.Offenders {
			if i == 10 {
				fmt.Fprintf(&b, "- … %d more in scoreboard.json\n", len(sb.Offenders)-10)
				break
			}
			fmt.Fprintf(&b, "- `%s` %s · `%s` · %d screens, %d shots\n", o.Rule, o.Detail, o.Path, o.Screens, o.Shots)
		}
	}
	return b.String()
}

// Scoreboard computes a pass's scoreboard (and its delta against the
// previous pass) and writes scoreboard.json + scoreboard.md into the pass dir.
func (t *Tool) Scoreboard(o ScoreboardOptions) (Result, error) {
	pass, err := t.resolveShotPass(o.Pass)
	if err != nil {
		return Result{}, err
	}
	dir := t.passAbs(pass)
	records, err := LoadRecords(dir)
	if err != nil {
		return Result{}, err
	}
	if len(records) == 0 {
		return Result{}, diag(DiagPassMissing, t.PassDir(pass)+" holds no shot records", "vybava ui-loop run")
	}
	file := o.Backlog
	if file == "" {
		if _, err := os.Stat(filepath.Join(dir, "review", "backlog.json")); err == nil {
			file = filepath.Join(dir, "review", "backlog.json")
		}
	}
	var backlog *Backlog
	if file != "" {
		if backlog, err = LoadBacklog(file); err != nil {
			return Result{}, err
		}
		// A previous pass's backlog would mark this pass reviewed with stale verdicts.
		if backlog.Pass != pass {
			return Result{}, diag(DiagBacklogInvalid, fmt.Sprintf("%s is the backlog of pass %d, not pass %d", file, backlog.Pass, pass),
				fmt.Sprintf("pass this pass's backlog (its \"pass\" is %d)", pass))
		}
	}
	sb := ComputeScoreboard(pass, t.Config.Areas, records, backlog)
	var diags []runxDiagnostic
	if backlog != nil && backlog.Reviewed == nil {
		diags = append(diags, warn(DiagReviewedMissing,
			file+" has no \"reviewed\" list, so a screen with no finding counts as clean even if no reviewer judged it",
			"have the review stage write \"reviewed\": the screen ids its reviewers judged this pass (docs/uiloop.md)"))
	}
	prevPass := o.Previous
	if prevPass == 0 {
		prevPass = pass - 1
	}
	if prevPass > 0 {
		prev, err := t.previousScoreboard(prevPass)
		switch {
		case err != nil:
			return Result{}, err
		case prev == nil:
			diags = append(diags, info(DiagNoPrevious, fmt.Sprintf("pass %d has no records or scoreboard; no delta", prevPass), ""))
		default:
			sb.WithDelta(*prev)
		}
	}
	b, err := json.MarshalIndent(sb, "", "  ")
	if err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "scoreboard.json"), append(b, '\n'), 0o644); err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "scoreboard.md"), []byte(sb.Markdown()), 0o644); err != nil {
		return Result{}, err
	}
	return Result{Data: sb, Diagnostics: diags}, nil
}

// previousScoreboard prefers the pass's written scoreboard (it carries the
// review), else recomputes the capture columns from its records.
func (t *Tool) previousScoreboard(pass int) (*Scoreboard, error) {
	dir := t.passAbs(pass)
	if b, err := os.ReadFile(filepath.Join(dir, "scoreboard.json")); err == nil {
		var sb Scoreboard
		if err := json.Unmarshal(b, &sb); err != nil {
			return nil, fmt.Errorf("pass %d scoreboard.json: %w", pass, err)
		}
		return &sb, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	records, err := LoadRecords(dir)
	if err != nil || len(records) == 0 {
		return nil, err
	}
	sb := ComputeScoreboard(pass, t.Config.Areas, records, nil)
	return &sb, nil
}
