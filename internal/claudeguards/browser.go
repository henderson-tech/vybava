package claudeguards

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// guardBrowser — the Onyx browser is THE browser (incident-born, 2026-09-11).
//
// The playwright and chrome-devtools MCP wrappers attach to the browser that
// onyx-mcp owns for THIS session, looked up by CLAUDE_CODE_SESSION_ID. When no
// such browser exists they used to launch a standalone one: no Onyx extension,
// no vault autofill, a profile nobody reaps, and — the part with teeth — a
// silent redirect: a dead onyx browser did not fail the Playwright call, it
// moved the whole flow into a different, sometimes days-old browser with no
// signal. Two of those standalone Heliums segfaulted the same afternoon.
//
// So the first third-party browser call in a session must find this session's
// onyx browser already running. When the lookup says 404, block and name the
// one call that fixes it: mcp__onyx__browser_start(session: <id>).
//
// Fail-open on everything that is not a clear "no browser" answer: no session
// id in the environment, no token file, server unreachable, 401. The guard
// enforces "attach to onyx" on a Mac where onyx is installed; it must never
// brick a session where it is not.
// ---------------------------------------------------------------------------

const (
	browserLookupTimeout = 1500 * time.Millisecond
	defaultOnyxHTTPPort  = "3212"
)

// onyxSessionID is the identity every onyx browser tool is keyed by.
func onyxSessionID() string {
	for _, k := range []string{"CLAUDE_CODE_SESSION_ID", "ONYX_MCP_SESSION"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// onyxTokenFile mirrors deploy/install-mcp-service.sh's default; the token value
// is read into the request header only and never printed.
func onyxTokenFile() string {
	if v := strings.TrimSpace(os.Getenv("ONYX_MCP_HTTP_TOKEN_FILE")); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "Onyx", "mcp-http-token")
}

// onyxHTTPBase is the loopback origin of the resident onyx-mcp daemon. The port
// is the one knob the installer moves, so tests point at an httptest server the
// same way an operator points at a second port.
//
// Only a plain port number survives. The value lands in the URL's authority, so
// one carrying an "@" ("evil.example.com@1") would move a bearer-token POST off
// loopback to a host the environment names — and the answer is printed to
// stderr. Anything that is not a port is not a knob, it is a splice.
func onyxHTTPBase() string {
	port := defaultOnyxHTTPPort
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("ONYX_MCP_HTTP_PORT"))); err == nil && n > 0 && n <= 65535 {
		port = strconv.Itoa(n)
	}
	return "http://127.0.0.1:" + port
}

func onyxBrowserLookupURL(session string) string {
	return onyxHTTPBase() + "/browser?session=" + session
}

// onyxBearerToken reads the daemon's loopback bearer token. ok=false means
// "onyx is not installed or not reachable here" — every caller fails open on
// it. The value goes into an Authorization header and nowhere else: never argv,
// never a URL, never a log line.
func onyxBearerToken() (string, bool) {
	path := onyxTokenFile()
	if path == "" {
		return "", false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	token := strings.TrimSpace(string(raw))
	return token, token != ""
}

// browserState asks onyx-mcp whether this session's browser is running.
// Returns (running, known): known is false whenever the answer is not a clear
// 200 or 404 — the caller fails open on !known.
func browserState(session string) (running, known bool) {
	token, ok := onyxBearerToken()
	if !ok {
		return false, false
	}
	req, err := http.NewRequest(http.MethodGet, onyxBrowserLookupURL(session), nil)
	if err != nil {
		return false, false
	}
	req.Header.Set("Authorization", "Bearer "+token)
	client := &http.Client{Timeout: browserLookupTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, true
	case http.StatusNotFound:
		return false, true
	default:
		return false, false
	}
}

// Browser is the PreToolUse decision for the third-party browser MCP tools
// (settings.json matcher: mcp__playwright__.*|mcp__plugin_chrome-devtools-mcp_chrome-devtools__.*).
func Browser(in *HookInput) *Denial {
	if d := screenshotDir(in); d != nil { // local, no network: cheapest first
		return d
	}
	session := onyxSessionID()
	if session == "" {
		return nil
	}
	running, known := browserState(session)
	if !known || running {
		return nil
	}
	return deny("browser:onyx-first",
		fmt.Sprintf(`No Onyx browser is running for this session (%s), and %s would launch a
standalone browser instead: no Onyx extension, no vault autofill, an unreaped
profile — and if the onyx browser later dies, calls silently move to another
browser with no signal. The Onyx browser is THE browser.`, session, in.ToolName),
		fmt.Sprintf(`Start it first, then retry:
  mcp__onyx__browser_start(session: %q)              # add headless: false when a human must act
The playwright / chrome-devtools wrappers attach to it by session lookup. If this
session's playwright or chrome-devtools MCP already launched its own browser
earlier, run /mcp to reconnect so the wrapper attaches to the onyx one.`, session))
}
