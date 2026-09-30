package polishkit

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/henderson-tech/vybava/internal/runx"
)

// ReportOptions are the report verb's flags.
type ReportOptions struct {
	Pass int
	// Previous: 0 = the pass before, when it exists; -1 = no delta.
	Previous int
}

// Delta is the change versus the previous pass, by cell id.
type Delta struct {
	Previous  int      `json:"previous"`
	Fixed     []string `json:"fixed"`
	Regressed []string `json:"regressed"`
	New       []string `json:"new"`
}

// ReportData is what report returns; Markdown is also written to report.md.
type ReportData struct {
	Pass     int            `json:"pass"`
	File     string         `json:"file"`
	Counts   map[string]int `json:"counts"`
	Failing  []string       `json:"failing"`
	Delta    *Delta         `json:"delta,omitempty"`
	Markdown string         `json:"markdown"`
}

// Verdict glyphs (no long dashes: the skip glyph is a plain hyphen).
var glyph = map[string]string{VerdictPass: "✓", VerdictFail: "✗", VerdictSkip: "-", VerdictPending: "·"}

// Report renders the pass as Markdown: a table per lane (screen x state),
// the matrix cells, the failing cells with their shots and findings, and
// the delta versus the previous pass.
func (t *Tool) Report(opts ReportOptions) (Result, error) {
	run, err := t.LoadRun(opts.Pass)
	if err != nil {
		return Result{}, err
	}
	var prev *RunFile
	switch {
	case opts.Previous > 0:
		prev, err = t.LoadRun(opts.Previous)
		if err != nil {
			return Result{}, err
		}
	case opts.Previous == 0:
		for _, p := range t.Passes() {
			if p < run.Pass {
				if r, err := t.LoadRun(p); err == nil {
					prev = r
				}
			}
		}
	}
	data := ReportData{Pass: run.Pass, File: filepath.Join(run.PassDir, "report.md"), Counts: map[string]int{}, Failing: []string{}}
	for _, v := range append([]string{VerdictPending}, Verdicts...) {
		data.Counts[v] = 0
	}
	for _, c := range run.Cells {
		data.Counts[c.Verdict]++
		if c.Verdict == VerdictFail {
			data.Failing = append(data.Failing, c.ID)
		}
	}
	if prev != nil {
		data.Delta = ComputeDelta(prev, run)
	}
	data.Markdown = RenderReport(run, data)
	if err := os.WriteFile(data.File, []byte(data.Markdown), 0o644); err != nil {
		return Result{}, err
	}
	res := Result{Data: data, Lines: strings.Split(strings.TrimRight(data.Markdown, "\n"), "\n")}
	if data.Counts[VerdictPending] > 0 {
		res.Diagnostics = append(res.Diagnostics, warn(DiagShotRequired, fmt.Sprintf("%d cells are still pending; the report marks them ·", data.Counts[VerdictPending]), fmt.Sprintf("polish-kit status --pass %d --json", run.Pass)))
		res.Next = []string{fmt.Sprintf("polish-kit status --pass %d --json", run.Pass)}
	} else if data.Counts[VerdictFail] > 0 {
		res.Next = []string{fmt.Sprintf("polish-kit run init --pass %d --json", run.Pass+1)}
	}
	return res, nil
}

// ComputeDelta compares verdicts by cell id.
func ComputeDelta(prev, cur *RunFile) *Delta {
	d := &Delta{Previous: prev.Pass, Fixed: []string{}, Regressed: []string{}, New: []string{}}
	before := map[string]string{}
	for _, c := range prev.Cells {
		before[c.ID] = c.Verdict
	}
	for _, c := range cur.Cells {
		was, seen := before[c.ID]
		switch {
		case !seen && c.Verdict == VerdictFail:
			d.New = append(d.New, c.ID)
		case seen && was == VerdictFail && c.Verdict == VerdictPass:
			d.Fixed = append(d.Fixed, c.ID)
		case seen && was == VerdictPass && c.Verdict == VerdictFail:
			d.Regressed = append(d.Regressed, c.ID)
		}
	}
	return d
}

// RenderReport is the pure Markdown renderer.
func RenderReport(run *RunFile, data ReportData) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# Polish pass %d\n\n", run.Pass)
	targets := make([]string, 0, len(run.Plan.Targets))
	for _, pt := range run.Plan.Targets {
		targets = append(targets, fmt.Sprintf("%s (%d files, %s)", pt.ID, len(pt.Files), pt.Reason))
	}
	fmt.Fprintf(&sb, "- intensity: %s\n- base: %s\n- targets: %s\n", run.Plan.Intensity, run.Plan.Base, strings.Join(targets, ", "))
	if run.Plan.Findings != "" {
		fmt.Fprintf(&sb, "- findings: %s\n", run.Plan.Findings)
	}
	fmt.Fprintf(&sb, "- cells: %d pass, %d fail, %d skip, %d pending\n\n", data.Counts[VerdictPass], data.Counts[VerdictFail], data.Counts[VerdictSkip], data.Counts[VerdictPending])

	for _, l := range run.Lanes {
		var states []SheetState
		byKey := map[string]Cell{}
		var screens []string
		for _, c := range run.Cells {
			if c.Kind != CellChrome || c.Lane != l.ID {
				continue
			}
			st := SheetState{Theme: c.Theme, Nav: c.Nav, TextSize: c.TextSize}
			if !slices.Contains(states, st) {
				states = append(states, st)
			}
			if !slices.Contains(screens, c.Screen) {
				screens = append(screens, c.Screen)
			}
			byKey[c.Screen+"|"+st.Label()] = c
		}
		if len(states) == 0 {
			continue
		}
		fmt.Fprintf(&sb, "## Lane %s (%s)\n\n| Screen |", l.ID, l.Kind)
		for _, st := range states {
			fmt.Fprintf(&sb, " %s |", st.Label())
		}
		sb.WriteString("\n|---|")
		for range states {
			sb.WriteString("---|")
		}
		sb.WriteString("\n")
		for _, s := range screens {
			fmt.Fprintf(&sb, "| %s |", screenTitle(run, s))
			for _, st := range states {
				c, ok := byKey[s+"|"+st.Label()]
				if !ok {
					sb.WriteString("  |")
					continue
				}
				fmt.Fprintf(&sb, " %s |", glyph[c.Verdict])
			}
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	var matrix []Cell
	for _, c := range run.Cells {
		if c.Kind == CellMatrix {
			matrix = append(matrix, c)
		}
	}
	if len(matrix) > 0 {
		sb.WriteString("## Matrix\n\n| Lane | Flow | Tier | Verdict | Note | Finding |\n|---|---|---|---|---|---|\n")
		for _, c := range matrix {
			fmt.Fprintf(&sb, "| %s | %s | %s | %s %s | %s | %s |\n", c.Lane, cellText(c.Flow), cellText(c.Tier), glyph[c.Verdict], c.Verdict, cellText(c.Note), cellText(c.Finding))
		}
		sb.WriteString("\n")
	}

	sb.WriteString("## Failing cells\n\n")
	if len(data.Failing) == 0 {
		sb.WriteString("none\n\n")
	}
	for _, c := range run.Cells {
		if c.Verdict != VerdictFail {
			continue
		}
		fmt.Fprintf(&sb, "- `%s`", c.ID)
		if c.Note != "" {
			fmt.Fprintf(&sb, ": %s", c.Note)
		}
		if c.Finding != "" {
			fmt.Fprintf(&sb, " (finding: %s)", c.Finding)
		}
		if c.Shot != "" {
			fmt.Fprintf(&sb, " - shot `%s`", c.Shot)
		}
		sb.WriteString("\n")
	}
	if len(data.Failing) > 0 {
		sb.WriteString("\n")
	}

	if data.Delta != nil {
		fmt.Fprintf(&sb, "## Delta vs pass %d\n\n- fixed: %s\n- regressed: %s\n- new: %s\n", data.Delta.Previous, listOrNone(data.Delta.Fixed), listOrNone(data.Delta.Regressed), listOrNone(data.Delta.New))
	} else {
		sb.WriteString("## Delta\n\nno previous pass\n")
	}
	return sb.String()
}

func screenTitle(run *RunFile, id string) string {
	if s, ok := screenOf(run, id); ok && s.Title != "" {
		return s.Title
	}
	return id
}

func cellText(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", "\\|"), "\n", " ")
}

func listOrNone(ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	quoted := make([]string, len(ids))
	for i, id := range ids {
		quoted[i] = "`" + id + "`"
	}
	return strings.Join(quoted, ", ")
}

// errorsAs is errors.As under the name the lane resolver uses.
func errorsAs(err error, target *runx.DiagError) bool { return errors.As(err, target) }
