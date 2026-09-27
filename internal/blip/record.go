package blip

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"mime"
	"net/http"
	"net/url"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// recordBodyLimit caps the stored request body.
const recordBodyLimit = 64 * 1024

// credentialHeaders are stripped from every record and from every replay
// before the --as identity is applied: identity values are never stored,
// and a replay never carries the original caller's credentials.
var credentialHeaders = []string{"Authorization", "Proxy-Authorization", "Cookie", "X-Api-Key", "X-Auth-Token"}

// stripCredentials removes credentialHeaders from h and returns the names
// that were present.
func stripCredentials(h http.Header) []string {
	var names []string
	for _, name := range credentialHeaders {
		if _, ok := h[name]; ok {
			names = append(names, name)
			h.Del(name)
		}
	}
	return names
}

// credentialQueryKeys are query parameters that carry a credential (token
// reads, signed URLs). Matched case-insensitively; stripped from the stored
// query and never re-added by a replay — the --as identity is the only
// credential a replay carries.
var credentialQueryKeys = []string{"access_token", "x-amz-signature", "x-amz-security-token", "x-amz-credential", "x-goog-signature", "x-goog-credential", "sig", "signature", "token", "api_key", "apikey", "key", "auth", "jwt", "session"}

// stripCredentialQuery removes credentialQueryKeys from a raw query,
// preserving the order and encoding of what remains, and returns the
// (lower-cased) names that were present.
func stripCredentialQuery(raw string) (string, []string) {
	if raw == "" {
		return "", nil
	}
	var kept, names []string
	seen := map[string]bool{}
	for _, pair := range strings.Split(raw, "&") {
		key := pair
		if i := strings.IndexByte(pair, '='); i >= 0 {
			key = pair[:i]
		}
		if decoded, err := url.QueryUnescape(key); err == nil {
			key = decoded
		}
		key = strings.ToLower(key)
		if slices.Contains(credentialQueryKeys, key) {
			if !seen[key] {
				names = append(names, key)
				seen[key] = true
			}
			continue
		}
		kept = append(kept, pair)
	}
	return strings.Join(kept, "&"), names
}

// Record is one proxied HTTP request as stored in <name>.rec.jsonl (full
// URL: path AND query — the request under test). Credential headers are
// stripped; only their names survive in CredentialHeaders. Response bodies
// are never stored — status and byte length only.
type Record struct {
	At                time.Time   `json:"at"`
	Method            string      `json:"method"`
	Path              string      `json:"path"`     // decoded: what --match/--only compare against
	RawPath           string      `json:"raw_path"` // as sent on the wire: what a replay uses
	Query             string      `json:"query,omitempty"`
	Headers           http.Header `json:"headers"`
	CredentialHeaders []string    `json:"credential_headers,omitempty"`
	CredentialQuery   []string    `json:"credential_query,omitempty"`
	Body              []byte      `json:"body,omitempty"`
	BodyTruncated     bool        `json:"body_truncated,omitempty"`
	// BodyUnparsed: a JSON/form body that did not parse (or was truncated)
	// is stored WITHOUT bytes — an unparsed credential-typed body is never
	// kept — and, like a truncated one, never replayed.
	BodyUnparsed bool `json:"body_unparsed,omitempty"`
	// BodyRedacted names the credential keys whose values were replaced by
	// "[redacted]" in Body; such a record is never replayed.
	BodyRedacted  []string `json:"body_redacted,omitempty"`
	Status        int      `json:"status"`
	ResponseBytes int64    `json:"response_bytes"`
}

// recorder appends Records to a 0600 JSONL file while on.
type recorder struct {
	path   string
	on     atomic.Bool
	mu     sync.Mutex
	n      atomic.Int64
	creds  map[string]bool // credential header NAMES seen (never values)
	qcreds map[string]bool // credential query KEYS seen (never values)
}

func newRecorder(path string) *recorder {
	r := &recorder{path: path, creds: map[string]bool{}, qcreds: map[string]bool{}}
	if path != "" {
		if recs, err := ReadRecords(path); err == nil {
			r.n.Store(int64(len(recs)))
			for _, rec := range recs {
				r.noteCredentials(rec.CredentialHeaders, rec.CredentialQuery)
			}
		}
	}
	return r
}

func (r *recorder) count() int64 { return r.n.Load() }

func (r *recorder) noteCredentials(headers, query []string) {
	for _, name := range headers {
		r.creds[name] = true
	}
	for _, name := range query {
		r.qcreds[name] = true
	}
}

// credentialNames returns the header names and query keys seen, sorted.
func (r *recorder) credentialNames() (headers, query []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	headers = slices.Sorted(maps.Keys(r.creds))
	query = slices.Sorted(maps.Keys(r.qcreds))
	return headers, query
}

// capture reads up to recordBodyLimit of the body and puts it back.
func (r *recorder) capture(req *http.Request) ([]byte, bool) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, false
	}
	head, err := io.ReadAll(io.LimitReader(req.Body, recordBodyLimit+1))
	if err != nil {
		return nil, false
	}
	truncated := len(head) > recordBodyLimit
	rewindBody(req, head, req.Body)
	if truncated {
		head = head[:recordBodyLimit]
	}
	return head, truncated
}

// credentialBodyKeys are redacted from JSON (one level deep) and
// form-urlencoded request bodies at record time; other content types are
// stored as-is.
var credentialBodyKeys = []string{"password", "passwd", "secret", "token", "access_token", "refresh_token", "id_token", "client_secret", "api_key", "apikey", "otp", "code", "pin", "session", "jwt", "authorization"}

const redactedValue = "[redacted]"

// redactBody replaces credential values in a JSON object or a form body and
// returns the redacted body plus the key names it touched (nil when none).
func redactBody(contentType string, body []byte) (out []byte, redacted []string, unparsed bool) {
	if len(body) == 0 {
		return body, nil, false
	}
	mediaType, _, _ := mime.ParseMediaType(contentType)
	switch {
	case mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"):
		dec := json.NewDecoder(bytes.NewReader(body))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil {
			return nil, nil, true
		}
		v = redactJSON(v, "", &redacted)
		if redacted == nil {
			return body, nil, false
		}
		sort.Strings(redacted)
		out, err := json.Marshal(v)
		if err != nil {
			return nil, nil, true
		}
		return out, redacted, false
	case mediaType == "application/x-www-form-urlencoded":
		var kept []string
		for _, pair := range strings.Split(string(body), "&") {
			key, _, hasValue := strings.Cut(pair, "=")
			decoded, err := url.QueryUnescape(key)
			if err != nil {
				return nil, nil, true
			}
			if hasValue && slices.Contains(credentialBodyKeys, strings.ToLower(decoded)) {
				kept = append(kept, key+"="+url.QueryEscape(redactedValue))
				redacted = append(redacted, decoded)
				continue
			}
			kept = append(kept, pair)
		}
		if redacted == nil {
			return body, nil, false
		}
		return []byte(strings.Join(kept, "&")), redacted, false
	}
	return body, nil, false
}

// redactJSON walks objects and arrays at any depth, replacing the value of
// every credential key and recording its dotted path (`credentials.password`,
// `users[1].token`).
func redactJSON(v any, path string, names *[]string) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			p := k
			if path != "" {
				p = path + "." + k
			}
			if slices.Contains(credentialBodyKeys, strings.ToLower(k)) {
				t[k] = redactedValue
				*names = append(*names, p)
				continue
			}
			t[k] = redactJSON(val, p, names)
		}
		return t
	case []any:
		for i := range t {
			t[i] = redactJSON(t[i], path+"["+strconv.Itoa(i)+"]", names)
		}
		return t
	}
	return v
}

func (r *recorder) append(req *http.Request, body []byte, truncated bool, status int, respBytes int64) {
	if r.path == "" {
		return
	}
	headers := req.Header.Clone()
	creds := stripCredentials(headers)
	query, qcreds := stripCredentialQuery(req.URL.RawQuery)
	// A truncated body can no longer be parsed for credentials, so it is not
	// stored at all; replay skips truncated records anyway.
	var redacted []string
	unparsed := false
	if truncated {
		body = nil
	} else {
		body, redacted, unparsed = redactBody(req.Header.Get("Content-Type"), body)
	}
	rec := Record{At: time.Now(), Method: req.Method, Path: req.URL.Path, RawPath: req.URL.EscapedPath(), Query: query, Headers: headers, CredentialHeaders: creds, CredentialQuery: qcreds,
		Body: body, BodyTruncated: truncated, BodyUnparsed: unparsed, BodyRedacted: redacted, Status: status, ResponseBytes: respBytes}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.noteCredentials(creds, qcreds)
	f, err := os.OpenFile(r.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err == nil {
		r.n.Add(1)
	}
}

func (r *recorder) clear() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n.Store(0)
	r.creds, r.qcreds = map[string]bool{}, map[string]bool{}
	if err := os.Remove(r.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// ReadRecords loads a recording; a missing file is an empty recording.
func ReadRecords(path string) ([]Record, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var recs []Record
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1024*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, err
		}
		recs = append(recs, rec)
	}
	return recs, sc.Err()
}
