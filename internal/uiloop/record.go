package uiloop

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// RecordVersion is the shot record's version (harness/capture.ts RECORD_VERSION).
const RecordVersion = 1

// Record is the part of a shot record (<passDir>/shots/<id>/<viewport>.<theme>.json,
// harness/capture.ts ShotRecord) that split, publish and scoreboard read. The
// record carries more; unknown fields are ignored on purpose.
type Record struct {
	V        int    `json:"v"`
	Pass     int    `json:"pass"`
	Order    int    `json:"order"`
	ID       string `json:"id"`
	App      string `json:"app"`
	Area     string `json:"area"`
	Kind     string `json:"kind"`
	State    string `json:"state"`
	Title    string `json:"title"`
	Route    string `json:"route"`
	URL      string `json:"url"`
	As       string `json:"as"`
	Viewport string `json:"viewport"`
	Size     struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"size"`
	Theme   string `json:"theme"`
	Status  string `json:"status"`
	Failure *struct {
		Step      string `json:"step"`
		StepIndex *int   `json:"stepIndex"`
		Error     string `json:"error"`
	} `json:"failure"`
	Files struct {
		Viewport string `json:"viewport"`
		Full     string `json:"full"`
	} `json:"files"`
	Lint *struct {
		Defects map[string]int `json:"defects"`
		Info    map[string]int `json:"info"`
		// Distinct: defect rule → the distinct element path + detail it hit
		// on this shot; nil in records of a harness before v0.23.
		Distinct map[string][]LintKey `json:"distinct"`
	} `json:"lint"`
	ConsoleErrors []string `json:"consoleErrors"`
	// CapturedAt is when the shot's test started (ISO 8601, the box's clock).
	CapturedAt  string   `json:"capturedAt"`
	SourceFiles []string `json:"sourceFiles"`

	// Dir is the record's directory relative to the pass directory (set on load).
	Dir string `json:"-"`
}

// LintKey is one defect's identity across a pass (harness/lint.ts LintKey).
type LintKey struct {
	Path   string `json:"path"`
	Detail string `json:"detail"`
}

// Defects sums the record's lint defects.
func (r Record) Defects() int {
	n := 0
	if r.Lint != nil {
		for _, v := range r.Lint.Defects {
			n += v
		}
	}
	return n
}

// Key identifies one shot within a pass.
func (r Record) Key() string { return r.ID + "@" + r.Viewport + "." + r.Theme }

// LoadRecords reads every shot record of a pass directory, sorted by
// manifest order, id, viewport, theme. A record cut off mid-write (a killed
// run) is skipped: the next --resume retakes it.
func LoadRecords(passDir string) ([]Record, error) {
	paths, err := filepath.Glob(filepath.Join(passDir, "shots", "*", "*.json"))
	if err != nil {
		return nil, err
	}
	var records []Record
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var r Record
		if err := json.Unmarshal(b, &r); err != nil {
			var syntax *json.SyntaxError
			if errors.As(err, &syntax) || errors.Is(err, io.ErrUnexpectedEOF) {
				continue
			}
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if r.V != RecordVersion {
			return nil, fmt.Errorf("%s: record v%d, this vybava reads v%d — sync the harness and re-run the pass", p, r.V, RecordVersion)
		}
		r.Dir = filepath.ToSlash(filepath.Join("shots", filepath.Base(filepath.Dir(p))))
		records = append(records, r)
	}
	sort.SliceStable(records, func(i, j int) bool {
		a, b := records[i], records[j]
		if a.Order != b.Order {
			return a.Order < b.Order
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		if a.Viewport != b.Viewport {
			return a.Viewport < b.Viewport
		}
		return a.Theme < b.Theme
	})
	return records, nil
}
