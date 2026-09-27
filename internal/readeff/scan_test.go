package readeff

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/transcripts"
)

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Both agents' tool traffic pairs into calls, and a scoped scan keeps only
// the repository's sessions.
func TestScanPairsBothAgents(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, _ := transcripts.GitRoot(repo)
	claude, codex := t.TempDir(), t.TempDir()
	writeLines(t, filepath.Join(claude, claudeSlug(repo), "s1.jsonl"),
		`{"type":"assistant","sessionId":"s1","cwd":"`+repo+`","timestamp":"2026-09-20T10:00:00Z","message":{"content":[{"type":"tool_use","id":"a","name":"Bash","input":{"command":"sed -n '1,3p' a.go"}}]}}`,
		`{"type":"user","sessionId":"s1","cwd":"`+repo+`","message":{"content":[{"type":"tool_result","tool_use_id":"a","content":"x\ny\nz"}]},"toolUseResult":{"stdout":"x\ny\nz","stderr":""}}`,
		`{"type":"assistant","sessionId":"s1","cwd":"`+repo+`","message":{"content":[{"type":"tool_use","id":"b","name":"Edit","input":{"file_path":"`+repo+`/a.go"}}]}}`,
		`{"type":"user","sessionId":"s1","cwd":"`+repo+`","message":{"content":[{"type":"tool_result","tool_use_id":"b","content":"ok"}]},"toolUseResult":{"filePath":"`+repo+`/a.go","structuredPatch":[{"lines":[" a","-b","+c"]}]}}`,
	)
	writeLines(t, filepath.Join(claude, "-elsewhere", "s2.jsonl"),
		`{"type":"assistant","sessionId":"s2","cwd":"/elsewhere","message":{"content":[{"type":"tool_use","id":"c","name":"Read","input":{}}]}}`,
		`{"type":"user","sessionId":"s2","message":{"content":[{"type":"tool_result","tool_use_id":"c","content":"q"}]}}`,
	)
	writeLines(t, filepath.Join(codex, "sessions", "2026", "09", "20", "rollout-x.jsonl"),
		`{"type":"session_meta","payload":{"id":"t1","timestamp":"2026-09-20T11:00:00Z","cwd":"`+repo+`"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"k","input":"await tools.exec_command({cmd:\"rg -n foo\"})"}}`,
		`{"type":"response_item","payload":{"type":"function_call","name":"exec_command","call_id":"m","arguments":"{\"cmd\":\"cat a.go\"}"}}`,
		// a parallel batch: the later call finishes first
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"m","output":"x\n"}}`,
		`{"type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"k","output":[{"type":"input_text","text":"Script completed\nOutput:\n"},{"type":"input_text","text":"a.go:3:foo\n"}]}}`,
	)
	// a subagent record without a sessionId takes its parent's from the path
	writeLines(t, filepath.Join(claude, claudeSlug(repo), "s1", "subagents", "agent-a.jsonl"),
		`{"type":"assistant","cwd":"`+repo+`","message":{"content":[{"type":"tool_use","id":"z","name":"Bash","input":{"command":"ls"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"z","content":"a.go"}]}}`,
	)
	var got []Session
	warnings, err := Scan(Options{ClaudeRoot: claude, CodexDir: codex, Since: time.Time{}, Repo: root}, func(s Session) { got = append(got, s) })
	if err != nil || len(warnings) > 0 {
		t.Fatalf("Scan: %v %v", err, warnings)
	}
	if len(got) != 3 {
		t.Fatalf("sessions = %d, want the repository's 3", len(got))
	}
	slices.SortFunc(got, func(a, b Session) int { return strings.Compare(a.ID, b.ID) })
	c, sub, x := got[0], got[1], got[2]
	if sub.ID != "s1/agent-a" {
		t.Errorf("subagent id = %q, want s1/agent-a", sub.ID)
	}
	if c.Agent != "claude" || len(c.Calls) != 2 || c.Calls[0].Class != ClassRead || c.Calls[0].Lines != 3 ||
		c.Calls[0].Spans[0] != (Span{Path: repo + "/a.go", Start: 1, N: 3}) || c.Calls[1].Changed != 2 {
		t.Errorf("claude session = %+v", c)
	}
	if x.Agent != "codex" || len(x.Calls) != 2 || x.Calls[0].Class != ClassSearch || x.Calls[1].Class != ClassRead || x.Calls[0].Lines != 1 ||
		len(x.Calls[0].Found) != 1 || x.Calls[0].Found[0] != repo+"/a.go" {
		t.Errorf("codex session = %+v", x)
	}
	// A session id picks its transcript by name, before anything is decoded.
	jobs, err := scanJobs(Options{ClaudeRoot: claude, CodexDir: codex, Session: "s1"})
	if err != nil || len(jobs) != 2 || !strings.Contains(jobs[0].path+jobs[1].path, "/s1.jsonl") {
		t.Errorf("jobs for s1 = %v %v, want s1.jsonl and its subagent only", jobs, err)
	}
}
