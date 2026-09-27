package blip

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// recordBodyLimit caps the stored request body.
const recordBodyLimit = 64 * 1024

// Record is one proxied HTTP request as stored in <name>.rec.jsonl. Headers
// are kept verbatim (that is what `authz` replays); response bodies are
// never stored — status and byte length only.
type Record struct {
	At            time.Time   `json:"at"`
	Method        string      `json:"method"`
	Path          string      `json:"path"`
	Query         string      `json:"query,omitempty"`
	Headers       http.Header `json:"headers"`
	Body          []byte      `json:"body,omitempty"`
	BodyTruncated bool        `json:"body_truncated,omitempty"`
	Status        int         `json:"status"`
	ResponseBytes int64       `json:"response_bytes"`
}

// recorder appends Records to a 0600 JSONL file while on.
type recorder struct {
	path string
	on   atomic.Bool
	mu   sync.Mutex
	n    atomic.Int64
}

func newRecorder(path string) *recorder {
	r := &recorder{path: path}
	if path != "" {
		if recs, err := ReadRecords(path); err == nil {
			r.n.Store(int64(len(recs)))
		}
	}
	return r
}

func (r *recorder) count() int64 { return r.n.Load() }

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
	rec := Record{At: time.Now(), Method: req.Method, Path: req.URL.Path, Query: req.URL.RawQuery, Headers: req.Header.Clone(),
		Body: body, BodyTruncated: truncated, Status: status, ResponseBytes: respBytes}
	line, err := json.Marshal(rec)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
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
