package blip

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Config is what a proxy is: its name, mode and the two addresses.
type Config struct {
	Name     string `json:"name"`
	Mode     string `json:"mode"` // http|tcp
	Listen   string `json:"listen"`
	Upstream string `json:"upstream"` // http://host:port or host:port (tcp)
}

// ParseUpstream validates --to and derives the mode.
func ParseUpstream(to string) (mode, upstream string, err error) {
	u, perr := url.Parse(to)
	if perr != nil || u.Host == "" {
		return "", "", diag(DiagUpstreamInvalid, "--to must be http://host:port or tcp://host:port, got "+fmt.Sprintf("%q", to), "blip up <name> --listen :9091 --to http://localhost:3000")
	}
	switch u.Scheme {
	case "http", "https":
		return "http", u.Scheme + "://" + u.Host, nil
	case "tcp":
		if u.Port() == "" {
			return "", "", diag(DiagUpstreamInvalid, "tcp upstream needs a port: "+to, "blip up <name> --listen :5433 --to tcp://localhost:5432")
		}
		return "tcp", u.Host, nil
	}
	return "", "", diag(DiagUpstreamInvalid, "unsupported upstream scheme "+fmt.Sprintf("%q", u.Scheme)+" (http, tcp)", "blip up <name> --listen :9091 --to http://localhost:3000")
}

// Counters are the live numbers `status` reports.
type Counters struct {
	Requests    int64 `json:"requests"`
	Faulted     int64 `json:"faulted"`
	InFlight    int64 `json:"in_flight"`
	Connections int64 `json:"connections"`
	Recorded    int64 `json:"recorded"`
}

// Status is the control-socket snapshot every verb reads.
type Status struct {
	Config
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Fault     *Fault    `json:"fault"`
	Recording bool      `json:"recording"`
	// CredentialHeaders names (never values) the credential headers the
	// recorded flow carried; the values are stripped at record time.
	CredentialHeaders []string `json:"credential_headers"`
	CredentialQuery   []string `json:"credential_query"`
	Counters          Counters `json:"counters"`
}

// Server is one proxy: listener, atomic fault, counters, the set of open
// connections (for cut), the log writer and the optional recorder. It is the
// in-process core the daemon wraps and the tests drive directly.
type Server struct {
	cfg       Config
	startedAt time.Time
	pid       int

	fault  atomic.Pointer[faultState]
	holdMu sync.Mutex
	hold   chan struct{} // closed on every fault change: releases `timeout`

	requests, faulted, inFlight, connections atomic.Int64

	connMu sync.Mutex
	conns  map[net.Conn]struct{}

	ln      net.Listener
	httpSrv *http.Server
	proxy   *reverseProxy
	logMu   sync.Mutex
	logW    io.Writer
	rec     *recorder

	// OnChange runs after every fault/recording change (the daemon persists
	// the state file from it).
	OnChange func()
}

// NewServer prepares a proxy; Start binds it. logW receives one line per
// request/connection; recPath is where `record on` appends.
func NewServer(cfg Config, logW io.Writer, recPath string) (*Server, error) {
	s := &Server{cfg: cfg, logW: logW, conns: map[net.Conn]struct{}{}, hold: make(chan struct{}), rec: newRecorder(recPath)}
	if cfg.Mode == "http" {
		u, err := url.Parse(cfg.Upstream)
		if err != nil {
			return nil, err
		}
		s.proxy = newReverseProxy(u)
	}
	return s, nil
}

// Start binds the listener and serves in the background.
func (s *Server) Start() error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return diag(DiagListenInvalid, "cannot listen on "+s.cfg.Listen+": "+err.Error(), "blip up "+s.cfg.Name+" --listen :<free port> --to "+s.cfg.Upstream)
	}
	s.ln = ln
	s.startedAt = time.Now()
	if s.cfg.Mode == "http" {
		s.httpSrv = &http.Server{Handler: s, ConnState: s.trackHTTPConn}
		go func() { _ = s.httpSrv.Serve(ln) }()
	} else {
		go s.serveTCP(ln)
	}
	return nil
}

// Addr is the bound address (useful when --listen was :0).
func (s *Server) Addr() net.Addr { return s.ln.Addr() }

// Close stops listening and closes every connection.
func (s *Server) Close() error {
	if s.httpSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.httpSrv.Shutdown(ctx)
	}
	s.Cut()
	if s.ln != nil {
		return s.ln.Close()
	}
	return nil
}

// SetFault REPLACES the active fault and releases `timeout` holds.
func (s *Server) SetFault(f *Fault) {
	var fs *faultState
	if f != nil {
		cp := *f
		if cp.SetAt.IsZero() {
			cp.SetAt = time.Now()
		}
		if cp.Rate == 0 {
			cp.Rate = 1
		}
		fs = &faultState{Fault: cp}
	}
	s.holdMu.Lock()
	s.fault.Store(fs)
	s.rotateHoldLocked()
	s.holdMu.Unlock()
	s.changed()
}

// Ok clears the fault.
func (s *Server) Ok() { s.SetFault(nil) }

// Cut closes every established connection, faults untouched.
func (s *Server) Cut() int {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	n := len(s.conns)
	for c := range s.conns {
		_ = c.Close()
		delete(s.conns, c)
	}
	return n
}

// SetRecording toggles the request recorder (HTTP mode).
func (s *Server) SetRecording(on bool) {
	s.rec.on.Store(on)
	s.changed()
}

// ClearRecording deletes the recording file.
func (s *Server) ClearRecording() error { return s.rec.clear() }

// Status snapshots the proxy.
func (s *Server) Status() Status {
	credHeaders, credQuery := s.rec.credentialNames()
	st := Status{Config: s.cfg, PID: s.pid, StartedAt: s.startedAt, Recording: s.rec.on.Load(), CredentialHeaders: credHeaders, CredentialQuery: credQuery}
	if fs := s.current(time.Now()); fs != nil {
		f := fs.Fault
		st.Fault = &f
	}
	s.connMu.Lock()
	st.Counters = Counters{Requests: s.requests.Load(), Faulted: s.faulted.Load(), InFlight: s.inFlight.Load(), Connections: int64(len(s.conns)), Recorded: s.rec.count()}
	s.connMu.Unlock()
	return st
}

// current returns the active fault, clearing it when --for has elapsed.
func (s *Server) current(now time.Time) *faultState {
	fs, _ := s.snapshot(now)
	return fs
}

// snapshot returns the active fault AND the hold channel that belongs to
// it, read under the same lock every fault change writes under — so a
// `timeout` decided against fault F always waits on F's channel, never on
// the one a concurrent `ok`/`set` just rotated in. Expired faults are
// cleared here.
func (s *Server) snapshot(now time.Time) (*faultState, <-chan struct{}) {
	s.holdMu.Lock()
	fs := s.fault.Load()
	if fs != nil && fs.expired(now) {
		s.fault.Store(nil)
		s.rotateHoldLocked()
		s.holdMu.Unlock()
		s.logf("fault %s expired (--for %s)", fs.Kind, fs.For)
		s.changed()
		return nil, s.holdCh()
	}
	ch := s.hold
	s.holdMu.Unlock()
	return fs, ch
}

func (s *Server) changed() {
	if s.OnChange != nil {
		s.OnChange()
	}
}

// holdCh is the channel a `timeout`-held request waits on.
func (s *Server) holdCh() <-chan struct{} {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	return s.hold
}

// rotateHoldLocked releases every held request; caller holds holdMu.
func (s *Server) rotateHoldLocked() {
	close(s.hold)
	s.hold = make(chan struct{})
}

func (s *Server) track(c net.Conn) {
	s.connMu.Lock()
	s.conns[c] = struct{}{}
	s.connMu.Unlock()
}

func (s *Server) untrack(c net.Conn) {
	s.connMu.Lock()
	delete(s.conns, c)
	s.connMu.Unlock()
}

func (s *Server) trackHTTPConn(c net.Conn, st http.ConnState) {
	switch st {
	case http.StateNew:
		s.connections.Add(1)
		s.track(c)
	case http.StateClosed:
		s.untrack(c)
	case http.StateHijacked:
		s.untrack(c) // the handler closes it
	}
}

// logf appends one timestamped line to the proxy log.
func (s *Server) logf(format string, a ...any) {
	if s.logW == nil {
		return
	}
	s.logMu.Lock()
	defer s.logMu.Unlock()
	fmt.Fprintf(s.logW, "%s %s\n", time.Now().Format("15:04:05.000"), strings.TrimSpace(fmt.Sprintf(format, a...)))
}
