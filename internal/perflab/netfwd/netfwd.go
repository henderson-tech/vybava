package netfwd

import (
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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/henderson-tech/vybava/internal/perflab/hostexec"
	"github.com/henderson-tech/vybava/internal/runx"
)

// Spec is one phone's forward. DevicePort is the port the perf build bakes
// (http://localhost:<DevicePort>); the Mac listens on the same port.
type Spec struct {
	DeviceID   string // ledger id (fix lines, record name)
	Serial     string // adb serial
	Lease      string // carried into fix lines; the verb layer validates it
	DevicePort int
	Origin     string // host-side API origin, e.g. http://10.8.0.10:21936
	Health     string // health path, e.g. /api/v1/health/ready
	Hold       string // the adapter's api.hold command, API_UNREACHABLE's fix
}

// Env is how the package reaches the Mac and the phone; tests swap fields.
type Env struct {
	Run       hostexec.Runner
	ADB       string
	HTTPGet   func(ctx context.Context, url string, timeout time.Duration) (int, error)
	Listen    func(network, address string) (net.Listener, error)
	Dial      func(ctx context.Context, network, address string) (net.Conn, error)
	StateDir  string
	Now       func() time.Time
	Sleep     func(time.Duration)
	Log       io.Writer // progress (stderr)
	Pid       int
	Terminate func(pid int) error
	// Heartbeat is how often a running forward re-checks its reverse and
	// prints a `still` line (30 s by default).
	Heartbeat time.Duration
}

// DefaultEnv is the real Mac; StateDir is $PERFLAB_STATE_DIR or
// ~/.local/state/perflab.
func DefaultEnv(log io.Writer) (Env, error) {
	state := os.Getenv("PERFLAB_STATE_DIR")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Env{}, err
		}
		state = filepath.Join(home, ".local", "state", "perflab")
	}
	var d net.Dialer
	return Env{
		Run: hostexec.OS{}, ADB: "adb", HTTPGet: HTTPGet, Listen: net.Listen, Dial: d.DialContext,
		StateDir: state, Now: time.Now, Sleep: time.Sleep, Log: log, Pid: os.Getpid(),
		Terminate: hostexec.Terminate, Heartbeat: 30 * time.Second,
	}, nil
}

// HTTPGet is the health probe: one GET, status only, bounded by timeout.
func HTTPGet(ctx context.Context, rawURL string, timeout time.Duration) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

// HealthTimeout is the design's 8 s health budget.
const HealthTimeout = 8 * time.Second

// ForwardCommand is the exact command FORWARD_DOWN names.
func ForwardCommand(s Spec) string {
	return fmt.Sprintf("perflab net forward --device %s --lease %s --device-port %d", orDefault(s.DeviceID, s.Serial), orDefault(s.Lease, "<token>"), s.DevicePort)
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func (s Spec) validate() error {
	if s.Serial == "" {
		return diag(DiagUsage, "no adb serial for the device", "perflab device show "+orDefault(s.DeviceID, "<id>")+" --json")
	}
	if s.DevicePort <= 0 || s.DevicePort > 65535 {
		return diag(DiagUsage, fmt.Sprintf("device port %d is outside 1..65535", s.DevicePort), "pass --device-port <port> (the adapter's api.device.android.devicePort)")
	}
	return nil
}

// target is the origin's host:port.
func (s Spec) target() (string, error) {
	u, err := url.Parse(s.Origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return "", diag(DiagUsage, fmt.Sprintf("origin %q is not an http(s) URL", Redact(s.Origin)), "perflab adapter check --json (api.origin)")
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[u.Scheme]
	}
	return net.JoinHostPort(u.Hostname(), port), nil
}

// Redact drops userinfo, query and fragment from a URL before it reaches
// an envelope.
func Redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable url>"
	}
	u.User, u.RawQuery, u.Fragment = nil, "", ""
	return u.String()
}

func (s Spec) originHealthURL() string {
	return strings.TrimRight(s.Origin, "/") + s.Health
}

// Reverse is one `adb reverse --list` row: the phone-side and Mac-side
// sockets.
type Reverse struct {
	Transport string `json:"transport"`
	Device    string `json:"device"`
	Host      string `json:"host"`
}

// ParseReverseList reads `adb -s <serial> reverse --list` ("UsbFfs
// tcp:8081 tcp:8081" per row).
func ParseReverseList(out string) []Reverse {
	rows := []Reverse{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 || !strings.Contains(f[1], ":") {
			continue
		}
		rows = append(rows, Reverse{Transport: f[0], Device: f[1], Host: f[2]})
	}
	return rows
}

func hasReverse(rows []Reverse, devicePort, hostPort int) bool {
	return slices.Contains(rows, Reverse{Transport: rowsTransport(rows, devicePort), Device: "tcp:" + strconv.Itoa(devicePort), Host: "tcp:" + strconv.Itoa(hostPort)})
}

func rowsTransport(rows []Reverse, devicePort int) string {
	for _, r := range rows {
		if r.Device == "tcp:"+strconv.Itoa(devicePort) {
			return r.Transport
		}
	}
	return ""
}

var offlineOutput = regexp.MustCompile(`(?i)device '.*' not found|device offline|unauthorized|no devices/emulators found|device not found`)

// adb runs `adb -s <serial> args...`, mapping a missing adb and a missing
// device to their diagnostics. Any other non-zero exit is returned as is.
func (e Env) adb(ctx context.Context, s Spec, timeout time.Duration, args ...string) (hostexec.Result, error) {
	argv := append([]string{e.ADB, "-s", s.Serial}, args...)
	res, err := e.Run.Run(ctx, hostexec.Cmd{Argv: argv, Timeout: timeout})
	if err != nil {
		if hostexec.NotFound(err) {
			return res, diag(DiagToolMissing, "adb is not installed", "brew install --cask android-platform-tools")
		}
		return res, err
	}
	if res.Exit != 0 && offlineOutput.MatchString(res.Combined()) {
		return res, diag(DiagDeviceOffline, fmt.Sprintf("adb does not see %s: %s", s.Serial, res.Tail()), "perflab device probe "+orDefault(s.DeviceID, s.Serial)+" --json")
	}
	return res, nil
}

// ReverseList reads the phone's reverses.
func ReverseList(ctx context.Context, env Env, s Spec) ([]Reverse, error) {
	res, err := env.adb(ctx, s, 15*time.Second, "reverse", "--list")
	if err != nil {
		return nil, err
	}
	if res.Exit != 0 {
		return nil, fmt.Errorf("adb reverse --list: %s", res.Tail())
	}
	return ParseReverseList(string(res.Stdout)), nil
}

var safeHealth = regexp.MustCompile(`^/[A-Za-z0-9._~/%?=&-]*$`)
var statusLine = regexp.MustCompile(`HTTP/\d(?:\.\d)?\s+(\d{3})`)

// errNoNC marks a phone whose shell has no nc, so the phone-side check is
// skipped (warning), never failed.
var errNoNC = errors.New("the phone's shell has no nc")

// PhoneHealth asks the PHONE for the health path on its own localhost:<port>
// (toybox nc over adb shell): the one check that proves the app's baked
// origin works end to end.
func PhoneHealth(ctx context.Context, env Env, s Spec) (int, error) {
	if !safeHealth.MatchString(s.Health) {
		return 0, fmt.Errorf("health path %q is not shell-safe, phone-side check skipped", s.Health)
	}
	// toybox nc (Android 13) drops the connection the moment stdin hits
	// EOF, before the answer arrives, so stdin is held open (verified on
	// the S20: a bare `printf | nc` prints nothing, this form prints 200).
	script := fmt.Sprintf(`(printf 'GET %s HTTP/1.0\r\nHost: localhost\r\nConnection: close\r\n\r\n'; sleep 5) | nc -w 8 127.0.0.1 %d`, s.Health, s.DevicePort)
	res, err := env.adb(ctx, s, 20*time.Second, "shell", script)
	if err != nil {
		return 0, err
	}
	out := res.Combined()
	if strings.Contains(out, "nc: not found") || strings.Contains(out, "nc: inaccessible or not found") {
		return 0, errNoNC
	}
	if m := statusLine.FindStringSubmatch(out); m != nil {
		code, _ := strconv.Atoi(m[1])
		return code, nil
	}
	if res.Exit != 0 || strings.TrimSpace(out) == "" {
		return 0, fmt.Errorf("no HTTP answer on the phone's localhost:%d (%s)", s.DevicePort, res.Tail())
	}
	return 0, fmt.Errorf("unexpected answer on the phone's localhost:%d: %.80q", s.DevicePort, out)
}

func ok2xx(code int) bool { return code >= 200 && code < 300 }

// Stats counts the forward's traffic.
type Stats struct {
	Connections int64 `json:"connections"`
	Open        int64 `json:"open"`
	BytesUp     int64 `json:"bytesUp"`
	BytesDown   int64 `json:"bytesDown"`
	DialErrors  int64 `json:"dialErrors"`
}

// Forwarder is a running forward: listeners on both loopbacks plus the
// phone's reverse. Close undoes both.
type Forwarder struct {
	env       Env
	spec      Spec
	target    string
	listeners []net.Listener
	hostPort  int
	reversed  bool
	closed    atomic.Bool
	wg        sync.WaitGroup
	mu        sync.Mutex
	open      map[net.Conn]struct{}
	conns     atomic.Int64
	active    atomic.Int64
	up, down  atomic.Int64
	dialErrs  atomic.Int64
}

// Addrs are the listening addresses.
func (f *Forwarder) Addrs() []string {
	out := make([]string, 0, len(f.listeners))
	for _, l := range f.listeners {
		out = append(out, l.Addr().String())
	}
	return out
}

// HostPort is the bound Mac-side port.
func (f *Forwarder) HostPort() int { return f.hostPort }

// Stats is a snapshot of the traffic counters.
func (f *Forwarder) Stats() Stats {
	return Stats{Connections: f.conns.Load(), Open: f.active.Load(), BytesUp: f.up.Load(), BytesDown: f.down.Load(), DialErrors: f.dialErrs.Load()}
}

func isAddrInUse(err error) bool { return errors.Is(err, syscall.EADDRINUSE) }

// listen binds 127.0.0.1:<port> and [::1]:<same port>. A busy port is
// FORWARD_DOWN naming the holder lookup; a Mac without an IPv6 loopback
// only warns.
func (f *Forwarder) listen() ([]runx.Diagnostic, error) {
	port := f.spec.DevicePort
	l4, err := f.env.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		if isAddrInUse(err) {
			return nil, diag(DiagForwardDown, fmt.Sprintf("127.0.0.1:%d is already in use by another process", port), fmt.Sprintf("lsof -nP -iTCP:%d -sTCP:LISTEN", port))
		}
		return nil, err
	}
	f.listeners = append(f.listeners, l4)
	f.hostPort = l4.Addr().(*net.TCPAddr).Port
	var warns []runx.Diagnostic
	l6, err := f.env.Listen("tcp6", net.JoinHostPort("::1", strconv.Itoa(f.hostPort)))
	switch {
	case err == nil:
		f.listeners = append(f.listeners, l6)
	case isAddrInUse(err):
		_ = l4.Close()
		f.listeners = nil
		return nil, diag(DiagForwardDown, fmt.Sprintf("[::1]:%d is already in use by another process", f.hostPort), fmt.Sprintf("lsof -nP -iTCP:%d -sTCP:LISTEN", f.hostPort))
	default:
		warns = append(warns, row("warning", DiagForwardDown, fmt.Sprintf("no IPv6 loopback listener on [::1]:%d (%v); an adb that dials ::1 will not connect", f.hostPort, err), ""))
	}
	for _, l := range f.listeners {
		go f.serve(l)
	}
	return warns, nil
}

func (f *Forwarder) serve(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			if f.closed.Load() || errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		f.wg.Add(1)
		go f.handle(c)
	}
}

func (f *Forwarder) track(c net.Conn, add bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if add {
		f.open[c] = struct{}{}
	} else {
		delete(f.open, c)
	}
}

func (f *Forwarder) handle(c net.Conn) {
	defer f.wg.Done()
	f.conns.Add(1)
	f.active.Add(1)
	defer f.active.Add(-1)
	f.track(c, true)
	defer f.track(c, false)
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	up, err := f.env.Dial(ctx, "tcp", f.target)
	cancel()
	if err != nil {
		f.dialErrs.Add(1)
		return
	}
	f.track(up, true)
	defer f.track(up, false)
	defer up.Close()
	done := make(chan struct{}, 2)
	pipe := func(dst, src net.Conn, n *atomic.Int64) {
		copied, _ := io.Copy(dst, src)
		n.Add(copied)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		done <- struct{}{}
	}
	go pipe(up, c, &f.up)
	go pipe(c, up, &f.down)
	<-done
	<-done
}

// Close stops listening, drops open connections and removes the phone's
// reverse. Safe to call twice.
func (f *Forwarder) Close(ctx context.Context) error {
	if !f.closed.CompareAndSwap(false, true) {
		return nil
	}
	for _, l := range f.listeners {
		_ = l.Close()
	}
	f.mu.Lock()
	for c := range f.open {
		_ = c.Close()
	}
	f.mu.Unlock()
	waited := make(chan struct{})
	go func() { f.wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
	}
	if !f.reversed {
		return nil
	}
	return removeReverse(ctx, f.env, f.spec)
}

// removeReverse drops `tcp:<devicePort>`; an already-missing reverse (or
// a gone phone) is not an error.
func removeReverse(ctx context.Context, env Env, s Spec) error {
	res, err := env.adb(ctx, s, 15*time.Second, "reverse", "--remove", "tcp:"+strconv.Itoa(s.DevicePort))
	var d runx.DiagError
	if errors.As(err, &d) && d.Diag.Code == DiagDeviceOffline {
		return nil
	}
	if err != nil {
		return err
	}
	if res.Exit != 0 && !strings.Contains(res.Combined(), "not found") {
		return fmt.Errorf("adb reverse --remove tcp:%d: %s", s.DevicePort, res.Tail())
	}
	return nil
}

// Start brings the whole chain up and proves it: the origin answers from
// the Mac, both loopbacks listen, the reverse is listed, the health path
// answers through the forward and on the phone's own localhost. On any
// failure it undoes what it set up. The returned rows are warnings.
func Start(ctx context.Context, env Env, s Spec) (*Forwarder, []runx.Diagnostic, error) {
	if err := s.validate(); err != nil {
		return nil, nil, err
	}
	target, err := s.target()
	if err != nil {
		return nil, nil, err
	}
	if code, herr := env.HTTPGet(ctx, s.originHealthURL(), HealthTimeout); herr != nil || !ok2xx(code) {
		return nil, nil, unreachable(s, code, herr)
	}
	f := &Forwarder{env: env, spec: s, target: target, open: map[net.Conn]struct{}{}}
	warns, err := f.listen()
	if err != nil {
		return nil, nil, err
	}
	fail := func(e error) (*Forwarder, []runx.Diagnostic, error) {
		_ = f.Close(context.WithoutCancel(ctx))
		return nil, nil, e
	}
	mapping := []string{"reverse", "tcp:" + strconv.Itoa(s.DevicePort), "tcp:" + strconv.Itoa(f.hostPort)}
	res, err := env.adb(ctx, s, 15*time.Second, mapping...)
	if err != nil {
		return fail(err)
	}
	if res.Exit != 0 {
		return fail(diag(DiagForwardDown, "adb reverse failed: "+res.Tail(), ForwardCommand(s)))
	}
	f.reversed = true
	rows, err := ReverseList(ctx, env, s)
	if err != nil {
		return fail(err)
	}
	if !hasReverse(rows, s.DevicePort, f.hostPort) {
		return fail(diag(DiagForwardDown, fmt.Sprintf("adb reverse tcp:%d tcp:%d is not listed after setting it", s.DevicePort, f.hostPort), ForwardCommand(s)))
	}
	hostURL := fmt.Sprintf("http://127.0.0.1:%d%s", f.hostPort, s.Health)
	var code int
	var herr error
	for attempt := range 3 {
		if attempt > 0 {
			env.Sleep(time.Second)
		}
		if code, herr = env.HTTPGet(ctx, hostURL, HealthTimeout); herr == nil && ok2xx(code) {
			break
		}
	}
	if herr != nil || !ok2xx(code) {
		return fail(diag(DiagForwardDown, fmt.Sprintf("GET %s through the forward answered %s", hostURL, Answer(code, herr)), ForwardCommand(s)))
	}
	phone, perr := PhoneHealth(ctx, env, s)
	switch {
	case perr == nil && ok2xx(phone):
	case errors.Is(perr, errNoNC):
		warns = append(warns, row("warning", DiagForwardDown, "phone-side check skipped: "+perr.Error(), ""))
	default:
		var d runx.DiagError
		if errors.As(perr, &d) {
			return fail(perr)
		}
		return fail(diag(DiagForwardDown, fmt.Sprintf("the phone's own localhost:%d%s answered %s", s.DevicePort, s.Health, Answer(phone, perr)), ForwardCommand(s)))
	}
	return f, warns, nil
}

// Answer renders a health probe outcome for a detail: the status code, or the
// transport error without Go's repeated "Get \"<url>\":" prefix.
func Answer(code int, err error) string {
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			return ue.Err.Error()
		}
		return err.Error()
	}
	return strconv.Itoa(code)
}

func unreachable(s Spec, code int, err error) runx.DiagError {
	fix := s.Hold
	if fix == "" {
		fix = "curl -sS -o /dev/null -w '%{http_code}' " + Redact(s.originHealthURL())
	}
	return diag(DiagAPIUnreachable, fmt.Sprintf("GET %s from the Mac answered %s, so no forward can reach it", Redact(s.originHealthURL()), Answer(code, err)), fix)
}

// Record is the running forward's state file
// (<state>/net/<device>-<port>.json): it lets `net status` and `net stop`
// find the perflab process that owns the port.
type Record struct {
	PID        int       `json:"pid"`
	DeviceID   string    `json:"deviceId"`
	Serial     string    `json:"serial"`
	DevicePort int       `json:"devicePort"`
	HostPort   int       `json:"hostPort"`
	Origin     string    `json:"origin"`
	StartedAt  time.Time `json:"startedAt"`
}

func recordPath(env Env, s Spec) string {
	return filepath.Join(env.StateDir, "net", fmt.Sprintf("%s-%d.json", orDefault(s.DeviceID, s.Serial), s.DevicePort))
}

func writeRecord(env Env, s Spec, r Record) error {
	path := recordPath(env, s)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readRecord(env Env, s Spec) (Record, bool) {
	b, err := os.ReadFile(recordPath(env, s))
	if err != nil {
		return Record{}, false
	}
	var r Record
	if json.Unmarshal(b, &r) != nil {
		return Record{}, false
	}
	return r, true
}

// listeners are the pids listening on 127.0.0.1/::1:<port> (lsof). A pid
// that still listens on the recorded port IS the recorded forwarder: a
// recycled pid cannot hold a port it never bound.
func listeners(ctx context.Context, env Env, port int) []int {
	res, err := env.Run.Run(ctx, hostexec.Cmd{Argv: []string{"lsof", "-nP", "-t", "-iTCP:" + strconv.Itoa(port), "-sTCP:LISTEN"}, Timeout: 10 * time.Second})
	if err != nil {
		return nil
	}
	var pids []int
	for _, f := range strings.Fields(string(res.Stdout)) {
		if pid, err := strconv.Atoi(f); err == nil && !slices.Contains(pids, pid) {
			pids = append(pids, pid)
		}
	}
	return pids
}

// ForwardData is `net forward`'s payload.
type ForwardData struct {
	Device     string    `json:"device"`
	Serial     string    `json:"serial"`
	DevicePort int       `json:"devicePort"`
	Origin     string    `json:"origin"`
	Listen     []string  `json:"listen,omitempty"`
	PID        int       `json:"pid"`
	Reused     bool      `json:"reused"`
	StartedAt  time.Time `json:"startedAt"`
	StoppedAt  time.Time `json:"stoppedAt,omitzero"`
	Reverse    string    `json:"reverse"`
	Readds     int       `json:"reverseReadded"`
	Stats      Stats     `json:"stats"`
}

// Result is what a verb hands the envelope.
type Result struct {
	Data        any
	Diagnostics []runx.Diagnostic
	Next        []string
}

// Serving returns the live perflab forward for the same device port and
// origin whose every link answers (a standalone `net forward` running as a
// background task, which doctor's FORWARD_DOWN fix starts). `net forward`
// and `run` reuse it: a second listener on the port fails "already in use".
func Serving(ctx context.Context, env Env, s Spec) (Record, bool) {
	rec, ok := readRecord(env, s)
	if !ok || rec.Origin != s.Origin || rec.PID == env.Pid || !slices.Contains(listeners(ctx, env, rec.DevicePort), rec.PID) {
		return Record{}, false
	}
	st, err := Status(ctx, env, s)
	return rec, err == nil && len(st.Diagnostics) == 0
}

// Forward is the `net forward` verb: it brings the chain up, records the
// owning pid, prints a `phase=forwarding` progress line once the phone is
// proven to reach the origin, re-adds the reverse when a USB drop removes
// it, and tears everything down when ctx ends (SIGINT/SIGTERM). A live
// healthy forward for the same port and origin is reused, not duplicated.
func Forward(ctx context.Context, env Env, s Spec) (Result, error) {
	if err := s.validate(); err != nil {
		return Result{}, err
	}
	statusNext := fmt.Sprintf("perflab net status --device %s --device-port %d --json", orDefault(s.DeviceID, s.Serial), s.DevicePort)
	if rec, ok := Serving(ctx, env, s); ok {
		return Result{Data: ForwardData{Device: s.DeviceID, Serial: s.Serial, DevicePort: s.DevicePort, Origin: Redact(s.Origin), PID: rec.PID, Reused: true, StartedAt: rec.StartedAt,
			Reverse: fmt.Sprintf("tcp:%d tcp:%d", s.DevicePort, rec.HostPort)}, Next: []string{statusNext}}, nil
	}
	prog := hostexec.NewProgress(env.Log, "net "+orDefault(s.DeviceID, s.Serial), env.Now)
	prog.Phase("start", fmt.Sprintf("port=%d", s.DevicePort), "origin="+Redact(s.Origin))
	f, warns, err := Start(ctx, env, s)
	if err != nil {
		return Result{}, err
	}
	started := env.Now().UTC()
	rec := Record{PID: env.Pid, DeviceID: s.DeviceID, Serial: s.Serial, DevicePort: s.DevicePort, HostPort: f.HostPort(), Origin: s.Origin, StartedAt: started}
	if err := writeRecord(env, s, rec); err != nil {
		_ = f.Close(context.WithoutCancel(ctx))
		return Result{}, err
	}
	defer os.Remove(recordPath(env, s))
	prog.Phase("forwarding", fmt.Sprintf("port=%d", f.HostPort()), "listen="+strings.Join(f.Addrs(), ","))
	every := env.Heartbeat
	if every <= 0 {
		every = 30 * time.Second
	}
	readds := 0
	tick := time.NewTicker(every)
	defer tick.Stop()
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-tick.C:
			st := f.Stats()
			state := "ok"
			if rows, rerr := ReverseList(ctx, env, s); rerr != nil {
				state = "phone-gone"
			} else if !hasReverse(rows, s.DevicePort, f.HostPort()) {
				state = "missing"
				if res, aerr := env.adb(ctx, s, 15*time.Second, "reverse", "tcp:"+strconv.Itoa(s.DevicePort), "tcp:"+strconv.Itoa(f.HostPort())); aerr == nil && res.Exit == 0 {
					state = "readded"
					readds++
				}
			}
			prog.Still(fmt.Sprintf("open=%d", st.Open), fmt.Sprintf("total=%d", st.Connections), "reverse="+state)
		}
	}
	prog.Phase("stopping")
	diags := append([]runx.Diagnostic{}, warns...)
	if cerr := f.Close(context.WithoutCancel(ctx)); cerr != nil {
		diags = append(diags, row("warning", DiagForwardDown, "the reverse could not be removed: "+cerr.Error(), fmt.Sprintf("perflab net stop --device %s --lease %s --device-port %d", orDefault(s.DeviceID, s.Serial), orDefault(s.Lease, "<token>"), s.DevicePort)))
	}
	return Result{
		Data: ForwardData{Device: s.DeviceID, Serial: s.Serial, DevicePort: s.DevicePort, Origin: Redact(s.Origin), Listen: f.Addrs(), PID: env.Pid,
			StartedAt: started, StoppedAt: env.Now().UTC(), Reverse: fmt.Sprintf("tcp:%d tcp:%d", s.DevicePort, f.HostPort()), Readds: readds, Stats: f.Stats()},
		Diagnostics: diags,
		Next:        []string{},
	}, nil
}

// StatusData is `net status`'s payload.
type StatusData struct {
	Device      string    `json:"device"`
	DevicePort  int       `json:"devicePort"`
	Reverses    []Reverse `json:"reverses"`
	Reversed    bool      `json:"reversed"`
	Listeners   []int     `json:"listeners"`
	Forwarder   *Record   `json:"forwarder,omitempty"`
	Owned       bool      `json:"owned"`
	HostHealth  int       `json:"hostHealth,omitempty"`
	PhoneHealth int       `json:"phoneHealth,omitempty"`
}

// Status inspects every link without changing anything; each missing or
// failing link is one diagnostic with its fix. Doctor's Android API check
// is this verb.
func Status(ctx context.Context, env Env, s Spec) (Result, error) {
	if err := s.validate(); err != nil {
		return Result{}, err
	}
	rows, err := ReverseList(ctx, env, s)
	if err != nil {
		return Result{}, err
	}
	data := StatusData{Device: orDefault(s.DeviceID, s.Serial), DevicePort: s.DevicePort, Reverses: rows, Listeners: listeners(ctx, env, s.DevicePort)}
	if rec, ok := readRecord(env, s); ok {
		data.Forwarder = &rec
		data.Owned = slices.Contains(data.Listeners, rec.PID)
	}
	hostPort := s.DevicePort
	if data.Forwarder != nil && data.Owned {
		hostPort = data.Forwarder.HostPort
	}
	data.Reversed = hasReverse(rows, s.DevicePort, hostPort)
	var diags []runx.Diagnostic
	fix := ForwardCommand(s)
	if !data.Reversed {
		diags = append(diags, row("error", DiagForwardDown, fmt.Sprintf("no adb reverse tcp:%d on %s: the app's baked http://localhost:%d goes nowhere", s.DevicePort, s.Serial, s.DevicePort), fix))
	}
	switch {
	case len(data.Listeners) == 0:
		diags = append(diags, row("error", DiagForwardDown, fmt.Sprintf("nothing listens on 127.0.0.1:%d on the Mac (`perflab net forward` serves it and streams until SIGINT: run it as a background task)", hostPort), fix))
	case !data.Owned:
		diags = append(diags, row("warning", DiagForwardDown, fmt.Sprintf("127.0.0.1:%d is held by pid %v, not a perflab forward; it may not reach the origin", hostPort, data.Listeners), fmt.Sprintf("lsof -nP -iTCP:%d -sTCP:LISTEN", hostPort)))
	}
	if len(data.Listeners) > 0 {
		code, herr := env.HTTPGet(ctx, fmt.Sprintf("http://127.0.0.1:%d%s", hostPort, s.Health), HealthTimeout)
		data.HostHealth = code
		if herr != nil || !ok2xx(code) {
			if s.Origin != "" {
				if oc, oerr := env.HTTPGet(ctx, s.originHealthURL(), HealthTimeout); oerr != nil || !ok2xx(oc) {
					diags = append(diags, unreachable(s, oc, oerr).Diag)
					return Result{Data: data, Diagnostics: diags, Next: nextOf(diags)}, nil
				}
			}
			diags = append(diags, row("error", DiagForwardDown, fmt.Sprintf("GET http://127.0.0.1:%d%s through the forward answered %s", hostPort, s.Health, Answer(code, herr)), fix))
		}
	}
	if data.Reversed && len(data.Listeners) > 0 && ok2xx(data.HostHealth) {
		phone, perr := PhoneHealth(ctx, env, s)
		data.PhoneHealth = phone
		switch {
		case perr == nil && ok2xx(phone):
		case errors.Is(perr, errNoNC):
			diags = append(diags, row("warning", DiagForwardDown, "phone-side check skipped: "+perr.Error(), ""))
		default:
			diags = append(diags, row("error", DiagForwardDown, fmt.Sprintf("the phone's own localhost:%d%s answered %s", s.DevicePort, s.Health, Answer(phone, perr)), fix))
		}
	}
	return Result{Data: data, Diagnostics: diags, Next: nextOf(diags)}, nil
}

func nextOf(diags []runx.Diagnostic) []string {
	next := []string{}
	for _, d := range diags {
		if d.Fix != "" && !slices.Contains(next, d.Fix) {
			next = append(next, d.Fix)
		}
	}
	return next
}

// StopData is `net stop`'s payload.
type StopData struct {
	Device         string `json:"device"`
	DevicePort     int    `json:"devicePort"`
	StoppedPID     int    `json:"stoppedPid,omitempty"`
	StaleRecord    bool   `json:"staleRecord"`
	ReverseRemoved bool   `json:"reverseRemoved"`
}

// Stop ends a recorded forward: SIGTERM to the literal pid its record names
// when that pid still holds the port (the forward then removes its own
// reverse), else the leftover reverse and record are cleared.
func Stop(ctx context.Context, env Env, s Spec) (Result, error) {
	if err := s.validate(); err != nil {
		return Result{}, err
	}
	data := StopData{Device: orDefault(s.DeviceID, s.Serial), DevicePort: s.DevicePort}
	if rec, ok := readRecord(env, s); ok {
		if rec.PID != env.Pid && slices.Contains(listeners(ctx, env, rec.DevicePort), rec.PID) {
			if err := env.Terminate(rec.PID); err != nil {
				return Result{}, fmt.Errorf("signal forwarder pid %d: %w", rec.PID, err)
			}
			data.StoppedPID = rec.PID
			for range 50 {
				if !slices.Contains(listeners(ctx, env, rec.DevicePort), rec.PID) {
					break
				}
				env.Sleep(100 * time.Millisecond)
			}
		} else {
			data.StaleRecord = true
		}
		_ = os.Remove(recordPath(env, s))
	}
	if err := removeReverse(ctx, env, s); err != nil {
		return Result{}, err
	}
	data.ReverseRemoved = true
	return Result{Data: data, Next: []string{}}, nil
}
