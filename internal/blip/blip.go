// Package blip is a chaos proxy for adverse-condition testing: one named
// proxy per `blip up <name> --listen <addr> --to <upstream>`, whose fault is
// toggled live from the CLI while the app under test keeps pointing at it.
//
// Two modes, chosen by the upstream scheme:
//
//   - http://host:port — an HTTP-aware reverse proxy. Faults may be scoped by
//     request path glob and method, and `error` answers with a status of its
//     own without forwarding.
//   - tcp://host:port — a raw byte proxy (Postgres, Redis, anything). Faults
//     act on connections and byte streams; `error`, `--match` and `--method`
//     do not exist here and are refused with the corrected invocation.
//
// One fault at a time: `set` REPLACES the active fault, `ok` clears it. The
// fault lives behind an atomic pointer so a swap is safe under concurrent
// requests, and `timeout`-held requests are released (503) by the swap.
//
// Process model: `up` re-executes the binary detached with the hidden `serve`
// verb; the daemon writes <state>/<name>.json, appends <state>/<name>.log,
// and answers control calls (JSON over HTTP) on <state>/<name>.sock. Every
// other verb talks to that socket. Recording (`record on`) additionally
// appends each proxied HTTP request to <state>/<name>.rec.jsonl for the
// `authz` access-control replay, which hits the upstream directly.
package blip

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
)

// The CLOSED diagnostic-code enum for the blip applet. Adding a code means a
// doc comment here stating when it fires and what the fix is.
const (
	// DiagNotRunning: no proxy of that name is up — the fix is `blip up`.
	DiagNotRunning = "NOT_RUNNING"
	// DiagStale: a state file exists but its daemon is dead or its socket
	// does not answer — the fix is `blip down <name>`, which cleans it.
	DiagStale = "STALE_STATE"
	// DiagArgsDiffer: `up` on a running name with a different listen/upstream
	// — the fix is `blip down <name>` first, or the running invocation.
	DiagArgsDiffer = "ARGS_DIFFER"
	// DiagAlreadyUp: `up` on a running name with the same args — info only.
	DiagAlreadyUp = "ALREADY_UP"
	// DiagUpstreamInvalid: --to is not http://host:port or tcp://host:port.
	DiagUpstreamInvalid = "UPSTREAM_INVALID"
	// DiagListenInvalid: --listen is not a bindable host:port.
	DiagListenInvalid = "LISTEN_INVALID"
	// DiagFaultInvalid: unknown fault kind or a malformed argument/flag.
	DiagFaultInvalid = "FAULT_INVALID"
	// DiagHTTPOnly: `error`, --match or --method on a tcp proxy.
	DiagHTTPOnly = "HTTP_ONLY"
	// DiagNameInvalid: the proxy name is not [a-z0-9-].
	DiagNameInvalid = "NAME_INVALID"
	// DiagStartFailed: the daemon did not answer within the start window;
	// detail carries the last log lines.
	DiagStartFailed = "START_FAILED"
	// DiagNothingRecorded: authz asked before any request was recorded.
	DiagNothingRecorded = "NOTHING_RECORDED"
	// DiagIdentityInvalid: --as is not one of none|header:|cookie:|env:.
	DiagIdentityInvalid = "IDENTITY_INVALID"
	// DiagAuthzLeakCandidates: at least one replay answered outside --expect.
	DiagAuthzLeakCandidates = "AUTHZ_LEAK_CANDIDATES"
	// DiagFaultExpired: --for elapsed; the daemon cleared the fault itself.
	DiagFaultExpired = "FAULT_EXPIRED"
)

// Result is what every verb hands the CLI: the payload, one short human line
// per fact, and the diagnostics and next commands the envelope carries.
type Result struct {
	Data        any
	Lines       []string
	Diagnostics []runx.Diagnostic
	Next        []string
}

func diag(code, detail, fix string) runx.DiagError {
	return runx.DiagError{Diag: runx.Diagnostic{Code: code, Severity: "error", Detail: detail, Fix: fix}}
}

func info(code, detail, fix string) runx.Diagnostic {
	return runx.Diagnostic{Code: code, Severity: "info", Detail: detail, Fix: fix}
}

// StateDir is ~/.local/state/blip, or $BLIP_STATE_DIR (tests, sandboxes).
func StateDir() (string, error) {
	if dir := os.Getenv("BLIP_STATE_DIR"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "blip"), nil
}

// Paths groups the per-proxy files under the state dir.
type Paths struct{ State, Sock, Log, Rec string }

func pathsFor(dir, name string) Paths {
	base := filepath.Join(dir, name)
	return Paths{State: base + ".json", Sock: base + ".sock", Log: base + ".log", Rec: base + ".rec.jsonl"}
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

func validName(name string) error {
	if !nameRE.MatchString(name) {
		return diag(DiagNameInvalid, "proxy name must be lowercase [a-z0-9-]: "+strconv.Quote(name), "blip up <name> --listen <addr> --to <upstream>")
	}
	return nil
}

// ParseDuration accepts Go durations ("800ms", "1.5s", "2m").
func ParseDuration(s string) (time.Duration, error) {
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("not a duration: %q (want e.g. 800ms, 2s)", s)
	}
	return d, nil
}

// ParseRate parses a bandwidth like "20kbps", "1.5mbps", "800bps" into
// bytes per second (network convention: the units are bits).
func ParseRate(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	mult := 0.0
	var num string
	switch {
	case strings.HasSuffix(s, "kbps"):
		mult, num = 1000, strings.TrimSuffix(s, "kbps")
	case strings.HasSuffix(s, "mbps"):
		mult, num = 1000*1000, strings.TrimSuffix(s, "mbps")
	case strings.HasSuffix(s, "bps"):
		mult, num = 1, strings.TrimSuffix(s, "bps")
	default:
		return 0, fmt.Errorf("not a rate: %q (want e.g. 20kbps, 1mbps, 800bps)", s)
	}
	f, err := strconv.ParseFloat(num, 64)
	if err != nil || f <= 0 {
		return 0, fmt.Errorf("not a rate: %q (want e.g. 20kbps, 1mbps, 800bps)", s)
	}
	bytesPerSec := int64(f * mult / 8)
	if bytesPerSec < 1 {
		bytesPerSec = 1
	}
	return bytesPerSec, nil
}

// ParseProbability parses --rate for faults: 0 < p <= 1.
func ParseProbability(s string) (float64, error) {
	p, err := strconv.ParseFloat(s, 64)
	if err != nil || p <= 0 || p > 1 {
		return 0, fmt.Errorf("not a probability: %q (want 0 < rate <= 1)", s)
	}
	return p, nil
}

// ParseFlap parses "<down>/<up>".
func ParseFlap(s string) (down, up time.Duration, err error) {
	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("not a flap window: %q (want <down>/<up>, e.g. 5s/10s)", s)
	}
	if down, err = ParseDuration(parts[0]); err != nil {
		return 0, 0, err
	}
	if up, err = ParseDuration(parts[1]); err != nil {
		return 0, 0, err
	}
	if down == 0 || up == 0 {
		return 0, 0, fmt.Errorf("flap windows must be > 0: %q", s)
	}
	return down, up, nil
}

// globRE turns a path glob into a regexp: `*` matches anything INCLUDING
// `/`, `?` one character; everything else is literal.
func globRE(glob string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for _, r := range glob {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

// MatchPath reports whether path matches glob (see globRE).
func MatchPath(glob, path string) bool {
	if glob == "" {
		return true
	}
	re, err := globRE(glob)
	if err != nil {
		return false
	}
	return re.MatchString(path)
}
