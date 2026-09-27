package blip

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
)

func TestParsers(t *testing.T) {
	if b, err := ParseRate("20kbps"); err != nil || b != 2500 {
		t.Fatalf("20kbps = %d, %v", b, err)
	}
	if b, err := ParseRate("1mbps"); err != nil || b != 125000 {
		t.Fatalf("1mbps = %d, %v", b, err)
	}
	if _, err := ParseRate("fast"); err == nil {
		t.Fatal("ParseRate(fast) accepted")
	}
	if d, err := ParseDuration("800ms"); err != nil || d != 800*time.Millisecond {
		t.Fatalf("800ms = %v, %v", d, err)
	}
	if down, up, err := ParseFlap("5s/10s"); err != nil || down != 5*time.Second || up != 10*time.Second {
		t.Fatalf("flap = %v/%v, %v", down, up, err)
	}
	if _, _, err := ParseFlap("5s"); err == nil {
		t.Fatal("ParseFlap(5s) accepted")
	}
	for _, bad := range []string{"0", "1.5", "x"} {
		if _, err := ParseProbability(bad); err == nil {
			t.Fatalf("ParseProbability(%q) accepted", bad)
		}
	}
	if !MatchPath("/api/orders*", "/api/orders/42/items") || MatchPath("/api/orders*", "/api/users") || !MatchPath("", "/anything") {
		t.Fatal("MatchPath: * must cross / and empty glob must match all")
	}
	if exp, err := ParseExpect("401, 403"); err != nil || len(exp) != 2 {
		t.Fatalf("ParseExpect = %v, %v", exp, err)
	}
}

func TestParseFaultAndScope(t *testing.T) {
	f, err := ParseFault("api", "http", []string{"delay", "800ms"}, ScopeFlags{Jitter: "200ms", Match: "/api/*", Method: "post", After: 3, For: "30s", Rate: "0.5"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Delay != 800*time.Millisecond || f.Jitter != 200*time.Millisecond || f.Method != "POST" || f.After != 3 || f.For != 30*time.Second || f.Rate != 0.5 {
		t.Fatalf("fault = %+v", f)
	}
	if got := f.String(); !strings.HasPrefix(got, "delay 800ms --jitter 200ms --match") {
		t.Fatalf("String() = %q", got)
	}
	for _, c := range []struct {
		args  []string
		flags ScopeFlags
		code  string
	}{
		{[]string{"error", "503"}, ScopeFlags{}, DiagHTTPOnly},
		{[]string{"drop"}, ScopeFlags{Match: "/x"}, DiagHTTPOnly},
		{[]string{"wobble"}, ScopeFlags{}, DiagFaultInvalid},
		{[]string{"slow"}, ScopeFlags{}, DiagFaultInvalid},
	} {
		_, err := ParseFault("db", "tcp", c.args, c.flags)
		if !hasCode(err, c.code) {
			t.Fatalf("%v on tcp: err = %v, want %s", c.args, err, c.code)
		}
	}
}

func TestFaultDecide(t *testing.T) {
	now := time.Now()
	fs := &faultState{Fault: Fault{Kind: "drop", Match: "/api/*", Method: "POST", After: 2, For: time.Minute, Rate: 1, SetAt: now}}
	if fs.decide("GET", "/api/x", now) || fs.decide("POST", "/other", now) {
		t.Fatal("method/path scope ignored")
	}
	if fs.decide("POST", "/api/x", now) || fs.decide("POST", "/api/x", now) {
		t.Fatal("--after 2 applied too early")
	}
	if !fs.decide("POST", "/api/x", now) {
		t.Fatal("third matching request should be faulted")
	}
	if fs.decide("POST", "/api/x", now.Add(2*time.Minute)) {
		t.Fatal("--for elapsed but still applied")
	}
	flap := &faultState{Fault: Fault{Kind: "flap", Down: time.Second, Up: 2 * time.Second, Rate: 1, SetAt: now}}
	if !flap.decide("", "", now.Add(500*time.Millisecond)) || flap.decide("", "", now.Add(1500*time.Millisecond)) {
		t.Fatal("flap windows wrong: down first second, up the next two")
	}
	half := &faultState{Fault: Fault{Kind: "drop", Rate: 0.5, SetAt: now}}
	hits := 0
	for i := 0; i < 400; i++ {
		if half.decide("", "", now) {
			hits++
		}
	}
	if hits < 120 || hits > 280 {
		t.Fatalf("rate 0.5 hit %d/400", hits)
	}
}

func startHTTP(t *testing.T, upstream string, recPath string) (*Server, *bytes.Buffer) {
	t.Helper()
	var logBuf bytes.Buffer
	s, err := NewServer(Config{Name: "t", Mode: "http", Listen: "127.0.0.1:0", Upstream: upstream}, &logBuf, recPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, &logBuf
}

func get(t *testing.T, url string) (*http.Response, string, error) {
	t.Helper()
	c := &http.Client{Timeout: 3 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := c.Get(url)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b), nil
}

func TestHTTPProxyFaults(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "hello "+r.URL.Path) }))
	defer up.Close()
	s, logBuf := startHTTP(t, up.URL, "")
	base := "http://" + s.Addr().String()

	if resp, body, err := get(t, base+"/a"); err != nil || resp.StatusCode != 200 || body != "hello /a" {
		t.Fatalf("pass-through: %v %v %q", resp, err, body)
	}
	s.SetFault(&Fault{Kind: "error", Status: 503, Body: `{"error":"x"}`, Match: "/api/*"})
	if resp, _, _ := get(t, base+"/other"); resp.StatusCode != 200 {
		t.Fatalf("--match leaked onto /other: %d", resp.StatusCode)
	}
	resp, body, err := get(t, base+"/api/orders")
	if err != nil || resp.StatusCode != 503 || body != `{"error":"x"}` || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("error fault: %v %v %q %q", resp, err, body, resp.Header.Get("Content-Type"))
	}
	s.SetFault(&Fault{Kind: "delay", Delay: 300 * time.Millisecond})
	start := time.Now()
	if resp, _, _ := get(t, base+"/a"); resp.StatusCode != 200 || time.Since(start) < 300*time.Millisecond {
		t.Fatalf("delay fault: status %d after %s", resp.StatusCode, time.Since(start))
	}
	s.SetFault(&Fault{Kind: "drop"})
	if _, _, err := get(t, base+"/a"); err == nil {
		t.Fatal("drop fault answered")
	}
	s.SetFault(&Fault{Kind: "timeout"})
	done := make(chan int, 1)
	go func() {
		resp, _, err := get(t, base+"/held")
		if err != nil {
			done <- -1
			return
		}
		done <- resp.StatusCode
	}()
	time.Sleep(150 * time.Millisecond)
	if st := s.Status(); st.Counters.InFlight != 1 || st.Fault.Kind != "timeout" {
		t.Fatalf("held request not in flight: %+v", st)
	}
	s.Ok()
	if code := <-done; code != 503 {
		t.Fatalf("released held request answered %d, want 503", code)
	}
	if resp, _, _ := get(t, base+"/a"); resp.StatusCode != 200 {
		t.Fatalf("after ok: %d", resp.StatusCode)
	}
	st := s.Status()
	if st.Counters.Requests != 7 || st.Counters.Faulted != 4 || st.Fault != nil {
		t.Fatalf("counters = %+v", st.Counters)
	}
	if log := logBuf.String(); !strings.Contains(log, "GET /api/orders fault=error status=503") || !strings.Contains(log, "fault=drop status=0") {
		t.Fatalf("log:\n%s", log)
	}
}

func TestHTTPFaultExpires(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s, _ := startHTTP(t, up.URL, "")
	s.SetFault(&Fault{Kind: "error", Status: 500, For: 100 * time.Millisecond})
	if resp, _, _ := get(t, "http://"+s.Addr().String()+"/"); resp.StatusCode != 500 {
		t.Fatalf("fresh fault not applied: %d", resp.StatusCode)
	}
	time.Sleep(150 * time.Millisecond)
	if resp, _, _ := get(t, "http://"+s.Addr().String()+"/"); resp.StatusCode != 200 || s.Status().Fault != nil {
		t.Fatalf("--for did not clear: %d %+v", resp.StatusCode, s.Status().Fault)
	}
}

func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

func TestTCPProxy(t *testing.T) {
	var logBuf bytes.Buffer
	s, err := NewServer(Config{Name: "db", Mode: "tcp", Listen: "127.0.0.1:0", Upstream: echoServer(t)}, &logBuf, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	roundtrip := func() error {
		c, err := net.DialTimeout("tcp", s.Addr().String(), time.Second)
		if err != nil {
			return err
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(time.Second))
		if _, err := c.Write([]byte("ping")); err != nil {
			return err
		}
		buf := make([]byte, 4)
		if _, err := io.ReadFull(c, buf); err != nil {
			return err
		}
		if string(buf) != "ping" {
			return errors.New("echo mismatch: " + string(buf))
		}
		return nil
	}
	if err := roundtrip(); err != nil {
		t.Fatalf("pass-through: %v", err)
	}
	s.SetFault(&Fault{Kind: "drop"})
	if err := roundtrip(); err == nil {
		t.Fatal("drop fault let bytes through")
	}
	s.Ok()
	held, err := net.Dial("tcp", s.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	_, _ = held.Write([]byte("x"))
	_ = held.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.ReadFull(held, make([]byte, 1)); err != nil {
		t.Fatalf("echo before cut: %v", err)
	}
	if n := s.Cut(); n < 2 {
		t.Fatalf("cut closed %d connections, want client+upstream", n)
	}
	_ = held.SetDeadline(time.Now().Add(time.Second))
	if _, err := held.Read(make([]byte, 8)); err == nil {
		t.Fatal("cut connection still readable")
	}
	if err := roundtrip(); err != nil {
		t.Fatalf("after cut, new connections must work: %v", err)
	}
	if !strings.Contains(logBuf.String(), "fault=drop") {
		t.Fatalf("log:\n%s", logBuf.String())
	}
}

func TestRecordAndAuthz(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/private") && r.Header.Get("Authorization") == "" {
			w.WriteHeader(401)
			return
		}
		_, _ = io.WriteString(w, "payload for "+r.URL.Path)
	}))
	defer up.Close()
	rec := filepath.Join(t.TempDir(), "t.rec.jsonl")
	s, _ := startHTTP(t, up.URL, rec)
	base := "http://" + s.Addr().String()
	send := func(method, path string) {
		req, _ := http.NewRequest(method, base+path, strings.NewReader(`{"k":1}`))
		req.Header.Set("Authorization", "Bearer test-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	send("GET", "/before") // recording off: not stored
	s.SetRecording(true)
	send("GET", "/public/list")
	send("GET", "/private/me")
	send("POST", "/private/orders")
	s.SetRecording(false)
	send("GET", "/after")
	recs, err := ReadRecords(rec)
	if err != nil || len(recs) != 3 || s.Status().Counters.Recorded != 3 {
		t.Fatalf("records = %d (%v), counter %d", len(recs), err, s.Status().Counters.Recorded)
	}
	if recs[2].Method != "POST" || string(recs[2].Body) != `{"k":1}` || recs[0].Status != 200 || recs[0].Headers.Get("Authorization") == "" {
		t.Fatalf("record shape: %+v", recs[2])
	}

	id, err := ParseIdentity("none")
	if err != nil {
		t.Fatal(err)
	}
	rep, err := Replay(up.URL, recs, AuthzOptions{Identity: id, Expect: []int{401, 403, 404}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Checked != 2 || len(rep.Candidates) != 1 || len(rep.Skipped) != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if c := rep.Candidates[0]; c.Path != "/public/list" || c.Replayed != 200 || c.Verdict != "same payload" {
		t.Fatalf("candidate = %+v", c)
	}
	if rep.Skipped[0].Method != "POST" || !strings.Contains(rep.Skipped[0].Reason, "mutation") {
		t.Fatalf("skipped = %+v", rep.Skipped[0])
	}
	res, err := AuthzResult("t", rep)
	if !hasExit(err, 2) || res.Diagnostics[0].Code != DiagAuthzLeakCandidates || res.Next[0] != "blip t record clear" {
		t.Fatalf("AuthzResult: %v %+v", err, res)
	}
	rep, err = Replay(up.URL, recs, AuthzOptions{Identity: id, Expect: []int{401}, Only: "/private*", Mutations: true})
	if err != nil || rep.Checked != 2 || len(rep.Candidates) != 0 {
		t.Fatalf("--only/--mutations: %+v %v", rep, err)
	}
	if err := s.ClearRecording(); err != nil || s.Status().Counters.Recorded != 0 {
		t.Fatalf("clear: %v", err)
	}
	if _, err := ParseIdentity("token:abc"); !hasCode(err, DiagIdentityInvalid) {
		t.Fatalf("bad identity accepted: %v", err)
	}
}

func TestControlSocket(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s, _ := startHTTP(t, up.URL, "")
	sock := filepath.Join(t.TempDir(), "t.sock")
	ctl, err := ServeControl(s, sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ctl.ln.Close()
	c := dial(sock)
	st, _, err := c.call("POST", "/set", &Fault{Kind: "delay", Delay: time.Second})
	if err != nil || st.Fault == nil || st.Fault.Kind != "delay" {
		t.Fatalf("set over socket: %+v %v", st, err)
	}
	if st, err = c.status(); err != nil || st.Fault.Delay != time.Second {
		t.Fatalf("status over socket: %+v %v", st, err)
	}
	if st, _, err = c.call("POST", "/ok", nil); err != nil || st.Fault != nil {
		t.Fatalf("ok over socket: %+v %v", st, err)
	}
}

func hasCode(err error, code string) bool {
	var d runx.DiagError
	return errors.As(err, &d) && d.Diag.Code == code
}

func hasExit(err error, code int) bool {
	var e runx.ExitCoder
	return errors.As(err, &e) && e.ExitCode() == code
}
