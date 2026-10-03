// Package cmux is the one client of the cmux terminal's control socket
// (JSON lines, v2): socket discovery, the reachability/version/access check,
// and the handful of methods fleet and watch need — resolve a session's
// surface by pid, read its screen, paste into it, press a key, focus it, and
// follow the event stream. Every request is one connection; nothing is
// cached across calls. Contract and gotchas: docs/cmux.md.
package cmux

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MinVersion is the oldest cmux app this client is tested against.
const MinVersion = "0.64.25"

// RequiredMethods are the socket methods fleet and watch call; a cmux that
// does not advertise one of them is reported outdated.
var RequiredMethods = []string{
	"agent.resolve_delivery_target",
	"surface.read_text",
	"surface.send_key",
	"surface.focus",
	"terminal.paste",
	"workspace.select",
	"window.focus",
}

// maxResponseBytes caps one response line; read_text of a long scrollback
// is the largest reply this client asks for.
const maxResponseBytes = 8 << 20

// SocketPath is where the cmux socket lives: $CMUX_SOCKET_PATH (set inside
// cmux terminals), else the path cmux last wrote to
// ~/.local/state/cmux/last-socket-path, else the default beside it.
// A process outside cmux (Fleet.app, the watch LaunchAgent) has no env var.
func SocketPath(getenv func(string) string, home string) string {
	if path := strings.TrimSpace(getenv("CMUX_SOCKET_PATH")); path != "" {
		return path
	}
	dir := filepath.Join(home, ".local", "state", "cmux")
	if raw, err := os.ReadFile(filepath.Join(dir, "last-socket-path")); err == nil {
		if path := strings.TrimSpace(string(raw)); path != "" {
			return path
		}
	}
	return filepath.Join(dir, "cmux.sock")
}

// Client talks to one cmux socket.
type Client struct {
	Socket string
	// Timeout bounds one request when ctx has no earlier deadline; 0 = 10 s.
	Timeout time.Duration
	// Sleep waits before a retry of a request cmux refused unrun; nil = time.Sleep.
	Sleep func(time.Duration)
}

// Error is a request cmux answered with ok:false.
type Error struct {
	Method  string
	Code    string
	Message string
	// Retryable is cmux's word that the command never ran (overloaded, or a
	// withdrawn main-actor hop) — only then may a caller send it again.
	Retryable    bool
	RetryAfterMS int
}

func (e *Error) Error() string {
	return fmt.Sprintf("cmux %s: %s: %s", e.Method, e.Code, e.Message)
}

// ErrNotFound reports whether err is cmux's not_found (no live target,
// unknown surface).
func ErrNotFound(err error) bool {
	var ce *Error
	return errors.As(err, &ce) && ce.Code == "not_found"
}

// UnreachableError is a socket that could not be dialled, or a connection
// cmux closed without a reply — the access mode refusing a process that
// does not descend from cmux looks exactly like that.
type UnreachableError struct {
	Socket string
	Err    error
	// Closed is true when the dial succeeded but no reply came.
	Closed bool
}

func (e *UnreachableError) Error() string {
	if e.Closed {
		return fmt.Sprintf("cmux closed %s without a reply: %v", e.Socket, e.Err)
	}
	return fmt.Sprintf("cmux socket %s unreachable: %v", e.Socket, e.Err)
}

func (e *UnreachableError) Unwrap() error { return e.Err }

type request struct {
	ID     string `json:"id"`
	Method string `json:"method"`
	Params any    `json:"params"`
}

type response struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Retryable    bool `json:"retryable"`
			RetryAfterMS int  `json:"retry_after_ms"`
		} `json:"data"`
	} `json:"error"`
}

// maxAttempts bounds how often a request cmux refused unrun is re-sent.
const maxAttempts = 3

// Call sends one request and decodes its result into out (nil discards it).
// It re-sends only what cmux says never ran (Error.Retryable), so a paste
// cmux accepted is never sent twice.
func (c Client) Call(ctx context.Context, method string, params, out any) error {
	if params == nil {
		params = struct{}{}
	}
	var err error
	for attempt := 1; ; attempt++ {
		var raw json.RawMessage
		raw, err = c.once(ctx, method, params)
		var ce *Error
		if err == nil {
			if out == nil || len(raw) == 0 {
				return nil
			}
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("cmux %s result: %w", method, err)
			}
			return nil
		}
		if !errors.As(err, &ce) || !ce.Retryable || attempt == maxAttempts || ctx.Err() != nil {
			return err
		}
		wait := time.Duration(ce.RetryAfterMS) * time.Millisecond
		if wait <= 0 {
			wait = 200 * time.Millisecond
		}
		c.sleep(wait)
	}
}

func (c Client) sleep(d time.Duration) {
	if c.Sleep != nil {
		c.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (c Client) deadline(ctx context.Context) time.Time {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	deadline := time.Now().Add(timeout)
	if limit, ok := ctx.Deadline(); ok && limit.Before(deadline) {
		deadline = limit
	}
	return deadline
}

func (c Client) dial(ctx context.Context) (net.Conn, error) {
	d := net.Dialer{Timeout: 3 * time.Second}
	conn, err := d.DialContext(ctx, "unix", c.Socket)
	if err != nil {
		return nil, &UnreachableError{Socket: c.Socket, Err: err}
	}
	return conn, nil
}

func (c Client) once(ctx context.Context, method string, params any) (json.RawMessage, error) {
	conn, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(c.deadline(ctx)); err != nil {
		return nil, err
	}
	if err := json.NewEncoder(conn).Encode(request{ID: "vybava-" + method, Method: method, Params: params}); err != nil {
		return nil, &UnreachableError{Socket: c.Socket, Err: err, Closed: true}
	}
	line, err := readLine(bufio.NewReaderSize(conn, 64<<10))
	if err != nil {
		return nil, &UnreachableError{Socket: c.Socket, Err: err, Closed: true}
	}
	var resp response
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, fmt.Errorf("cmux %s response: %w", method, err)
	}
	if !resp.OK {
		ce := &Error{Method: method, Code: "unknown", Message: "ok:false without an error"}
		if resp.Error != nil {
			ce.Code, ce.Message = resp.Error.Code, resp.Error.Message
			ce.Retryable, ce.RetryAfterMS = resp.Error.Data.Retryable, resp.Error.Data.RetryAfterMS
		}
		return nil, ce
	}
	return resp.Result, nil
}

// readLine reads one newline-terminated frame, refusing one over the cap.
func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, isPrefix, err := r.ReadLine()
		if err != nil {
			return nil, err
		}
		line = append(line, chunk...)
		if len(line) > maxResponseBytes {
			return nil, fmt.Errorf("reply exceeds %d bytes", maxResponseBytes)
		}
		if !isPrefix {
			return line, nil
		}
	}
}
