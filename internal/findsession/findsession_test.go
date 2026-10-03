package findsession

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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

// The ending as the terminal rendered it — inline code and bold dropped,
// lines wrapped, TUI chrome after it — finds the session that wrote it
// ahead of a later one that only had it pasted in.
func TestFindRenderedEndingPrefersAuthor(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	cwd := filepath.Join(home, "repo")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	reply := `**Trap for that deploy:** once the fix is live, delete those eighteen template copies, or customers will see ` +
		"`482913##`" + `. I have left this note as a comment on the pull request so whoever ships it removes them first.`
	encoded := strings.ReplaceAll(reply, `"`, `\"`)
	writeLines(t, filepath.Join(root, "-repo", "aaaaaaaa-0000-0000-0000-000000000001.jsonl"),
		`{"type":"user","cwd":"`+cwd+`","gitBranch":"main","timestamp":"2026-10-01T08:00:00Z","origin":{"kind":"human"},"message":{"content":"add the hash"}}`,
		`{"type":"attachment","attachment":{"type":"ultra_effort_enter"},"timestamp":"2026-10-01T08:00:01Z"}`,
		`{"type":"assistant","effort":"xhigh","timestamp":"2026-10-01T09:00:00Z","message":{"model":"claude-opus-5-5","content":[{"type":"text","text":"`+encoded+`"}]}}`,
		`{"type":"ai-title","aiTitle":"Hash suffix on PIN templates"}`,
	)
	writeLines(t, filepath.Join(root, "-repo", "bbbbbbbb-0000-0000-0000-000000000002.jsonl"),
		`{"type":"user","cwd":"`+cwd+`","timestamp":"2026-10-02T08:00:00Z","origin":{"kind":"human"},"message":{"content":"which session wrote this? `+encoded+`"}}`,
		`{"type":"assistant","effort":"high","timestamp":"2026-10-02T08:00:05Z","message":{"model":"claude-fable-5-1","content":[{"type":"text","text":"Looking."}]}}`,
	)
	paste := `  Trap for that deploy: once the fix is live, delete those eighteen template copies, or customers will
  see 482913##. I have left this note as a comment on the pull request so whoever ships it removes them first.

✔ Goal achieved (4h · 2 turns · 73.5k tokens) (ctrl+o to expand)`

	res, err := Find(paste, Options{Root: root, Home: home, Full: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Sessions) != 2 {
		t.Fatalf("sessions = %d, want 2: %+v", len(res.Sessions), res.Sessions)
	}
	top := res.Sessions[0]
	if top.ID != "aaaaaaaa-0000-0000-0000-000000000001" || top.Authored == 0 || top.Title != "Hash suffix on PIN templates" {
		t.Fatalf("top = %+v", top)
	}
	if want := "cd ~/repo && ccoo -- --resume aaaaaaaa-0000-0000-0000-000000000001"; top.Reopen != want {
		t.Fatalf("reopen = %q, want %q", top.Reopen, want)
	}
	if res.Sessions[1].Own() {
		t.Fatalf("the pasting session counts as an author: %+v", res.Sessions[1])
	}
}

// An id copied out of a sentence keeps its trailing period; it still
// resolves, and an excluded session (the caller's own) never does.
func TestFindByIDForgivesPunctuation(t *testing.T) {
	root := t.TempDir()
	writeLines(t, filepath.Join(root, "-repo", "e3f88062-ca30-4f57-a535-fd8d3a73c723.jsonl"),
		`{"type":"assistant","cwd":"/gone/worktree","timestamp":"2026-10-01T09:00:00Z","effort":"high","message":{"model":"claude-opus-5-5","content":[]}}`,
	)
	res, err := Find("e3f88062-ca30-4f57-a535-fd8d3a73c723.", Options{Root: root})
	if err != nil || len(res.Sessions) != 1 {
		t.Fatalf("Find = %+v, %v", res, err)
	}
	if !res.Sessions[0].CwdMissing || res.Diagnostics[len(res.Diagnostics)-1].Code != DiagCwdMissing {
		t.Fatalf("missing launch directory not reported: %+v", res.Diagnostics)
	}
	res, err = Find("e3f88062", Options{Root: root, Exclude: "e3f88062-ca30-4f57-a535-fd8d3a73c723"})
	if err != nil || len(res.Sessions) != 0 {
		t.Fatalf("excluded session found: %+v, %v", res.Sessions, err)
	}
}

func TestPreset(t *testing.T) {
	for _, tc := range []struct {
		model, effort string
		ultracode     bool
		want          string
	}{
		{"claude-opus-5-5", "xhigh", true, "ccoo"},
		{"claude-opus-5-5", "high", false, "cco"},
		{"claude-opus-5-5", "low", false, "ccol"},
		{"claude-opus-5-5", "xhigh", false, "cc --model opus --effort xhigh"},
		{"claude-fable-5-1", "high", false, "cch"},
		{"claude-fable-5-1", "low", false, "ccl"},
		{"claude-fable-5-1", "medium", false, "cc --model fable --effort medium"},
		{"claude-sonnet-5-5", "high", false, "cc"},
	} {
		if got := Preset(tc.model, tc.effort, tc.ultracode); got != tc.want {
			t.Errorf("Preset(%s, %s, %v) = %q, want %q", tc.model, tc.effort, tc.ultracode, got, tc.want)
		}
	}
}
