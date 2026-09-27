package blip

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Identity is the substituted caller for an authz replay. It only ever
// comes from --as; the value is never printed.
type Identity struct {
	Kind   string // none|header|cookie
	Header string // header name (header kind)
	Cookie string // cookie name (cookie kind)
	value  string
}

// ParseIdentity parses --as:
//
//	none                       strip Authorization and Cookie
//	header:<Name>=<value>      replace that header
//	cookie:<name>=<value>      replace that cookie
//	env:<VAR>                  Authorization taken from that environment variable
func ParseIdentity(spec string) (Identity, error) {
	usage := "blip <name> authz --as none | 'header:Authorization=Bearer <token>' | 'cookie:<name>=<value>' | env:<VAR>"
	kind, rest, _ := strings.Cut(spec, ":")
	switch kind {
	case "none":
		if rest != "" {
			return Identity{}, diag(DiagIdentityInvalid, "--as none takes no value", usage)
		}
		return Identity{Kind: "none"}, nil
	case "header", "cookie":
		name, value, ok := strings.Cut(rest, "=")
		if !ok || name == "" {
			return Identity{}, diag(DiagIdentityInvalid, "--as "+kind+": needs <name>=<value>", usage)
		}
		if kind == "header" {
			return Identity{Kind: "header", Header: http.CanonicalHeaderKey(name), value: value}, nil
		}
		return Identity{Kind: "cookie", Cookie: name, value: value}, nil
	case "env":
		if rest == "" {
			return Identity{}, diag(DiagIdentityInvalid, "--as env: needs a variable name", usage)
		}
		v, ok := os.LookupEnv(rest)
		if !ok || v == "" {
			return Identity{}, diag(DiagIdentityInvalid, "environment variable "+rest+" is not set (inject it via onyx run_command)", usage)
		}
		return Identity{Kind: "header", Header: "Authorization", value: v}, nil
	}
	// Never echo the given value: it may be a credential.
	return Identity{}, diag(DiagIdentityInvalid, "--as must be one of: none, header:<Name>=<value>, cookie:<name>=<value>, env:<VAR>", usage)
}

// String names the identity without its value.
func (id Identity) String() string {
	switch id.Kind {
	case "header":
		return "header " + id.Header
	case "cookie":
		return "cookie " + id.Cookie
	}
	return "none (unauthenticated)"
}

func (id Identity) apply(h http.Header) {
	switch id.Kind {
	case "none":
		h.Del("Authorization")
		h.Del("Cookie")
	case "header":
		h.Set(id.Header, id.value)
	case "cookie":
		h.Set("Cookie", id.Cookie+"="+id.value)
	}
}

// AuthzOptions are the authz flags.
type AuthzOptions struct {
	Identity  Identity
	Expect    []int // statuses that mean "correctly refused"
	Only      string
	Exclude   string
	Mutations bool
}

// ParseExpect parses "401,403,404".
func ParseExpect(s string) ([]int, error) {
	var out []int
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil || n < 100 || n > 599 {
			return nil, fmt.Errorf("not an HTTP status list: %q", s)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--expect needs at least one status")
	}
	return out, nil
}

// Candidate is a replay that was NOT refused.
type Candidate struct {
	Method        string `json:"method"`
	Path          string `json:"path"`
	Original      int    `json:"original_status"`
	Replayed      int    `json:"replayed_status"`
	OriginalBytes int64  `json:"original_bytes"`
	ReplayedBytes int64  `json:"replayed_bytes"`
	// Verdict: "same payload" (2xx, equal length to the original — the
	// strongest signal), "different payload" (2xx, other length) or
	// "not refused" (any other unexpected status).
	Verdict string `json:"verdict"`
}

// Skipped is a recorded request the replay did not send.
type Skipped struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// AuthzReport is `authz`'s data payload.
type AuthzReport struct {
	Upstream   string      `json:"upstream"`
	Identity   string      `json:"identity"`
	Checked    int         `json:"checked"`
	Candidates []Candidate `json:"candidates"`
	Skipped    []Skipped   `json:"skipped"`
}

var mutating = map[string]bool{"POST": true, "PUT": true, "PATCH": true, "DELETE": true}

// hopByHop headers are never replayed.
var hopByHop = []string{"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailers", "Transfer-Encoding", "Upgrade", "Content-Length", "Accept-Encoding"}

// Replay sends every recorded request straight to upstream with the
// identity substituted and classifies the answers. Response bodies are read
// for their length only and discarded.
func Replay(upstream string, recs []Record, opts AuthzOptions) (AuthzReport, error) {
	rep := AuthzReport{Upstream: upstream, Identity: opts.Identity.String(), Candidates: []Candidate{}, Skipped: []Skipped{}}
	expect := map[int]bool{}
	for _, s := range opts.Expect {
		expect[s] = true
	}
	client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	seen := map[string]bool{}
	for _, rec := range recs {
		// Records are stored credential-free; strip again so a hand-edited
		// or older recording can never replay a query credential.
		rec.Query, _ = stripCredentialQuery(rec.Query)
		key := rec.Method + " " + rec.Path + "?" + rec.Query
		if seen[key] {
			continue
		}
		seen[key] = true
		if (opts.Only != "" && !MatchPath(opts.Only, rec.Path)) || (opts.Exclude != "" && MatchPath(opts.Exclude, rec.Path)) {
			continue
		}
		if mutating[rec.Method] && !opts.Mutations {
			rep.Skipped = append(rep.Skipped, Skipped{Method: rec.Method, Path: rec.Path, Reason: "mutation (pass --mutations to replay)"})
			continue
		}
		if rec.BodyTruncated {
			rep.Skipped = append(rep.Skipped, Skipped{Method: rec.Method, Path: rec.Path, Reason: "body truncated at record time"})
			continue
		}
		if len(rec.BodyRedacted) > 0 {
			rep.Skipped = append(rep.Skipped, Skipped{Method: rec.Method, Path: rec.Path, Reason: "body carried credentials (redacted)"})
			continue
		}
		// Replay the path as it was sent: rec.Path is decoded (for --only),
		// so /files/a%3Fb must not be rebuilt as /files/a?b.
		wirePath := rec.RawPath
		if wirePath == "" {
			wirePath = (&url.URL{Path: rec.Path}).EscapedPath()
		}
		target := strings.TrimSuffix(upstream, "/") + wirePath
		if rec.Query != "" {
			target += "?" + rec.Query
		}
		req, err := http.NewRequest(rec.Method, target, bytes.NewReader(rec.Body))
		if err != nil {
			return rep, err
		}
		req.Header = rec.Headers.Clone()
		for _, h := range hopByHop {
			req.Header.Del(h)
		}
		// Records are stored credential-free, but strip again here so a
		// replay never carries anything but the --as identity, whichever
		// header the identity swaps.
		stripCredentials(req.Header)
		opts.Identity.apply(req.Header)
		resp, err := client.Do(req)
		if err != nil {
			return rep, fmt.Errorf("replay %s %s: %w", rec.Method, rec.Path, sanitizeErr(err))
		}
		n, _ := io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		rep.Checked++
		if expect[resp.StatusCode] {
			continue
		}
		c := Candidate{Method: rec.Method, Path: rec.Path, Original: rec.Status, Replayed: resp.StatusCode, OriginalBytes: rec.ResponseBytes, ReplayedBytes: n, Verdict: "not refused"}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			c.Verdict = "different payload"
			if n == rec.ResponseBytes {
				c.Verdict = "same payload"
			}
		}
		rep.Candidates = append(rep.Candidates, c)
	}
	sort.SliceStable(rep.Candidates, func(i, j int) bool {
		return verdictRank(rep.Candidates[i].Verdict) < verdictRank(rep.Candidates[j].Verdict)
	})
	return rep, nil
}

func verdictRank(v string) int {
	switch v {
	case "same payload":
		return 0
	case "different payload":
		return 1
	}
	return 2
}

// sanitizeErr keeps transport errors from echoing a URL with credentials.
func sanitizeErr(err error) error {
	var ue interface{ Unwrap() error }
	if errors.As(err, &ue) && ue.Unwrap() != nil {
		return ue.Unwrap()
	}
	return err
}
