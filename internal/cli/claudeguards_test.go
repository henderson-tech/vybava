package cli

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeOnyxDaemon answers the daemon's two routes — the GET /browser lookup says
// this session does have a browser, POST /mcp reports the stop — records what
// the POST carried, and points the command at itself through the same env the
// installer documents.
func fakeOnyxDaemon(t *testing.T, calls *int, posted *string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/browser" {
			return
		}
		*calls++
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		*posted = string(body)
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"stopped":true}}`))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ONYX_MCP_HTTP_PORT", u.Port())
	t.Setenv("ONYX_MCP_HTTP_TOKEN_FILE", tok)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-env")
}

func runBrowserTeardown(t *testing.T, stdin io.Reader, args ...string) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd, err := App{Stdin: stdin, Stdout: &out, Stderr: &errOut}.Command("claude-guards")
	if err != nil {
		t.Fatal(err)
	}
	cmd.SetArgs(append([]string{"browser-teardown"}, args...))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("browser-teardown must never fail the session: %v", err)
	}
}

// browser-teardown runs at every session end, so a payload it cannot parse must
// not be fatal: the environment still names the session whose browser has to
// stop, and the command exits 0 either way.
func TestClaudeGuardsBrowserTeardownSurvivesMalformedStdin(t *testing.T) {
	calls, posted := 0, ""
	fakeOnyxDaemon(t, &calls, &posted)

	runBrowserTeardown(t, strings.NewReader("{ this is not hook JSON"))

	if calls != 1 {
		t.Fatalf("calls = %d, want one browser_stop from the environment session", calls)
	}
	if !strings.Contains(posted, `"session":"sess-env"`) {
		t.Fatalf("body = %s, want the environment session", posted)
	}
}

// --session is the hand-run form, typed at a terminal that never sends EOF, so
// the flag must win before a single byte of stdin is read — otherwise the one
// documented manual invocation hangs on a read nothing ever ends.
func TestClaudeGuardsBrowserTeardownWithSessionNeverReadsStdin(t *testing.T) {
	calls, posted := 0, ""
	fakeOnyxDaemon(t, &calls, &posted)

	runBrowserTeardown(t, neverEnds{t}, "--session", "sess-flag")

	if !strings.Contains(posted, `"session":"sess-flag"`) {
		t.Fatalf("body = %s, want the flag's session", posted)
	}
}

// neverEnds is a terminal: a read of it would never return, so the test says so
// instead of hanging.
type neverEnds struct{ t *testing.T }

func (r neverEnds) Read([]byte) (int, error) {
	r.t.Error("browser-teardown read stdin although --session named the session")
	return 0, io.EOF
}

// redact-session runs at every session end: it must scrub the payload's
// session in place, audit it, and never fail the exit.
func TestClaudeGuardsRedactSessionScrubsThePayloadSession(t *testing.T) {
	claude, config := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("XDG_CONFIG_HOME", config)
	t.Setenv("CODEX_HOME", t.TempDir())
	// Assembled at run time: no secret shape in the source.
	secret := strings.Repeat("q7Zx", 8)
	transcript := filepath.Join(claude, "projects", "-Users-someone-app", "sess-end.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(transcript, []byte(`{"type":"user","content":"MAIL_PASSWORD=`+secret+`"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	cmd, err := App{Stdin: strings.NewReader(`{"session_id":"sess-end"}`), Stdout: &out, Stderr: &errOut}.Command("claude-guards")
	if err != nil {
		t.Fatal(err)
	}
	cmd.SetArgs([]string{"redact-session"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("redact-session must never fail the session: %v", err)
	}
	data, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) || !strings.Contains(string(data), "[REDACTED:env-assignment]") {
		t.Fatalf("transcript not redacted: %s", data)
	}
	audit, err := os.ReadFile(filepath.Join(config, "vybava", "redact-audit.jsonl"))
	if err != nil || !strings.Contains(string(audit), "sess-end.jsonl") || strings.Contains(string(audit), secret) {
		t.Fatalf("audit = %q, %v", audit, err)
	}
	if !strings.Contains(errOut.String(), "1 secret spans redacted") {
		t.Errorf("stderr = %q", errOut.String())
	}
	// A file it cannot read is said, never a silent partial scrub.
	loop := filepath.Join(filepath.Dir(transcript), "sess-end", "loop.jsonl")
	if err := os.MkdirAll(filepath.Dir(loop), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	errOut.Reset()
	cmd.SetArgs([]string{"redact-session", "--session", "sess-end"})
	if err := cmd.Execute(); err != nil || !strings.Contains(errOut.String(), "1 unreadable") {
		t.Errorf("unreadable not reported: %v, %q", err, errOut.String())
	}
}
