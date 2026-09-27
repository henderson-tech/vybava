package blip

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// reverseProxy wraps httputil.ReverseProxy with a 502 that names the upstream.
type reverseProxy struct{ *httputil.ReverseProxy }

func newReverseProxy(u *url.URL) *reverseProxy {
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.FlushInterval = 50 * time.Millisecond
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, "blip: upstream "+u.Host+" unreachable: "+err.Error()+"\n")
	}
	return &reverseProxy{rp}
}

// ServeHTTP is the HTTP-mode request path: decide the fault, apply it, log
// and (when recording) store the request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	s.requests.Add(1)
	s.inFlight.Add(1)
	defer s.inFlight.Add(-1)

	applied := ""
	fs := s.current(start)
	if fs != nil && fs.decide(r.Method, r.URL.Path, start) {
		applied = fs.Kind
		s.faulted.Add(1)
	}

	recording := s.rec.on.Load()
	var body []byte
	truncated := false
	if recording {
		body, truncated = s.rec.capture(r)
	}

	sw := &statusWriter{ResponseWriter: w}
	switch applied {
	case "drop", "flap":
		s.dropHTTP(w)
	case "error":
		body := fs.Body
		if body == "" {
			body = http.StatusText(fs.Status)
		}
		ct := "text/plain; charset=utf-8"
		if json.Valid([]byte(body)) {
			ct = "application/json"
		}
		sw.Header().Set("Content-Type", ct)
		sw.WriteHeader(fs.Status)
		_, _ = io.WriteString(sw, body)
	case "timeout":
		select {
		case <-s.holdCh():
			http.Error(sw, "blip: request held by timeout fault, released", http.StatusServiceUnavailable)
		case <-r.Context().Done():
		}
	case "delay":
		select {
		case <-time.After(fs.sleep()):
			s.proxy.ServeHTTP(sw, r)
		case <-r.Context().Done():
		}
	case "slow":
		sw.bytesPerSec = fs.Bytes
		s.proxy.ServeHTTP(sw, r)
	default:
		s.proxy.ServeHTTP(sw, r)
	}

	dur := time.Since(start)
	fault := applied
	if fault == "" {
		fault = "-"
	}
	s.logf("%s %s fault=%s status=%d dur=%s", r.Method, r.URL.RequestURI(), fault, sw.status, dur.Round(time.Millisecond))
	if recording {
		s.rec.append(r, body, truncated, sw.status, sw.bytes)
	}
}

// dropHTTP hijacks the connection and closes it without a response.
func (s *Server) dropHTTP(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetLinger(0) // RST rather than FIN: the client sees a reset
	}
	_ = conn.Close()
}

// statusWriter records the status and byte count and, for `slow`, throttles
// the body to bytesPerSec.
type statusWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	bytesPerSec int64
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.bytesPerSec <= 0 {
		n, err := w.ResponseWriter.Write(p)
		w.bytes += int64(n)
		return n, err
	}
	written := 0
	chunk := int(max(w.bytesPerSec/20, 1))
	for len(p) > 0 {
		n := min(chunk, len(p))
		m, err := w.ResponseWriter.Write(p[:n])
		written += m
		w.bytes += int64(m)
		if err != nil {
			return written, err
		}
		if f, ok := w.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		p = p[n:]
		time.Sleep(time.Duration(float64(m) / float64(w.bytesPerSec) * float64(time.Second)))
	}
	return written, nil
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hj.Hijack()
}

// rewindBody puts captured bytes back in front of the remaining body.
func rewindBody(r *http.Request, head []byte, rest io.ReadCloser) {
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), rest), rest}
}
