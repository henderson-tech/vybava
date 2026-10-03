package watch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// The socket API is plain HTTP/1.1 over the unix socket, so a Claude Code mod
// reaches it with $.http.fetch({socketPath}) and a shell with
// `curl --unix-socket`. Every answer is JSON; an error is {"error": "..."}
// with a 4xx/5xx status.
//
//	GET    /v1/health
//	POST   /v1/subscriptions            AddRequest → AddResult (201)
//	GET    /v1/subscriptions[?session=] → Listing
//	DELETE /v1/subscriptions/{id}       → {"removed": "<id>"} (404 when unknown)
//	GET    /v1/events?session=&after=&timeout=  → {"events": [...]}
//
// /v1/events acknowledges (deletes) the session's events up to `after` and
// long-polls up to `timeout` (capped at 60 s) for newer ones.

// ErrDaemonDown is a client's answer when no daemon listens on the socket.
var ErrDaemonDown = errors.New("vybava watch is not running")

// ErrUnsettled is a probe's answer while its source is mid-computation
// (GitHub's mergeability right after a push to the base): the engine keeps
// the last reading and asks again next interval.
var ErrUnsettled = errors.New("reading not settled yet")

// Listen opens the socket: the state dir 0700, the socket 0600. The daemon
// lock is taken first and held until the listener closes, so only one daemon
// ever judges the socket; a socket file nobody answers on is then a stale one
// and is replaced, a live one refuses.
func Listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lock, err := lockDaemon(path + ".lock")
	if errors.Is(err, errDaemonLocked) {
		return nil, fmt.Errorf("a watch daemon already serves %s", path)
	}
	if err != nil {
		return nil, fmt.Errorf("take the daemon lock: %w", err)
	}
	ln, err := listenUnlocked(path)
	if err != nil {
		lock.Close()
		return nil, err
	}
	return &lockedListener{Listener: ln, lock: lock}, nil
}

func listenUnlocked(path string) (net.Listener, error) {
	if _, err := os.Stat(path); err == nil {
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			conn.Close()
			return nil, fmt.Errorf("a watch daemon already serves %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale socket: %w", err)
		}
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// lockedListener releases the daemon lock when the listener closes.
type lockedListener struct {
	net.Listener
	lock *os.File
	once sync.Once
}

func (l *lockedListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { l.lock.Close() })
	return err
}

// Serve runs the API on ln and ticks the engine every tick until ctx ends,
// then stops accepting, lets in-flight probes finish and returns.
func Serve(ctx context.Context, e *Engine, ln net.Listener, tick time.Duration, log io.Writer) error {
	srv := &http.Server{Handler: Handler(e), ReadHeaderTimeout: 10 * time.Second}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	fmt.Fprintf(log, "watch: serving %s (pid %d, kinds %v)\n", ln.Addr(), os.Getpid(), e.Kinds())

	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	e.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := srv.Shutdown(shutdown)
			e.Wait()
			fmt.Fprintln(log, "watch: stopped")
			return err
		case err := <-served:
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		case <-ticker.C:
			e.Tick(ctx)
		}
	}
}

// Handler is the socket API.
func Handler(e *Engine) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, _ *http.Request) {
		reply(w, http.StatusOK, e.Health(os.Getpid()))
	})
	mux.HandleFunc("POST /v1/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		var req AddRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&req); err != nil {
			replyErr(w, http.StatusBadRequest, fmt.Errorf("body: %w", err))
			return
		}
		res, err := e.Add(r.Context(), req)
		if err != nil {
			replyErr(w, http.StatusUnprocessableEntity, err)
			return
		}
		reply(w, http.StatusCreated, res)
	})
	mux.HandleFunc("GET /v1/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, e.List(r.URL.Query().Get("session")))
	})
	mux.HandleFunc("DELETE /v1/subscriptions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		ok, err := e.Remove(id)
		switch {
		case err != nil:
			replyErr(w, http.StatusInternalServerError, err)
		case !ok:
			replyErr(w, http.StatusNotFound, fmt.Errorf("no subscription %s", id))
		default:
			reply(w, http.StatusOK, map[string]string{"removed": id})
		}
	})
	mux.HandleFunc("GET /v1/events", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		after, err := strconv.ParseInt(q.Get("after"), 10, 64)
		if q.Get("after") == "" {
			after, err = 0, nil
		}
		if err != nil {
			replyErr(w, http.StatusBadRequest, fmt.Errorf("after: %w", err))
			return
		}
		timeout := time.Duration(0)
		if v := q.Get("timeout"); v != "" {
			if timeout, err = time.ParseDuration(v); err != nil {
				replyErr(w, http.StatusBadRequest, fmt.Errorf("timeout: %w", err))
				return
			}
		}
		events, err := e.Events(r.Context(), q.Get("session"), after, timeout)
		if err != nil {
			replyErr(w, http.StatusBadRequest, err)
			return
		}
		reply(w, http.StatusOK, map[string]any{"events": events})
	})
	return mux
}

func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func replyErr(w http.ResponseWriter, status int, err error) {
	reply(w, status, map[string]string{"error": err.Error()})
}

// Client talks to the daemon over its socket.
type Client struct {
	socket string
	http   *http.Client
}

// NewClient dials socket per request; a long-poll may run up to MaxWait.
func NewClient(socket string) *Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
	}
	return &Client{socket: socket, http: &http.Client{Transport: transport, Timeout: MaxWait + 15*time.Second}}
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://watchd"+path, rd)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) {
			return fmt.Errorf("%w (no answer on %s)", ErrDaemonDown, c.socket)
		}
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return &APIError{Status: resp.StatusCode, Message: e.Error}
		}
		return &APIError{Status: resp.StatusCode, Message: string(bytes.TrimSpace(data))}
	}
	return json.Unmarshal(data, out)
}

// APIError is the daemon refusing a request.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return e.Message }

// Health asks the daemon for its self-report.
func (c *Client) Health(ctx context.Context) (Health, error) {
	var h Health
	return h, c.do(ctx, http.MethodGet, "/v1/health", nil, &h)
}

// Add subscribes.
func (c *Client) Add(ctx context.Context, req AddRequest) (AddResult, error) {
	var res AddResult
	return res, c.do(ctx, http.MethodPost, "/v1/subscriptions", req, &res)
}

// List answers one session's subscriptions ("" = all).
func (c *Client) List(ctx context.Context, session string) (Listing, error) {
	var l Listing
	path := "/v1/subscriptions"
	if session != "" {
		path += "?session=" + url.QueryEscape(session)
	}
	return l, c.do(ctx, http.MethodGet, path, nil, &l)
}

// Remove drops a subscription.
func (c *Client) Remove(ctx context.Context, id string) error {
	var out map[string]string
	return c.do(ctx, http.MethodDelete, "/v1/subscriptions/"+url.PathEscape(id), nil, &out)
}

// Events acknowledges up to after and long-polls for newer events.
func (c *Client) Events(ctx context.Context, session string, after int64, timeout time.Duration) ([]Event, error) {
	q := url.Values{"session": {session}, "after": {strconv.FormatInt(after, 10)}, "timeout": {timeout.String()}}
	var out struct {
		Events []Event `json:"events"`
	}
	return out.Events, c.do(ctx, http.MethodGet, "/v1/events?"+q.Encode(), nil, &out)
}
