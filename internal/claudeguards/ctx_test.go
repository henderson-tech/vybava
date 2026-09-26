package claudeguards

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiagnoseContext(t *testing.T) {
	header := make([]byte, 24)
	copy(header, "\x89PNG\r\n\x1a\n")
	binary.BigEndian.PutUint32(header[16:], 1280)
	binary.BigEndian.PutUint32(header[20:], 900)
	row := `{"type":"assistant","timestamp":"2026-09-11T10:00:00Z","message":{"id":"one","usage":{"input_tokens":10,"cache_read_input_tokens":499990,"output_tokens":100,"output_tokens_details":{"thinking_tokens":20}},"content":[{"type":"tool_use","id":"read1","name":"Read"}]}}`
	result := fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"text","text":"Open vitrinka todos"},{"type":"tool_result","tool_use_id":"read1","content":[{"type":"text","text":"some result"},{"type":"image","source":{"data":%q}}]}]}}`, base64.StdEncoding.EncodeToString(header))
	p := filepath.Join(t.TempDir(), "abc.jsonl")
	if err := os.WriteFile(p, []byte(row+"\n"+row+"\n"+result+"\n{partial"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := DiagnoseContext(p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Responses != 1 || r.OutputTokens != 100 || r.ThinkingTokens != 20 || r.MaxContext != 500000 || r.TodoInjections != 1 || r.MalformedRecords != 1 {
		t.Fatalf("report: %+v", r)
	}
	if len(r.Images) != 1 || r.Images[0].EstimatedTokens != 1536 {
		t.Fatal(r.Images)
	}
	if got, err := ResolveTranscript(filepath.Dir(p), "abc"); err != nil || got != p {
		t.Fatal(got, err)
	}
	if r.Kind != "session" {
		t.Fatalf("kind = %q, want session", r.Kind)
	}
}

// Agent transcripts were unresolvable by any selector, so `ctx` could not
// measure 40% of the transcript tree — exactly where delegated context hides.
func TestResolveTranscriptReachesAgentsByExplicitSelector(t *testing.T) {
	root := t.TempDir()
	subagent := filepath.Join(root, "slug", "abc", "subagents", "agent-1.jsonl")
	wfAgent := filepath.Join(root, "slug", "abc", "subagents", "workflows", "wf_x", "agent-2.jsonl")
	for _, p := range []string{subagent, wfAgent} {
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for selector, want := range map[string]string{"agent-1": subagent, "agent-2": wfAgent} {
		got, err := ResolveTranscript(root, selector)
		if err != nil || got != want {
			t.Fatalf("%s = %q (%v), want %q", selector, got, err, want)
		}
	}
}

// A subagent transcript carries the same record shape as a session, so the same
// parse measures it; the report must say which kind it measured so a corpus
// aggregator never averages a lead session in with the agents it spawned.
func TestDiagnoseContextMeasuresAgentTranscripts(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "slug", "abc", "subagents", "agent-1.jsonl")
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	row := `{"type":"assistant","isSidechain":true,"timestamp":"2026-09-11T10:00:00Z","message":{"id":"one","usage":{"input_tokens":10,"cache_read_input_tokens":40000,"output_tokens":70}}}`
	if err := os.WriteFile(p, []byte(row+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := DiagnoseContext(p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Responses != 1 || r.MaxContext != 40010 || r.OutputTokens != 70 {
		t.Fatalf("report: %+v", r)
	}
	if r.Kind != "subagent" {
		t.Fatalf("kind = %q, want subagent", r.Kind)
	}
	wf := filepath.Join(root, "slug", "abc", "subagents", "workflows", "wf_x", "agent-2.jsonl")
	if err := os.MkdirAll(filepath.Dir(wf), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wf, []byte(row+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := DiagnoseContext(wf)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "workflow-agent" {
		t.Fatalf("kind = %q, want workflow-agent", got.Kind)
	}
}

// Only PNG headers were read, so every screenshot in another format was priced
// at zero and the image total came out far below the truth.
func TestImageCostByFormat(t *testing.T) {
	gif := append([]byte("GIF89a"), 0x00, 0x05, 0x2c, 0x01) // 1280x300
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xC0, 0x00, 0x11, 0x08, 0x03, 0x84, 0x05, 0x00, 0x00, 0x00}
	for name, data := range map[string][]byte{"gif": gif, "jpeg": jpeg} {
		got := imageCost(base64.StdEncoding.EncodeToString(data))
		if got.Width != 1280 || got.EstimatedTokens == 0 {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	if got := imageCost(base64.StdEncoding.EncodeToString([]byte("not an image at all"))); got.EstimatedTokens != unknownImageTokens {
		t.Fatalf("an unreadable header must not cost zero: %+v", got)
	}
}

// `latest` walked into subagent transcripts, reporting another agent's few
// hundred tokens as this session's context.
func TestResolveTranscriptSkipsSubagents(t *testing.T) {
	root := t.TempDir()
	session := filepath.Join(root, "slug", "abc.jsonl")
	subagent := filepath.Join(root, "slug", "abc", "subagents", "agent-1.jsonl")
	for _, p := range []string{session, subagent} { // subagent is written last, so it is newest
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := ResolveTranscript(root, "latest"); err != nil || got != session {
		t.Fatalf("latest = %q (%v), want the session transcript", got, err)
	}
}

// A leak scan that could not read every file must say so: zero spans from a
// partial scan is not "no leaks".
func TestContextReportRendersAnIncompleteLeakScan(t *testing.T) {
	var out strings.Builder
	r := ContextReport{Leaks: &Leaks{Unscanned: 2, Counts: map[string]int{}, Session: "sess-x"}}
	if err := r.Render(&out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "2 files not scanned") {
		t.Errorf("render = %q", out.String())
	}
}
