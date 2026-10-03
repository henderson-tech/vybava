package netfwd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/perflab/hostexec"
	"github.com/henderson-tech/vybava/internal/runx"
)

// fakePhone is adb plus the phone behind it: reverses live in a table, and
// `shell` replays the toybox nc request over the reverse for real, so the
// phone-side health check exercises the Mac-side forwarder end to end.
type fakePhone struct {
	mu        sync.Mutex
	serial    string
	offline   bool
	reverses  map[string]string // device side -> host side
	shellOut  string            // when set, `shell` answers this instead
	listenPID func() []int      // lsof answer
	calls     []string
}

func (p *fakePhone) Run(ctx context.Context, c hostexec.Cmd) (hostexec.Result, error) {
	p.mu.Lock()
	p.calls = append(p.calls, strings.Join(c.Argv, " "))
	p.mu.Unlock()
	if c.Argv[0] == "lsof" {
		var b strings.Builder
		for _, pid := range p.listenPID() {
			fmt.Fprintf(&b, "%d\n", pid)
		}
		if b.Len() == 0 {
			return hostexec.Result{Exit: 1}, nil
		}
		return hostexec.Result{Stdout: []byte(b.String())}, nil
	}
	if c.Argv[0] != "adb" || c.Argv[1] != "-s" || c.Argv[2] != p.serial {
		return hostexec.Result{}, fmt.Errorf("unexpected %v", c.Argv)
	}
	if p.offline {
		return hostexec.Result{Exit: 1, Stderr: []byte("adb: device '" + p.serial + "' not found\n")}, nil
	}
	args := c.Argv[3:]
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case args[0] == "reverse" && args[1] == "--list":
		var b strings.Builder
		for d, h := range p.reverses {
			fmt.Fprintf(&b, "UsbFfs %s %s\n", d, h)
		}
		return hostexec.Result{Stdout: []byte(b.String())}, nil
	case args[0] == "reverse" && args[1] == "--remove":
		if _, ok := p.reverses[args[2]]; !ok {
			return hostexec.Result{Exit: 1, Stderr: []byte("adb: error: listener '" + args[2] + "' not found\n")}, nil
		}
		delete(p.reverses, args[2])
		return hostexec.Result{}, nil
	case args[0] == "reverse":
		p.reverses[args[1]] = args[2]
		return hostexec.Result{}, nil
	case args[0] == "shell":
		if p.shellOut != "" {
			return hostexec.Result{Stdout: []byte(p.shellOut)}, nil
		}
		return p.ncOverReverse(ctx, args[1])
	}
	return hostexec.Result{}, fmt.Errorf("unexpected adb %v", args)
}

var ncScript = regexp.MustCompile(`GET (\S+) HTTP/1\.0.*nc -w 8 127\.0\.0\.1 (\d+)$`)

// ncOverReverse is the phone dialing its localhost: through the reverse
// table to the Mac-side port, then a raw HTTP/1.0 request like toybox nc.
func (p *fakePhone) ncOverReverse(ctx context.Context, script string) (hostexec.Result, error) {
	m := ncScript.FindStringSubmatch(script)
	if m == nil {
		return hostexec.Result{}, fmt.Errorf("unexpected script %q", script)
	}
	host, ok := p.reverses["tcp:"+m[2]]
	if !ok {
		return hostexec.Result{Exit: 1}, nil // nc: connection refused, no output
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", "127.0.0.1:"+strings.TrimPrefix(host, "tcp:"))
	if err != nil {
		return hostexec.Result{Exit: 1}, nil
	}
	defer c.Close()
	fmt.Fprintf(c, "GET %s HTTP/1.0\r\nHost: localhost\r\nConnection: close\r\n\r\n", m[1])
	b, _ := io.ReadAll(c)
	return hostexec.Result{Stdout: b}, nil
}

func (p *fakePhone) hasReverse(port int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.reverses["tcp:"+strconv.Itoa(port)]
	return ok
}

// origin is a stand-in Devbox API answering its readiness path.
func origin(t *testing.T, status int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/health/ready" {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

const testPID = 4242

func newEnv(t *testing.T, phone *fakePhone) Env {
	t.Helper()
	var d net.Dialer
	return Env{
		Run: phone, ADB: "adb", HTTPGet: HTTPGet, Listen: net.Listen, Dial: d.DialContext,
		StateDir: t.TempDir(), Now: time.Now, Sleep: func(time.Duration) {}, Pid: testPID,
		Terminate: func(int) error { return errors.New("not in this test") }, Heartbeat: time.Hour,
	}
}

func newPhone() *fakePhone {
	return &fakePhone{serial: "RF8N21PY1BF", reverses: map[string]string{}, listenPID: func() []int { return nil }}
}

func spec(srv *httptest.Server, port int) Spec {
	return Spec{DeviceID: "s20", Serial: "RF8N21PY1BF", Lease: "tok", DevicePort: port, Origin: srv.URL, Health: "/api/v1/health/ready", Hold: "devbox hold fixit-work-x --for 4h"}
}

func code(t *testing.T, err error) string {
	t.Helper()
	var d runx.DiagError
	if !errors.As(err, &d) {
		t.Fatalf("want a DiagError, got %v", err)
	}
	return d.Diag.Code
}

func TestParseReverseList(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("testdata", "adb-reverse-list.txt"))
	if err != nil {
		t.Fatal(err)
	}
	got := ParseReverseList(string(b))
	if len(got) != 1 || got[0] != (Reverse{Transport: "UsbFfs", Device: "tcp:8081", Host: "tcp:8081"}) {
		t.Fatalf("got %+v", got)
	}
	if !hasReverse(got, 8081, 8081) || hasReverse(got, 23936, 23936) {
		t.Fatal("hasReverse")
	}
	if len(ParseReverseList("")) != 0 {
		t.Fatal("empty list")
	}
}

func TestForwardLifecycle(t *testing.T) {
	srv := origin(t, http.StatusOK)
	port := freePort(t)
	phone := newPhone()
	env := newEnv(t, phone)
	s := spec(srv, port)
	listening := make(chan struct{})
	var once sync.Once
	phone.listenPID = func() []int {
		select {
		case <-listening:
			return []int{testPID}
		default:
			return nil
		}
	}
	var progress strings.Builder
	var mu sync.Mutex
	env.Log = writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(string(p), "phase=forwarding") {
			once.Do(func() { close(listening) })
		}
		return progress.Write(p)
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var res Result
	var ferr error
	go func() { res, ferr = Forward(ctx, env, s); close(done) }()
	select {
	case <-listening:
	case <-done:
		t.Fatalf("forward ended early: %v", ferr)
	case <-time.After(10 * time.Second):
		t.Fatal("forward never reached phase=forwarding")
	}
	if !phone.hasReverse(port) {
		t.Fatal("no reverse while forwarding")
	}
	if _, err := os.Stat(recordPath(env, s)); err != nil {
		t.Fatalf("no record while forwarding: %v", err)
	}
	// The app's request, through the forward, on both loopbacks.
	for _, host := range []string{"127.0.0.1", "[::1]"} {
		code, err := HTTPGet(context.Background(), fmt.Sprintf("http://%s:%d/api/v1/health/ready", host, port), 5*time.Second)
		if host == "[::1]" && err != nil {
			continue // a CI box without an IPv6 loopback; Start warned
		}
		if err != nil || code != 200 {
			t.Fatalf("GET via %s: %d %v", host, code, err)
		}
	}
	st, err := Status(context.Background(), env, s)
	if err != nil || len(st.Diagnostics) != 0 {
		t.Fatalf("status of a healthy forward: %v %+v", err, st.Diagnostics)
	}
	if d := st.Data.(StatusData); !d.Reversed || !d.Owned || d.PhoneHealth != 200 || d.HostHealth != 200 {
		t.Fatalf("status data %+v", d)
	}
	// A second `net forward` for the same port and origin reuses this one.
	other := env
	other.Pid = testPID + 1
	again, err := Forward(context.Background(), other, s)
	if err != nil || !again.Data.(ForwardData).Reused || again.Data.(ForwardData).PID != testPID {
		t.Fatalf("second forward: %v %+v", err, again.Data)
	}
	// `run` asks the same question before starting its own forward.
	if rec, ok := Serving(context.Background(), other, s); !ok || rec.PID != testPID {
		t.Fatalf("Serving = %+v %v, want the live forward %d", rec, ok, testPID)
	}
	if _, ok := Serving(context.Background(), env, s); ok {
		t.Fatal("a process never reuses its own forward record")
	}
	cancel()
	<-done
	if ferr != nil {
		t.Fatal(ferr)
	}
	data := res.Data.(ForwardData)
	if data.Stats.Connections < 3 || data.Stats.DialErrors != 0 {
		t.Fatalf("stats %+v", data.Stats)
	}
	if phone.hasReverse(port) {
		t.Fatal("reverse left behind after SIGINT")
	}
	if _, err := os.Stat(recordPath(env, s)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("record left behind")
	}
	if l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port)); err != nil {
		t.Fatalf("port still held after stop: %v", err)
	} else {
		_ = l.Close()
	}
	mu.Lock()
	out := progress.String()
	mu.Unlock()
	for _, want := range []string{"perflab[net s20] phase=start port=", "perflab[net s20] phase=forwarding", "perflab[net s20] phase=stopping"} {
		if !strings.Contains(out, want) {
			t.Errorf("progress lacks %q:\n%s", want, out)
		}
	}
}

type writerFunc func(p []byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

func TestStartFailuresLeaveNothingBehind(t *testing.T) {
	cases := []struct {
		name   string
		status int
		setup  func(t *testing.T, phone *fakePhone, port int)
		want   string
		fix    string
	}{
		{name: "origin down (devbox parked)", status: 503, want: DiagAPIUnreachable, fix: "devbox hold"},
		{name: "port held by another process", status: 200, want: DiagForwardDown, fix: "lsof -nP -iTCP:",
			setup: func(t *testing.T, _ *fakePhone, port int) {
				l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = l.Close() })
			}},
		{name: "phone unplugged", status: 200, want: DiagDeviceOffline, fix: "perflab device probe s20",
			setup: func(_ *testing.T, phone *fakePhone, _ int) { phone.offline = true }},
		{name: "phone gets an error page", status: 200, want: DiagForwardDown, fix: "perflab net forward --device s20 --lease tok",
			setup: func(_ *testing.T, phone *fakePhone, _ int) {
				phone.shellOut = "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := origin(t, tc.status)
			port := freePort(t)
			phone := newPhone()
			if tc.setup != nil {
				tc.setup(t, phone, port)
			}
			env := newEnv(t, phone)
			_, _, err := Start(context.Background(), env, spec(srv, port))
			var d runx.DiagError
			if !errors.As(err, &d) || d.Diag.Code != tc.want || !strings.Contains(d.Diag.Fix, tc.fix) {
				t.Fatalf("got %v, want %s with fix containing %q", err, tc.want, tc.fix)
			}
			if phone.hasReverse(port) {
				t.Fatal("a failed start left the reverse")
			}
			if tc.setup == nil || tc.want != DiagForwardDown || !strings.Contains(tc.fix, "lsof") {
				if l, lerr := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port)); lerr != nil {
					t.Fatalf("a failed start left the port held: %v", lerr)
				} else {
					_ = l.Close()
				}
			}
		})
	}
}

func TestStatusNamesEveryMissingLink(t *testing.T) {
	srv := origin(t, http.StatusOK)
	phone := newPhone()
	env := newEnv(t, phone)
	s := spec(srv, freePort(t))
	res, err := Status(context.Background(), env, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Diagnostics) != 2 {
		t.Fatalf("want no-reverse + nothing-listening, got %+v", res.Diagnostics)
	}
	for _, d := range res.Diagnostics {
		if d.Code != DiagForwardDown || d.Fix != ForwardCommand(s) {
			t.Fatalf("diag %+v", d)
		}
	}
	if len(res.Next) != 1 || res.Next[0] != ForwardCommand(s) {
		t.Fatalf("next %v", res.Next)
	}
	phone.offline = true
	if _, err := Status(context.Background(), env, s); code(t, err) != DiagDeviceOffline {
		t.Fatal("an unplugged phone must be DEVICE_OFFLINE")
	}
}

func TestStopSignalsOnlyTheRecordedListener(t *testing.T) {
	srv := origin(t, http.StatusOK)
	for _, tc := range []struct {
		name      string
		listening []int
		wantPID   int
		stale     bool
	}{
		{"live forwarder still holds the port", []int{777}, 777, false},
		{"pid gone (or recycled) never signalled", []int{}, 0, true},
		{"port held by someone else", []int{999}, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			phone := newPhone()
			env := newEnv(t, phone)
			s := spec(srv, 23936)
			phone.reverses["tcp:23936"] = "tcp:23936"
			if err := writeRecord(env, s, Record{PID: 777, DeviceID: "s20", Serial: s.Serial, DevicePort: 23936, HostPort: 23936, Origin: s.Origin}); err != nil {
				t.Fatal(err)
			}
			var signalled []int
			current := tc.listening
			phone.listenPID = func() []int { return current }
			env.Terminate = func(pid int) error { signalled = append(signalled, pid); current = nil; return nil }
			res, err := Stop(context.Background(), env, s)
			if err != nil {
				t.Fatal(err)
			}
			d := res.Data.(StopData)
			if d.StoppedPID != tc.wantPID || d.StaleRecord != tc.stale || !d.ReverseRemoved {
				t.Fatalf("stop data %+v", d)
			}
			if tc.wantPID == 0 && len(signalled) != 0 {
				t.Fatalf("signalled %v without proof the pid holds the port", signalled)
			}
			if phone.hasReverse(23936) {
				t.Fatal("reverse not removed")
			}
			if _, ok := readRecord(env, s); ok {
				t.Fatal("record not removed")
			}
		})
	}
}

func TestHeartbeatReaddsADroppedReverse(t *testing.T) {
	srv := origin(t, http.StatusOK)
	port := freePort(t)
	phone := newPhone()
	env := newEnv(t, phone)
	env.Heartbeat = 20 * time.Millisecond
	ready := make(chan struct{})
	var once sync.Once
	env.Log = writerFunc(func(p []byte) (int, error) {
		if strings.Contains(string(p), "phase=forwarding") {
			once.Do(func() { close(ready) })
		}
		return len(p), nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() {
		r, err := Forward(ctx, env, spec(srv, port))
		if err != nil {
			t.Error(err)
		}
		done <- r
	}()
	<-ready
	phone.mu.Lock()
	delete(phone.reverses, "tcp:"+strconv.Itoa(port)) // the USB drop
	phone.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for !phone.hasReverse(port) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	r := <-done
	if r.Data.(ForwardData).Readds < 1 {
		t.Fatalf("the dropped reverse was not re-added: %+v", r.Data)
	}
}

func TestRedactAndUsage(t *testing.T) {
	if got := Redact("http://user:secret@10.8.0.10:21936/api?token=x#f"); got != "http://10.8.0.10:21936/api" {
		t.Fatalf("Redact = %q", got)
	}
	_, _, err := Start(context.Background(), Env{}, Spec{Serial: "x", DevicePort: 0})
	if code(t, err) != DiagUsage {
		t.Fatal("port 0 must be USAGE")
	}
	_, _, err = Start(context.Background(), Env{}, Spec{Serial: "x", DevicePort: 23936, Origin: "10.8.0.10:21936"})
	if code(t, err) != DiagUsage {
		t.Fatal("a scheme-less origin must be USAGE")
	}
}
