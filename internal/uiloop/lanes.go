package uiloop

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// lanesFile is <pass>/fix/lanes.json; it sits beside the checkpoints and is
// never read as one.
const lanesFile = "lanes.json"

// Lane is one fix lane: the directories it owns and its items, in order.
type Lane struct {
	Lane       string         `json:"lane"`
	Dirs       []string       `json:"dirs"`
	Keys       []string       `json:"keys"`
	BySeverity map[string]int `json:"bySeverity"`
}

// LanesData is `ui-loop lanes`, also written to <pass>/fix/lanes.json.
type LanesData struct {
	V       int    `json:"v"`
	Pass    int    `json:"pass"`
	PassDir string `json:"passDir"`
	File    string `json:"file"`
	// Primitives run first; then their dirs are frozen and the area lanes run.
	Primitives []Lane   `json:"primitives"`
	Areas      []Lane   `json:"areas"`
	Frozen     []string `json:"frozen"`
	Foreign    []string `json:"foreign"` // items with no in-repo, non-i18n file
	I18n       []string `json:"i18n"`    // items whose only in-repo files are i18n catalogs
	Open       int      `json:"open"`
	Finished   int      `json:"finished"` // open items with a finishing checkpoint, left out of the lanes
	Primitive  []string `json:"primitivePrefixes"`
	Max        int      `json:"max"`
}

// LanesOptions are `lanes`' flags.
type LanesOptions struct {
	Pass       int
	Primitives []string // directory prefixes that hold shared primitives (nil: the config's)
	Max        int      // lanes per phase (default 4)
}

// isI18n reports whether a repo-relative path is a translation catalog: a
// .json file inside a directory named i18n. Catalogs are owned by no lane.
func isI18n(p string) bool {
	if path.Ext(p) != ".json" {
		return false
	}
	parts := strings.Split(path.Dir(p), "/")
	for _, s := range parts {
		if s == "i18n" {
			return true
		}
	}
	return false
}

// repoRel makes a finding's file repo-relative; ok is false for a file
// outside the repo (an absolute path elsewhere, or one that climbs out).
func repoRel(root, file string) (string, bool) {
	file = strings.TrimSpace(file)
	if file == "" {
		return "", false
	}
	if filepath.IsAbs(file) {
		rel, err := filepath.Rel(root, file)
		if err != nil {
			return "", false
		}
		file = rel
	}
	rel := path.Clean(filepath.ToSlash(file))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || strings.HasPrefix(rel, "/") {
		return "", false
	}
	return rel, true
}

// underPrefix reports whether dir is a primitives prefix or inside one.
func underPrefix(dir string, prefixes []string) bool {
	for _, p := range prefixes {
		p = strings.TrimRight(path.Clean(filepath.ToSlash(p)), "/")
		if p != "" && p != "." && (dir == p || strings.HasPrefix(dir, p+"/")) {
			return true
		}
	}
	return false
}

// PlanLanes plans the fix lanes by OWNERSHIP. Each open item's home is the
// directory of its first in-repo, non-i18n file; items group by home, and a
// group is primitive when its dir is under a primitives prefix or items of
// two areas share it. Groups pack greedily (biggest first, onto the lightest
// lane) into at most max lanes per phase. A lane owns the files directly in
// its dirs — not their subdirectories — so two lanes never edit one file.
// Classification uses every open item, so the frozen dirs are stable across
// a resumed round; packing uses only the items still without a finishing
// checkpoint.
func PlanLanes(root string, findings []Finding, finished map[string]bool, prefixes []string, max int) LanesData {
	if max < 1 {
		max = 4
	}
	type group struct {
		dir   string
		areas map[string]bool
		items []Finding
	}
	groups := map[string]*group{}
	out := LanesData{V: 1, Primitives: []Lane{}, Areas: []Lane{}, Frozen: []string{}, Foreign: []string{}, I18n: []string{}, Primitive: prefixes, Max: max}
	if out.Primitive == nil {
		out.Primitive = []string{}
	}
	for _, f := range findings {
		if !f.Open() {
			continue
		}
		out.Open++
		home, catalog := "", false
		for _, file := range f.Files {
			rel, ok := repoRel(root, file)
			if !ok {
				continue
			}
			if isI18n(rel) {
				catalog = true
				continue
			}
			home = path.Dir(rel)
			break
		}
		if home == "" {
			if finished[f.Key] {
				out.Finished++
			} else if catalog {
				out.I18n = append(out.I18n, f.Key)
			} else {
				out.Foreign = append(out.Foreign, f.Key)
			}
			continue
		}
		g := groups[home]
		if g == nil {
			g = &group{dir: home, areas: map[string]bool{}}
			groups[home] = g
		}
		g.areas[f.Area] = true
		g.items = append(g.items, f)
	}
	var prim, area []*group
	for _, g := range groups {
		if underPrefix(g.dir, prefixes) || len(g.areas) > 1 {
			prim = append(prim, g)
			out.Frozen = append(out.Frozen, g.dir)
		} else {
			area = append(area, g)
		}
	}
	sort.Strings(out.Frozen)
	pack := func(gs []*group, prefix string) []Lane {
		type bin struct {
			dirs  []string
			items []Finding
		}
		type todo struct {
			dir   string
			items []Finding
		}
		var work []todo
		for _, g := range gs {
			var items []Finding
			for _, f := range g.items {
				if finished[f.Key] {
					out.Finished++
					continue
				}
				items = append(items, f)
			}
			if len(items) > 0 {
				work = append(work, todo{g.dir, items})
			}
		}
		sort.Slice(work, func(i, j int) bool {
			if len(work[i].items) != len(work[j].items) {
				return len(work[i].items) > len(work[j].items)
			}
			return work[i].dir < work[j].dir
		})
		bins := make([]bin, min(max, len(work)))
		for _, w := range work {
			light := 0
			for i := range bins {
				if len(bins[i].items) < len(bins[light].items) {
					light = i
				}
			}
			bins[light].dirs = append(bins[light].dirs, w.dir)
			bins[light].items = append(bins[light].items, w.items...)
		}
		lanes := []Lane{}
		for i, b := range bins {
			orderLaneItems(b.items)
			l := Lane{Lane: fmt.Sprintf("%s-%d", prefix, i+1), Dirs: b.dirs, BySeverity: map[string]int{}}
			sort.Strings(l.Dirs)
			for _, f := range b.items {
				l.Keys = append(l.Keys, f.Key)
				l.BySeverity[f.Severity]++
			}
			lanes = append(lanes, l)
		}
		return lanes
	}
	out.Primitives = pack(prim, "prim")
	out.Areas = pack(area, "area")
	return out
}

// orderLaneItems sorts a lane: worst severity first, carried items
// (not-met, partly) before fresh ones, then key.
func orderLaneItems(items []Finding) {
	carried := func(f Finding) int {
		if f.Status == "open" {
			return 0
		}
		return 1
	}
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if severityRank[a.Severity] != severityRank[b.Severity] {
			return severityRank[a.Severity] > severityRank[b.Severity]
		}
		if carried(a) != carried(b) {
			return carried(a) > carried(b)
		}
		return a.Key < b.Key
	})
}

// Lanes plans the pass's fix lanes from review/backlog.json and writes
// fix/lanes.json: `ui-loop lanes`.
func (t *Tool) Lanes(o LanesOptions) (Result, error) {
	pass, err := t.resolveShotPass(o.Pass)
	if err != nil {
		return Result{}, err
	}
	backlog, err := t.loadPassBacklog(pass)
	if err != nil {
		return Result{}, err
	}
	if backlog == nil {
		return Result{}, diag(DiagBacklogInvalid, t.PassDir(pass)+"/review/backlog.json does not exist", "run the review stage (merge-review, then synthesis) first")
	}
	checkpoints, diags, err := t.loadCheckpoints(pass)
	if err != nil {
		return Result{}, err
	}
	finished := map[string]bool{}
	for _, c := range checkpoints {
		if c.Finishes() {
			finished[c.Key] = true
		}
	}
	if o.Primitives == nil {
		o.Primitives = t.Config.Primitives
	}
	data := PlanLanes(t.Root, backlog.Findings, finished, o.Primitives, o.Max)
	data.Pass, data.PassDir, data.File = pass, t.PassDir(pass), t.PassDir(pass)+"/fix/"+lanesFile
	if err := writeJSON(filepath.Join(t.passAbs(pass), "fix", lanesFile), data); err != nil {
		return Result{}, err
	}
	if len(data.Foreign) > 0 {
		diags = append(diags, warn(DiagForeignItems, fmt.Sprintf("%d open items name no file in this repo: %s", len(data.Foreign), strings.Join(data.Foreign, ", ")),
			"checkpoint each blocked with where its fix lands (another repo), or repair its files in backlog.json"))
	}
	return Result{Data: data, Diagnostics: diags}, nil
}
