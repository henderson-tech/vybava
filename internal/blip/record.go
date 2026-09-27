package blip

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sort"
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

// Record is one proxied HTTP request as stored in <name>.rec.jsonl (full
// URL: path AND query — the request under test). Credential headers are
// stripped; only their names survive in CredentialHeaders. Response bodies
// are never stored — status and byte length only.
type Record struct {
	At                time.Time   `json:"at"`
	Method            string      `json:"method"`
	Path              string      `json:"path"`
	Query             string      `json:"query,omitempty"`
	Headers           http.Header `json:"headers"`
	CredentialHeaders []string    `json:"credential_headers,omitempty"`
	Body              []byte      `json:"body,omitempty"`
	BodyTruncated     bool        `json:"body_truncated,omitempty"`
	Status            int         `json:"status"`
	ResponseBytes     int64       `json:"response_bytes"`
}

// recorder appends Records to a 0600 JSONL file while on.
type recorder struct {
	path  string
	on    atomic.Bool
	mu    sync.Mutex
	n     atomic.Int64
	creds map[string]bool // credential header NAMES seen (never values)
}

func newRecorder(path string) *recorder {
	r := &recorder{path: path, creds: map[string]bool{}}
	if path != "" {
		if recs, err := ReadRecords(path); err == nil {
			r.n.Store(int64(len(recs)))
			for _, rec := range recs {
				for _, name := range rec.CredentialHeaders {
					r.creds[name] = true
				}
			}
		}
	}
	return r
}

func (r *recorder) count() int64 { return r.n.Load() }

func (r *recorder) credentialNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.creds))
	for n := range r.creds {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
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

func (r *recorder) append(req *http.Request, body []byte, truncated bool, status int, respBytes int64) {
	if r.path == "" {
		return
	}
	headers := req.Header.Clone()
	creds := stripCredentials(headers)
	rec := Record{At: time.Now(), Method: req.Method, Path: req.URL.Path, Query: req.URL.RawQuery, Headers: headers, CredentialHeaders: creds,
		Body: body, BodyTruncated: truncated, Status: status, ResponseBytes: respBytes}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, name := range creds {
		r.creds[name] = true
	}
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
	r.creds = map[string]bool{}
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
