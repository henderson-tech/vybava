package readeff

import (
	"encoding/json"
	"math"
	"slices"
	"strings"

	"github.com/henderson-tech/vybava/internal/transcripts"
)

// compactTool marks a compaction in a session's calls: the model no longer
// holds what it read before it, so a later read is not a re-read.
const compactTool = "(compact)"

// wholeRead is the scan option every stateless read uses: the whole file in
// one sweep, oversize records (huge tool output) stepped over.
var wholeRead = transcripts.ScanOptions{Budget: math.MaxInt64, SkipOversize: true}

// readClaude builds a session from one Claude Code transcript. A call pairs
// with its result by tool_use id and keeps its place in invocation order; a
// call whose result never came is dropped.
func readClaude(path string) (Session, error) {
	s := Session{Agent: "claude"}
	type pending struct {
		name  string
		input json.RawMessage
		cwd   string
		at    int // its place in s.Calls: results arrive in any order
	}
	open := map[string]pending{}
	res, err := transcripts.Scan(path, transcripts.Cursor{}, false, wholeRead, func(line []byte, _ int64) error {
		if !transcripts.ClaudeToolLine(line) && !transcripts.ClaudeCompactLine(line) {
			return nil
		}
		rec, err := transcripts.DecodeClaudeTool(line)
		if err != nil {
			return nil // a torn or foreign line carries no call we can pair
		}
		if s.ID == "" {
			s.ID, s.Start = rec.SessionID, rec.Timestamp
		}
		if s.Repo == "" && rec.Cwd != "" {
			s.Repo, _ = transcripts.GitRoot(rec.Cwd)
		}
		switch {
		case rec.Type == "system" && rec.Subtype == "compact_boundary":
			s.Calls = append(s.Calls, Call{Tool: compactTool, Class: ClassOther})
		case rec.Type == "assistant":
			for _, u := range rec.Message.ToolUses() {
				s.Calls = append(s.Calls, Call{open: true})
				open[u.ID] = pending{name: u.Name, input: u.Input, cwd: rec.Cwd, at: len(s.Calls) - 1}
			}
		case rec.Type == "user":
			outcome, _ := rec.Outcome()
			for _, r := range rec.Message.ToolResults() {
				p, ok := open[r.UseID]
				if !ok {
					continue
				}
				delete(open, r.UseID)
				s.Calls[p.at] = claudeCall(p.name, p.input, p.cwd, r, outcome)
			}
		}
		return nil
	})
	s.Calls = slices.DeleteFunc(s.Calls, func(c Call) bool { return c.open })
	s.Oversize = res.Oversize
	return s, err
}

// claudeCall reduces one paired call and result.
func claudeCall(name string, input json.RawMessage, cwd string, r transcripts.ToolResult, out transcripts.ClaudeToolOutcome) Call {
	lines := countLines(r.Text)
	if rule := guardRule(r.Text); r.IsError && rule != "" {
		return Call{Tool: name, Class: ClassOther, Lines: lines, Blocked: rule}
	}
	var in struct {
		FilePath string `json:"file_path"`
		Command  string `json:"command"`
	}
	_ = json.Unmarshal(input, &in) // an unexpected input shape leaves the fields empty
	c := Call{Tool: name, Lines: lines, Visible: 1}
	switch name {
	case "Read":
		if f := out.File; f != nil {
			c.Read, c.Lines = true, f.NumLines
			c.Spans = []Span{{Path: f.FilePath, Start: max(f.StartLine, 1), N: f.NumLines, Total: f.TotalLines,
				Whole: f.StartLine <= 1 && f.NumLines >= f.TotalLines}}
		}
	case "Grep", "Glob":
		c.Search, c.Query, c.dir = true, string(input), cwd
	case "Edit", "MultiEdit", "Write", "NotebookEdit":
		if r.IsError {
			break
		}
		c.Edit, c.Stale = true, out.StaleRecovered
		c.Edited = []string{firstNonEmpty(out.FilePath, in.FilePath)}
		for _, h := range out.StructuredPatch {
			for _, l := range h.Lines {
				if strings.HasPrefix(l, "+") || strings.HasPrefix(l, "-") {
					c.Changed++
				}
			}
		}
		if out.Type == "create" && len(out.StructuredPatch) == 0 {
			c.Changed = countLines(out.Content)
		}
	case "Bash":
		c = shellCall(in.Command, cwd)
		c.Tool, c.Lines, c.Stale = name, lines, out.StaleHint != ""
	}
	c.classify()
	return settle(c, r.Text, r.IsError)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
