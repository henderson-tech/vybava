package blip

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// State is the <name>.json file the daemon writes; other verbs read it to
// find the socket and to report a dead daemon as STALE_STATE.
type State struct {
	Config
	PID       int       `json:"pid"`
	Sock      string    `json:"sock"`
	Log       string    `json:"log"`
	Rec       string    `json:"rec"`
	StartedAt time.Time `json:"started_at"`
	Fault     *Fault    `json:"fault"`
	Recording bool      `json:"recording"`
}

func readState(path string) (*State, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func writeState(path string, st State) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Serve is the hidden daemon verb: run the proxy and its control socket
// until `down` or SIGTERM. It never returns an envelope; its stdout/stderr
// are the log file.
func Serve(cfg Config, paths Paths) error {
	if err := os.MkdirAll(filepath.Dir(paths.State), 0o700); err != nil {
		return err
	}
	logF, err := os.OpenFile(paths.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer logF.Close()
	srv, err := NewServer(cfg, logF, paths.Rec)
	if err != nil {
		return err
	}
	srv.pid = os.Getpid()
	persist := func() {
		st := srv.Status()
		_ = writeState(paths.State, State{Config: cfg, PID: os.Getpid(), Sock: paths.Sock, Log: paths.Log, Rec: paths.Rec, StartedAt: st.StartedAt, Fault: st.Fault, Recording: st.Recording})
	}
	srv.OnChange = persist
	if err := srv.Start(); err != nil {
		return err
	}
	ctl, err := ServeControl(srv, paths.Sock)
	if err != nil {
		_ = srv.Close()
		return err
	}
	persist()
	srv.logf("blip %s up: %s %s -> %s (pid %d)", cfg.Name, cfg.Mode, srv.Addr(), cfg.Upstream, os.Getpid())

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, syscall.SIGINT)
	select {
	case <-stop:
	case <-ctl.done:
	}
	srv.logf("blip %s down", cfg.Name)
	_ = ctl.ln.Close()
	_ = srv.Close()
	_ = os.Remove(paths.Sock)
	_ = os.Remove(paths.State)
	return nil
}

// controlServer answers the verbs over a unix socket: tiny JSON over HTTP.
type controlServer struct {
	ln   net.Listener
	done chan struct{}
}

// ServeControl starts the control socket for s.
func ServeControl(s *Server, sock string) (*controlServer, error) {
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(sock, 0o600)
	c := &controlServer{ln: ln, done: make(chan struct{})}
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, st Status) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	}
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) { reply(w, s.Status()) })
	mux.HandleFunc("POST /set", func(w http.ResponseWriter, r *http.Request) {
		var f Fault
		if err := json.NewDecoder(r.Body).Decode(&f); err != nil {
			http.Error(w, "bad fault: "+err.Error(), http.StatusBadRequest)
			return
		}
		s.SetFault(&f)
		reply(w, s.Status())
	})
	mux.HandleFunc("POST /ok", func(w http.ResponseWriter, r *http.Request) { s.Ok(); reply(w, s.Status()) })
	mux.HandleFunc("POST /cut", func(w http.ResponseWriter, r *http.Request) {
		n := s.Cut()
		w.Header().Set("X-Blip-Cut", fmt.Sprint(n))
		reply(w, s.Status())
	})
	mux.HandleFunc("POST /record", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			On    *bool `json:"on"`
			Clear bool  `json:"clear"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if body.Clear {
			if err := s.ClearRecording(); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}
		if body.On != nil {
			s.SetRecording(*body.On)
		}
		reply(w, s.Status())
	})
	mux.HandleFunc("POST /down", func(w http.ResponseWriter, r *http.Request) {
		reply(w, s.Status())
		select {
		case <-c.done:
		default:
			close(c.done)
		}
	})
	go func() { _ = (&http.Server{Handler: mux}).Serve(ln) }()
	return c, nil
}

// client talks to one proxy's control socket.
type client struct{ http *http.Client }

func dial(sock string) *client {
	return &client{http: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}}
}

func (c *client) call(method, path string, body any) (Status, http.Header, error) {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return Status{}, nil, err
		}
	}
	req, err := http.NewRequest(method, "http://blip"+path, &buf)
	if err != nil {
		return Status{}, nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return Status{}, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Status{}, nil, errors.New("control: " + string(bytes.TrimSpace(msg)))
	}
	var st Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return Status{}, nil, err
	}
	return st, resp.Header, nil
}

func (c *client) status() (Status, error) {
	st, _, err := c.call(http.MethodGet, "/status", nil)
	return st, err
}
