package perflab

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/henderson-tech/vybava/internal/vconfig"
)

// Hazard rules: render-cost sites to review, never verdicts. The sweep
// lists loop sites and says whether the file names an ambient gate; it
// never claims a loop is visibility-gated (docs/perflab.md "hazards").
const (
	HazardInfiniteRepeat  = "infinite-repeat"         // withRepeat(…, -1 …)
	HazardFrameCallback   = "frame-callback"          // useFrameCallback(
	HazardPathValue       = "path-value"              // usePathValue( (Skia redraws forever)
	HazardClock           = "clock"                   // useClock(
	HazardTextureNoCollap = "hw-texture-collapsable"  // renderToHardwareTextureAndroid without collapsable
	HazardClippedSubviews = "remove-clipped-subviews" // measured worse on flings
	HazardScrollViewRoute = "scrollview-route"        // a route file scrolling without FlashList/FlatList
	HazardQueriesNoComb   = "queries-without-combine" // useQueries( without combine
	HazardIntlInRender    = "intl-in-render"          // new Intl.* in a component body
)

// HazardRules lists every rule id.
var HazardRules = []string{HazardInfiniteRepeat, HazardFrameCallback, HazardPathValue, HazardClock, HazardTextureNoCollap,
	HazardClippedSubviews, HazardScrollViewRoute, HazardQueriesNoComb, HazardIntlInRender}

// loopRules are the rules whose rows carry a gate verdict.
var loopRules = map[string]bool{HazardInfiniteRepeat: true, HazardFrameCallback: true, HazardPathValue: true, HazardClock: true}

// HazardRow is one site.
type HazardRow struct {
	File  string   `json:"file"`
	Line  int      `json:"line"`
	Rule  string   `json:"rule"`
	Gated *bool    `json:"gated,omitempty"`
	Hints []string `json:"hints,omitempty"`
}

// HazardsOptions are the hazards flags.
type HazardsOptions struct {
	Root          string
	Gate          bool
	Baseline      string
	WriteBaseline string
}

// HazardBaseline is the ratchet file: per file and rule, how many sites
// were accepted. A count is used rather than lines so an edit above a site
// does not reopen it.
type HazardBaseline struct {
	Version int                       `json:"version"`
	Counts  map[string]map[string]int `json:"counts"`
}

var (
	repeatRe   = regexp.MustCompile(`withRepeat\(`)
	minusOneRe = regexp.MustCompile(`,\s*-1\b`)
	simpleRes  = map[string]*regexp.Regexp{
		HazardFrameCallback:   regexp.MustCompile(`\buseFrameCallback\(`),
		HazardPathValue:       regexp.MustCompile(`\busePathValue\(`),
		HazardClock:           regexp.MustCompile(`\buseClock\(`),
		HazardClippedSubviews: regexp.MustCompile(`\bremoveClippedSubviews\b`),
	}
	textureRe   = regexp.MustCompile(`\brenderToHardwareTextureAndroid\b`)
	useQueries  = regexp.MustCompile(`\buseQueries\(`)
	intlRe      = regexp.MustCompile(`^(\s+).*\bnew Intl\.[A-Z]`)
	scrollRe    = regexp.MustCompile(`<ScrollView\b`)
	listRe      = regexp.MustCompile(`\b(FlashList|FlatList|SectionList|LegendList)\b`)
	routeDirRe  = regexp.MustCompile(`(^|/)app/`)
	skipDirs    = map[string]bool{"node_modules": true, "ios": true, "android": true, "__tests__": true, "__mocks__": true, "dist": true, "build": true}
	testFileRes = regexp.MustCompile(`\.(test|spec)\.[jt]sx?$`)
)

// scanHazards scans one file's text.
func scanHazards(rel, text string, gates, hints []string) []HazardRow {
	var rows []HazardRow
	lineOf := func(off int) int { return strings.Count(text[:off], "\n") + 1 }
	gated := false
	for _, g := range gates {
		if strings.Contains(text, g) {
			gated = true
		}
	}
	var found []string
	for _, h := range hints {
		if regexp.MustCompile(`\b` + regexp.QuoteMeta(h) + `\b`).MatchString(text) {
			found = append(found, h)
		}
	}
	add := func(rule string, off int) {
		r := HazardRow{File: rel, Line: lineOf(off), Rule: rule}
		if loopRules[rule] {
			g := gated
			r.Gated = &g
			if gated {
				r.Hints = found
			}
		}
		rows = append(rows, r)
	}
	for _, m := range repeatRe.FindAllStringIndex(text, -1) {
		end := m[1] + 400
		if end > len(text) {
			end = len(text)
		}
		if minusOneRe.MatchString(text[m[1]:end]) {
			add(HazardInfiniteRepeat, m[0])
		}
	}
	for _, rule := range []string{HazardFrameCallback, HazardPathValue, HazardClock, HazardClippedSubviews} {
		for _, m := range simpleRes[rule].FindAllStringIndex(text, -1) {
			add(rule, m[0])
		}
	}
	for _, m := range textureRe.FindAllStringIndex(text, -1) {
		start := strings.LastIndex(text[:m[0]], "<")
		end := strings.Index(text[m[1]:], ">")
		if start < 0 || end < 0 {
			continue
		}
		if !strings.Contains(text[start:m[1]+end], "collapsable") {
			add(HazardTextureNoCollap, m[0])
		}
	}
	for _, m := range useQueries.FindAllStringIndex(text, -1) {
		if !strings.Contains(callArgs(text, m[1]), "combine") {
			add(HazardQueriesNoComb, m[0])
		}
	}
	if strings.HasSuffix(rel, ".tsx") {
		off := 0
		for _, line := range strings.SplitAfter(text, "\n") {
			if intlRe.MatchString(line) && !strings.Contains(line, "useMemo") {
				add(HazardIntlInRender, off)
			}
			off += len(line)
		}
	}
	if routeDirRe.MatchString(rel) && !listRe.MatchString(text) {
		if m := scrollRe.FindStringIndex(text); m != nil {
			add(HazardScrollViewRoute, m[0])
		}
	}
	return rows
}

// Hazards sweeps an app's TS/TSX sources for render-cost sites.
func (t *Tool) Hazards(o HazardsOptions) (Result, error) {
	root := o.Root
	var gates, hints, exclude []string
	if c := t.Config; c != nil {
		if root == "" {
			root = filepath.Join(t.ProjectDir, c.App.Root)
		}
		if h := c.Hazards; h != nil {
			gates, hints, exclude = h.AmbientGates, h.VisibilityHint, h.Exclude
		}
	}
	if root == "" {
		root = t.ProjectDir
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return Result{}, err
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return Result{}, diag(DiagUsage, root+" is not a directory", "perflab hazards <appRoot> --json")
	}
	rows := []HazardRow{}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if p != root && (skipDirs[name] || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !(strings.HasSuffix(name, ".ts") || strings.HasSuffix(name, ".tsx")) || strings.HasSuffix(name, ".d.ts") || testFileRes.MatchString(name) {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		for _, g := range exclude {
			if vconfig.MatchPath(g, rel) {
				return nil
			}
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil
		}
		rows = append(rows, scanHazards(rel, string(raw), gates, hints)...)
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].File != rows[j].File {
			return rows[i].File < rows[j].File
		}
		return rows[i].Line < rows[j].Line
	})
	counts := map[string]int{}
	current := map[string]map[string]int{}
	for _, r := range rows {
		counts[r.Rule]++
		if current[r.File] == nil {
			current[r.File] = map[string]int{}
		}
		current[r.File][r.Rule]++
	}
	res := Result{Data: map[string]any{"root": root, "rows": rows, "counts": counts}}
	ungated := 0
	for _, r := range rows {
		if r.Gated != nil && !*r.Gated {
			ungated++
		}
	}
	res.Lines = append(res.Lines, fmt.Sprintf("%d sites in %s (%d loops with no ambient gate in their file)", len(rows), root, ungated))
	for _, r := range rows {
		res.Lines = append(res.Lines, fmt.Sprintf("%s:%d %s", r.File, r.Line, r.Rule))
	}
	if o.WriteBaseline != "" {
		raw, _ := json.MarshalIndent(HazardBaseline{Version: 1, Counts: current}, "", "  ")
		if err := os.WriteFile(o.WriteBaseline, append(raw, '\n'), 0o644); err != nil {
			return Result{}, err
		}
		res.Lines = append(res.Lines, "baseline written: "+o.WriteBaseline)
	}
	if o.Gate {
		var base HazardBaseline
		if o.Baseline != "" {
			raw, err := os.ReadFile(o.Baseline)
			if err != nil {
				return Result{}, diag(DiagUsage, "cannot read --baseline: "+err.Error(), "perflab hazards --write-baseline "+o.Baseline+" --json")
			}
			if err := json.Unmarshal(raw, &base); err != nil {
				return Result{}, diag(DiagConfigInvalid, o.Baseline+" is not a hazards baseline: "+err.Error(), "perflab hazards --write-baseline "+o.Baseline+" --json")
			}
		}
		for _, file := range sortedKeys(current) {
			for _, rule := range sortedKeys(current[file]) {
				if extra := current[file][rule] - base.Counts[file][rule]; extra > 0 {
					res.Diagnostics = append(res.Diagnostics, errDiag(DiagHazardNew, fmt.Sprintf("%s: %d new %s site(s) beyond the baseline", file, extra, rule),
						"review the site; to accept it: perflab hazards --write-baseline "+orElse(o.Baseline, "<file>")+" --json"))
				}
			}
		}
	}
	return res, nil
}

// callArgs is the text from just after a call's '(' to its matching ')'
// (the rest of the file when unbalanced); strings are not parsed.
func callArgs(text string, from int) string {
	depth := 1
	for i := from; i < len(text); i++ {
		switch text[i] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 {
				return text[from:i]
			}
		}
	}
	return text[from:]
}

func orElse(s, d string) string {
	if s == "" {
		return d
	}
	return s
}
