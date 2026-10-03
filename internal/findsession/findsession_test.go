package findsession

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	// a file name that is no session id is never listed: it would reach the
	// reopen line unquoted
	writeLines(t, filepath.Join(root, "-repo", "e3f88062;touch pwned.jsonl"), `{"type":"assistant"}`)
	res, err = Find("e3f88062", Options{Root: root, Exclude: "e3f88062-ca30-4f57-a535-fd8d3a73c723"})
	if err != nil || len(res.Sessions) != 0 {
		t.Fatalf("excluded session found: %+v, %v", res.Sessions, err)
	}
	// no cwd recorded: no `cd` into an empty path, a warning instead
	writeLines(t, filepath.Join(root, "-repo", "0d0d0d0d-0000-0000-0000-000000000000.jsonl"), `{"type":"assistant","message":{"model":"claude-opus-5-5"},"effort":"high"}`)
	res, err = Find("0d0d0d0d", Options{Root: root})
	if err != nil || len(res.Sessions) != 1 || strings.HasPrefix(res.Sessions[0].Reopen, "cd ") || res.Diagnostics[0].Code != DiagCwdUnknown {
		t.Fatalf("unknown cwd: %+v %+v, %v", res.Sessions, res.Diagnostics, err)
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
		{"claude-opus-5-5", "high; rm -rf ~", false, "cc --model opus"},
	} {
		if got := Preset(tc.model, tc.effort, tc.ultracode); got != tc.want {
			t.Errorf("Preset(%s, %s, %v) = %q, want %q", tc.model, tc.effort, tc.ultracode, got, tc.want)
		}
	}
}

// The session that wrote the text is found however many newer sessions
// quote it, and a session that only passed the text to a tool (find-session
// itself, a file write) is no author.
func TestFindAuthorBehindQuotersAndToolCalls(t *testing.T) {
	root := t.TempDir()
	text := "the quiet harbour lamps were lit early tonight while the ferry waited for the last passengers"
	writeLines(t, filepath.Join(root, "-repo", "aaaaaaaa-0000-0000-0000-000000000000.jsonl"),
		`{"type":"assistant","cwd":"/repo","timestamp":"2026-09-01T08:00:00Z","effort":"high","message":{"model":"claude-opus-5-5","content":[{"type":"text","text":"`+text+`"}]}}`,
	)
	for i := range 10 {
		writeLines(t, filepath.Join(root, "-repo", fmt.Sprintf("bbbbbbbb-0000-0000-0000-%012d.jsonl", i)),
			`{"type":"user","cwd":"/repo","timestamp":"2026-09-02T08:00:00Z","origin":{"kind":"human"},"message":{"content":"`+text+`"}}`,
			`{"type":"assistant","timestamp":"2026-09-02T08:00:01Z","message":{"model":"claude-opus-5-5","content":[{"type":"tool_use","id":"t","name":"Bash","input":{"command":"find-session <<'Q'\n`+text+`\nQ"}}]}}`,
		)
		touch(t, filepath.Join(root, "-repo", fmt.Sprintf("bbbbbbbb-0000-0000-0000-%012d.jsonl", i)), time.Duration(i+1)*time.Minute)
	}
	res, err := Find(text, Options{Root: root, Full: true})
	if err != nil || len(res.Sessions) == 0 {
		t.Fatalf("Find = %+v, %v", res, err)
	}
	if top := res.Sessions[0]; top.ID != "aaaaaaaa-0000-0000-0000-000000000000" || !top.Own() {
		t.Fatalf("top = %s own=%v, want the author", top.ID, top.Own())
	}
	for _, s := range res.Sessions[1:] {
		if s.Own() {
			t.Fatalf("%s counted as author from a tool call", s.ID)
		}
	}
}

// A needle straddling a read-chunk boundary still matches.
func TestScanAcrossChunks(t *testing.T) {
	defer func(size int) { chunkSize = size }(chunkSize)
	chunkSize = 64
	path := filepath.Join(t.TempDir(), "s.jsonl")
	writeLines(t, path, strings.Repeat("x", 50)+"lantern harbour ferry"+strings.Repeat("y", 200))
	found, err := scanFile(path, []needle{newNeedle("lantern harbour ferry"), newNeedle("absent words here")}, 1, make([]byte, chunkSize+64))
	if err != nil || found != 1 {
		t.Fatalf("found = %d, %v", found, err)
	}
}

func touch(t *testing.T, path string, ahead time.Duration) {
	t.Helper()
	at := time.Now().Add(ahead)
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

// --limit is display only: two sessions that tie still report AMBIGUOUS when
// one is shown, and a partial scan's fix reruns the same short query.
func TestFindVerdictIgnoresLimit(t *testing.T) {
	root := t.TempDir()
	for _, id := range []string{"cafecafe-0000-0000-0000-000000000001", "cafecafe-0000-0000-0000-000000000002"} {
		writeLines(t, filepath.Join(root, "-repo", id+".jsonl"),
			`{"type":"assistant","cwd":"/repo","timestamp":"2026-09-01T08:00:00Z","effort":"high","message":{"model":"claude-opus-5-5","content":[{"type":"text","text":"lanterns over the harbour"}]}}`,
		)
	}
	res, err := Find("cafecafe", Options{Root: root, Limit: 1})
	if err != nil || len(res.Sessions) != 1 || !hasDiag(res, DiagAmbiguous) {
		t.Fatalf("Find = %+v %+v, %v", res.Sessions, res.Diagnostics, err)
	}
	writeLines(t, filepath.Join(root, "-old", "01d01d00-0000-0000-0000-000000000000.jsonl"), `{"type":"assistant"}`)
	touch(t, filepath.Join(root, "-old", "01d01d00-0000-0000-0000-000000000000.jsonl"), -30*24*time.Hour)
	res, err = Find("lanterns over the harbour", Options{Root: root})
	if err != nil || !hasDiag(res, DiagPartial) {
		t.Fatalf("no partial scan: %+v, %v", res.Diagnostics, err)
	}
	for _, d := range res.Diagnostics {
		if d.Code == DiagPartial && d.Fix != "find-session --full 'lanterns over the harbour'" {
			t.Fatalf("partial fix = %q", d.Fix)
		}
	}
}

// A credential in a session's prompt or title never reaches the output.
func TestFindRedactsSecrets(t *testing.T) {
	root := t.TempDir()
	token := "ghp_" + strings.Repeat("aB3dE5", 6)
	writeLines(t, filepath.Join(root, "-repo", "5ec5ec5e-0000-0000-0000-000000000000.jsonl"),
		`{"type":"user","cwd":"/repo","timestamp":"2026-09-01T08:00:00Z","origin":{"kind":"human"},"message":{"content":"use `+token+` for the push"}}`,
		`{"type":"ai-title","aiTitle":"Push with `+token+`"}`,
	)
	res, err := Find("5ec5ec5e", Options{Root: root})
	if err != nil || len(res.Sessions) != 1 {
		t.Fatalf("Find = %+v, %v", res, err)
	}
	if s := res.Sessions[0]; strings.Contains(s.Prompt+s.Title, token) {
		t.Fatalf("token leaked: prompt=%q title=%q", s.Prompt, s.Title)
	}
}

// A paste with no plain-word phrase still yields at most maxNeedles needles.
func TestFragmentsCapWords(t *testing.T) {
	var ids []string
	for i := range 500 {
		ids = append(ids, fmt.Sprintf("snake_case_%d", i))
	}
	frags, need := Fragments(strings.Join(ids, " "))
	if len(frags) != maxNeedles || need != maxNeedles {
		t.Fatalf("needles = %d, need = %d", len(frags), need)
	}
}

func hasDiag(res Result, code string) bool {
	for _, d := range res.Diagnostics {
		if d.Code == code {
			return true
		}
	}
	return false
}
