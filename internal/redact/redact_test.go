package redact

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fixture secrets are assembled at run time so neither the commit guard nor
// a host's push protection ever sees one in the source.
func fake(prefix string, n int) string {
	const alphabet = "a7Kq2Xm9Pz4Rt8Vb3Nc6Hw1Ly5Jd0Fs"
	var b strings.Builder
	b.WriteString(prefix)
	for i := 0; b.Len() < len(prefix)+n; i++ {
		b.WriteByte(alphabet[i%len(alphabet)])
	}
	return b.String()
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func jsonLine(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b) + "\n"
}

type fixture struct {
	roots    Roots
	session  string
	secrets  []string
	subagent string
}

// newFixture plants one session the way Claude Code lays it out: the main
// transcript, a subagent (reached a second time through the tmp task link),
// a workflow journal, a persisted tool result and a background-task output.
func newFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	f := fixture{
		roots: Roots{
			ClaudeProjects: filepath.Join(dir, "projects"),
			ClaudeTmp:      filepath.Join(dir, "tmp"),
			ClaudeHistory:  filepath.Join(dir, "history.jsonl"),
			Codex:          filepath.Join(dir, "codex"),
		},
		session: "2bbb9875-5525-4303-b0ad-8142b1e260e7",
	}
	mail, twilio, gh, pem := fake("", 44), fake("", 32), fake("gh"+"p_", 36), fake("MIIE", 80)
	f.secrets = []string{mail, twilio, gh, pem, mail[:7]}
	slug := filepath.Join(f.roots.ClaudeProjects, "-Users-someone-app")
	sessionDir := filepath.Join(slug, f.session)
	f.subagent = filepath.Join(sessionDir, "subagents", "agent-a954.jsonl")

	write(t, filepath.Join(slug, f.session+".jsonl"),
		jsonLine(t, map[string]any{"type": "user", "message": map[string]any{"content": "check the env ✓"}})+
			jsonLine(t, map[string]any{"type": "user", "toolUseResult": map[string]any{
				"stdout": "MAIL_PASSWORD=" + mail + "\nTWILIO_TOKEN=\"" + twilio + "\"\n",
			}}))
	write(t, f.subagent,
		jsonLine(t, map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "MAIL_PASSWORD=<set len 44 prefix " + mail[:7] + "…> — é   done"},
		}}})+
			jsonLine(t, map[string]any{"type": "user", "content": "key:\n-----BEGIN RSA PRIVATE KEY-----\n" + pem + "\n-----END RSA PRIVATE KEY-----\n"}))
	write(t, filepath.Join(sessionDir, "subagents", "workflows", "wf_1", "journal.jsonl"),
		jsonLine(t, map[string]any{"result": "names only: MAIL_PASSWORD TWILIO_TOKEN"}))
	write(t, filepath.Join(sessionDir, "tool-results", "b1.txt"), "remote: token "+gh+"\n")
	tasks := filepath.Join(f.roots.ClaudeTmp, "-Users-someone-app", f.session, "tasks")
	write(t, filepath.Join(tasks, "bq.output"), "export TWILIO_TOKEN='"+twilio+"'\n")
	if err := os.Symlink(f.subagent, filepath.Join(tasks, "a954.output")); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f fixture) files(t *testing.T) []string {
	t.Helper()
	files, unreadable, err := f.roots.Files(Target{Sessions: []string{f.session}})
	if err != nil || unreadable != 0 {
		t.Fatalf("Files: %v (unreadable %d)", err, unreadable)
	}
	return files
}

func TestApplyRedactsEverySessionFileAndASecondRunFindsNothing(t *testing.T) {
	f := newFixture(t)
	files := f.files(t)
	if len(files) != 6 {
		t.Fatalf("files = %d, want 6 (session, subagent, journal, tool result, task output, task link)", len(files))
	}
	before, err := os.Stat(f.subagent)
	if err != nil {
		t.Fatal(err)
	}
	var audit bytes.Buffer
	rep := Run(files, Options{Apply: true, Audit: &audit})
	if rep.Files != 5 {
		t.Errorf("scanned %d files, want 5: the task link is the subagent, scanned once", rep.Files)
	}
	if rep.Spans != 6 || rep.Redacted != 6 || rep.Errors != 0 {
		t.Fatalf("spans %d redacted %d errors %d, want 6/6/0: %+v", rep.Spans, rep.Redacted, rep.Errors, rep.Counts)
	}
	for _, path := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range f.secrets {
			if bytes.Contains(data, []byte(s)) {
				t.Errorf("%s still holds a planted secret", filepath.Base(path))
			}
		}
		if strings.HasSuffix(path, ".jsonl") {
			sc := bufio.NewScanner(bytes.NewReader(data))
			for sc.Scan() {
				if !json.Valid(sc.Bytes()) {
					t.Errorf("%s: invalid JSONL after apply: %s", filepath.Base(path), sc.Text())
				}
			}
		}
		if bytes.Contains(audit.Bytes(), []byte(f.secrets[0][:7])) {
			t.Error("the audit log carries a value")
		}
	}
	after, err := os.Stat(f.subagent)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("subagent size/mtime moved: %d→%d, %v→%v", before.Size(), after.Size(), before.ModTime(), after.ModTime())
	}
	if again := Run(files, Options{}); again.Spans != 0 {
		t.Errorf("second run found %d spans: %+v", again.Spans, again.Counts)
	}
	if lines := strings.Count(audit.String(), "\n"); lines != 4 {
		t.Errorf("audit lines = %d, want one per touched file (4)", lines)
	}
}

func TestScanReportsWithoutWritingOrPrintingValues(t *testing.T) {
	f := newFixture(t)
	original, err := os.ReadFile(f.subagent)
	if err != nil {
		t.Fatal(err)
	}
	rep := Run(f.files(t), Options{})
	if rep.Spans != 6 || rep.Redacted != 0 {
		t.Fatalf("scan: spans %d redacted %d", rep.Spans, rep.Redacted)
	}
	now, err := os.ReadFile(f.subagent)
	if err != nil || !bytes.Equal(now, original) {
		t.Fatal("scan mode changed a file")
	}
	out, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range f.secrets {
		if bytes.Contains(out, []byte(s)) {
			t.Errorf("the report carries a planted value (%d bytes)", len(s))
		}
	}
	var fragment bool
	for _, file := range rep.Leaky {
		for _, g := range file.Findings {
			fragment = fragment || g.Detector == "fragment" && g.Line == 1 && strings.Contains(g.Shape, "prefix ‹fragment›")
		}
	}
	if !fragment {
		t.Errorf("the incident fragment is not reported at line 1: %+v", rep.Leaky)
	}
}

func TestApplyLeavesBytesThatChangedUnderTheScan(t *testing.T) {
	f := newFixture(t)
	path := filepath.Join(f.roots.ClaudeTmp, "-Users-someone-app", f.session, "tasks", "bq.output")
	res := scanFile(path, detect{})
	if res.Spans != 1 {
		t.Fatalf("spans = %d", res.Spans)
	}
	// The writer replaced the file meanwhile: same length, other bytes.
	write(t, path, strings.Repeat("x", int(res.patches[0].off)+len(res.patches[0].orig)+2))
	applyPatches(&res.File, res.patches)
	if res.Changed != 1 || res.Redacted != 0 {
		t.Errorf("changed %d redacted %d, want 1/0", res.Changed, res.Redacted)
	}
	if data, _ := os.ReadFile(path); bytes.Contains(data, []byte("REDACTED")) {
		t.Error("wrote over bytes the scan never saw")
	}
}

func TestLiveAppendIsKeptAndAPartialRecordIsLeftToItsWriter(t *testing.T) {
	f := newFixture(t)
	res := scanFile(f.subagent, detect{})
	// The session appends a complete record and starts another after the scan.
	tail := jsonLine(t, map[string]any{"type": "user", "content": "later"}) + `{"type":"assist`
	fh, err := os.OpenFile(f.subagent, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fh.WriteString(tail); err != nil {
		t.Fatal(err)
	}
	if err := fh.Close(); err != nil {
		t.Fatal(err)
	}
	applyPatches(&res.File, res.patches)
	if res.Redacted != 2 || res.Error != "" {
		t.Fatalf("redacted %d error %q", res.Redacted, res.Error)
	}
	data, err := os.ReadFile(f.subagent)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(data, []byte(tail)) {
		t.Error("the appended bytes did not survive the apply")
	}
	if again := scanFile(f.subagent, detect{}); again.Spans != 0 || again.Error != "" {
		t.Errorf("rescan: spans %d error %q", again.Spans, again.Error)
	}
}

func TestTargetsResolveProjectsCodexAndRefuseAnUnknownSession(t *testing.T) {
	f := newFixture(t)
	rollout := filepath.Join(f.roots.Codex, "sessions", "2026", "09", "26", "rollout-2026-09-26T10-00-00-019c.jsonl")
	write(t, rollout, jsonLine(t, map[string]any{"type": "session_meta", "payload": map[string]any{"id": "019c", "cwd": "/Users/someone/app/sub"}}))
	write(t, filepath.Join(f.roots.ClaudeProjects, "-Users-someone-app--worktrees-x", "s2.jsonl"), "{}\n")
	write(t, filepath.Join(f.roots.ClaudeProjects, "-Users-someone-apple", "s3.jsonl"), "{}\n")

	files, _, err := f.roots.Files(Target{Projects: []string{"/Users/someone/app"}})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(files, "\n")
	for _, want := range []string{f.session + ".jsonl", "--worktrees-x/s2.jsonl", "rollout-2026", "bq.output"} {
		if !strings.Contains(joined, want) {
			t.Errorf("project files miss %s", want)
		}
	}
	if strings.Contains(joined, "apple") {
		t.Error("a sibling project sharing the prefix was taken")
	}
	if _, _, err := f.roots.Files(Target{Sessions: []string{"0000000000"}}); err == nil {
		t.Error("an unknown session read as clean")
	}
	files, _, err = f.roots.Files(Target{Sessions: []string{f.session}, Since: time.Now().Add(time.Hour)})
	if err != nil || len(files) != 0 {
		t.Errorf("--since in the future: %d files, %v", len(files), err)
	}
}
