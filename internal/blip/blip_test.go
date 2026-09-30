package blip

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

func startHTTP(t *testing.T, upstream string, recPath string) (*Server, *syncBuffer) {
	t.Helper()
	var logBuf syncBuffer
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
	var logBuf syncBuffer
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
	if recs[2].Method != "POST" || string(recs[2].Body) != `{"k":1}` || recs[0].Status != 200 {
		t.Fatalf("record shape: %+v", recs[2])
	}
	if recs[0].Headers.Get("Authorization") != "" || len(recs[0].CredentialHeaders) != 1 || recs[0].CredentialHeaders[0] != "Authorization" {
		t.Fatalf("credential value stored or name missing: %+v", recs[0])
	}
	if st := s.Status(); len(st.CredentialHeaders) != 1 || st.CredentialHeaders[0] != "Authorization" {
		t.Fatalf("status credential names = %v", st.CredentialHeaders)
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

// TestReplayCarriesOnlyTheIdentity: a recorded flow that used BOTH an
// Authorization header and a Cookie is replayed with exactly the --as
// credential — never the other original one — and the recording on disk
// holds no credential value at all.
func TestReplayCarriesOnlyTheIdentity(t *testing.T) {
	var seen []http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Clone())
		_, _ = io.WriteString(w, "ok")
	}))
	defer up.Close()
	rec := filepath.Join(t.TempDir(), "both.rec.jsonl")
	s, _ := startHTTP(t, up.URL, rec)
	s.SetRecording(true)
	req, _ := http.NewRequest("GET", "http://"+s.Addr().String()+"/account?tab=billing", nil)
	req.Header.Set("Authorization", "Bearer original-secret")
	req.Header.Set("Cookie", "sid=original-cookie; theme=dark")
	req.Header.Set("X-Api-Key", "original-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	raw, _ := os.ReadFile(rec)
	for _, secret := range []string{"original-secret", "original-cookie", "original-key"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("recording stores a credential value: %s", raw)
		}
	}
	if !bytes.Contains(raw, []byte(`"query":"tab=billing"`)) || !bytes.Contains(raw, []byte(`"credential_headers":["Authorization","Cookie","X-Api-Key"]`)) {
		t.Fatalf("record must keep the full URL and the credential NAMES: %s", raw)
	}
	recs, err := ReadRecords(rec)
	if err != nil || len(recs) != 1 {
		t.Fatalf("records: %d %v", len(recs), err)
	}
	seen = nil
	hdr, _ := ParseIdentity("header:Authorization=Bearer other-user")
	if _, err := Replay(up.URL, recs, AuthzOptions{Identity: hdr, Expect: []int{401}}); err != nil {
		t.Fatal(err)
	}
	ck, _ := ParseIdentity("cookie:sid=other-session")
	if _, err := Replay(up.URL, recs, AuthzOptions{Identity: ck, Expect: []int{401}}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 {
		t.Fatalf("upstream saw %d replays", len(seen))
	}
	if h := seen[0]; h.Get("Authorization") != "Bearer other-user" || h.Get("Cookie") != "" || h.Get("X-Api-Key") != "" {
		t.Fatalf("header replay leaked another credential: %v", h)
	}
	if h := seen[1]; h.Get("Cookie") != "sid=other-session" || h.Get("Authorization") != "" || h.Get("X-Api-Key") != "" {
		t.Fatalf("cookie replay leaked another credential: %v", h)
	}
	if _, err := ParseIdentity("token:super-secret"); err == nil || strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("bad --as must be refused without echoing the value: %v", err)
	}
}

// TestQueryCredentialsNeverStoredOrReplayed: a token read via
// ?access_token= keeps only the non-credential keys on disk, lists the key
// name, and replays without it.
func TestQueryCredentialsNeverStoredOrReplayed(t *testing.T) {
	var targets []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targets = append(targets, r.URL.RequestURI())
		_, _ = io.WriteString(w, "file")
	}))
	defer up.Close()
	rec := filepath.Join(t.TempDir(), "q.rec.jsonl")
	s, _ := startHTTP(t, up.URL, rec)
	s.SetRecording(true)
	if _, _, err := get(t, "http://"+s.Addr().String()+"/files?id=1&access_token=secret&X-Amz-Signature=sigsecret&X-Amz-Security-Token=tokensecret&X-Amz-Credential=AKIAsecret"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(rec)
	if bytes.Contains(raw, []byte("secret")) || !bytes.Contains(raw, []byte(`"query":"id=1"`)) || !bytes.Contains(raw, []byte(`"credential_query":["access_token","x-amz-signature","x-amz-security-token","x-amz-credential"]`)) {
		t.Fatalf("recording: %s", raw)
	}
	if st := s.Status(); len(st.CredentialQuery) != 4 || st.CredentialQuery[0] != "access_token" {
		t.Fatalf("status credential query = %v", st.CredentialQuery)
	}
	recs, _ := ReadRecords(rec)
	recs[0].Query = "id=1&token=handedited" // an older/edited recording must still be sanitized
	id, _ := ParseIdentity("none")
	targets = nil
	if _, err := Replay(up.URL, recs, AuthzOptions{Identity: id, Expect: []int{401}}); err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0] != "/files?id=1" {
		t.Fatalf("replay target = %v", targets)
	}
}

// TestTimeoutForReleasesWithoutTraffic: `set timeout --for 200ms` must
// answer the held request when the timer fires, with no other call.
func TestTimeoutForReleasesWithoutTraffic(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	s, _ := startHTTP(t, up.URL, "")
	s.SetFault(&Fault{Kind: "timeout", For: 200 * time.Millisecond})
	start := time.Now()
	resp, _, err := get(t, "http://"+s.Addr().String()+"/held")
	if err != nil || resp.StatusCode != 503 {
		t.Fatalf("held request: %v %v", resp, err)
	}
	if d := time.Since(start); d < 150*time.Millisecond || d > 1500*time.Millisecond {
		t.Fatalf("released after %s, want ~200ms", d)
	}
	if s.fault.Load() != nil {
		t.Fatal("fault still set after --for")
	}
}

// TestBodyRedaction: credential values in JSON and form bodies are never
// stored, the key names are, and such records are skipped by Replay.
func TestBodyRedaction(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	rec := filepath.Join(t.TempDir(), "b.rec.jsonl")
	s, _ := startHTTP(t, up.URL, rec)
	s.SetRecording(true)
	base := "http://" + s.Addr().String()
	post := func(path, ct, body string) {
		resp, err := http.Post(base+path, ct, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	post("/login", "application/json; charset=utf-8", `{"user":"lukas","Password":"hunter2","n":7,"credentials":{"password":"deep"},"users":[{"id":1},{"id":2,"token":"arr-secret"}]}`)
	post("/oauth/token", "application/x-www-form-urlencoded", "grant_type=client_credentials&client_id=app&client_secret=s3cret")
	post("/notes", "text/plain", "password=not-a-form")
	post("/broken", "application/json", `{"password":"unterminated`)
	huge := `{"pad":"` + strings.Repeat("x", recordBodyLimit) + `","password":"oversize-secret"}`
	post("/big", "application/json", huge)
	post("/trailing", "application/json", `{"ok":1} {"password":"trailing-secret"}`)
	post("/badform", "application/x-www-form-urlencoded", "user=abc%GG&password=escape-secret")
	raw, _ := os.ReadFile(rec)
	for _, secret := range []string{"hunter2", "s3cret", "deep", "arr-secret", "unterminated", "oversize-secret", "trailing-secret", "escape-secret", "abc%GG"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("credential value %q stored:\n%.300s", secret, raw)
		}
	}
	recs, _ := ReadRecords(rec)
	if len(recs) != 7 {
		t.Fatalf("records = %d", len(recs))
	}
	for _, i := range []int{5, 6} {
		if !recs[i].BodyUnparsed || recs[i].Body != nil {
			t.Fatalf("%s: trailing JSON / bad form escape must be unparsed with no bytes: %+v", recs[i].Path, recs[i])
		}
	}
	if got := recs[0].BodyRedacted; strings.Join(got, ",") != "Password,credentials.password,users[1].token" ||
		!bytes.Contains(recs[0].Body, []byte(`"user":"lukas"`)) || !bytes.Contains(recs[0].Body, []byte(`"n":7`)) || !bytes.Contains(recs[0].Body, []byte(`{"password":"[redacted]"}`)) {
		t.Fatalf("json redaction (any depth, other keys kept): %v %s", got, recs[0].Body)
	}
	// Forms are stored as url.Values.Encode() (sorted keys), never the raw bytes.
	if got := recs[1].BodyRedacted; len(got) != 1 || got[0] != "client_secret" || string(recs[1].Body) != "client_id=app&client_secret=%5Bredacted%5D&grant_type=client_credentials" {
		t.Fatalf("form redaction: %v %s", got, recs[1].Body)
	}
	if recs[2].BodyRedacted != nil || string(recs[2].Body) != "password=not-a-form" {
		t.Fatalf("text/plain must be stored as-is: %+v", recs[2])
	}
	if !recs[3].BodyUnparsed || recs[3].Body != nil {
		t.Fatalf("unparseable JSON must store no body: %+v", recs[3])
	}
	if !recs[4].BodyTruncated || recs[4].Body != nil {
		t.Fatalf("truncated body must store no bytes: truncated=%v len=%d", recs[4].BodyTruncated, len(recs[4].Body))
	}
	id, _ := ParseIdentity("none")
	rep, err := Replay(up.URL, recs, AuthzOptions{Identity: id, Expect: []int{401}, Mutations: true})
	if err != nil || rep.Checked != 1 || len(rep.Skipped) != 6 || rep.Skipped[0].Reason != "body carried credentials (redacted)" || !strings.HasPrefix(rep.Skipped[2].Reason, "body not stored") {
		t.Fatalf("replay must skip redacted/unstored bodies: %+v %v", rep, err)
	}
}

// TestBodySkipDoesNotConsumeURL: an unparsed recording followed by a valid
// one of the same method+URL still yields exactly one replay.
func TestBodySkipDoesNotConsumeURL(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++ }))
	defer up.Close()
	rec := filepath.Join(t.TempDir(), "s.rec.jsonl")
	s, _ := startHTTP(t, up.URL, rec)
	s.SetRecording(true)
	base := "http://" + s.Addr().String()
	for _, body := range []string{`{"q":"broken`, `{"q":"fine"}`, `{"q":"fine"}`} {
		resp, err := http.Post(base+"/search", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	recs, _ := ReadRecords(rec)
	if len(recs) != 3 || !recs[0].BodyUnparsed || recs[1].BodyUnparsed {
		t.Fatalf("records: %+v", recs)
	}
	hits = 0
	id, _ := ParseIdentity("none")
	rep, err := Replay(up.URL, recs, AuthzOptions{Identity: id, Expect: []int{401}, Mutations: true})
	if err != nil || rep.Checked != 1 || hits != 1 || len(rep.Skipped) != 1 {
		t.Fatalf("checked=%d hits=%d skipped=%d %v", rep.Checked, hits, len(rep.Skipped), err)
	}
}

// TestReplayDedupesOnWirePath: /files/a%2Fb and /files/a/b decode to the
// same Path but are different requests on the wire — both must be replayed.
func TestReplayDedupesOnWirePath(t *testing.T) {
	var targets []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targets = append(targets, r.URL.RequestURI())
	}))
	defer up.Close()
	rec := filepath.Join(t.TempDir(), "d.rec.jsonl")
	s, _ := startHTTP(t, up.URL, rec)
	s.SetRecording(true)
	for _, p := range []string{"/files/a%2Fb", "/files/a/b", "/files/a%2Fb"} {
		if _, _, err := get(t, "http://"+s.Addr().String()+p); err != nil {
			t.Fatal(err)
		}
	}
	recs, _ := ReadRecords(rec)
	targets = nil
	id, _ := ParseIdentity("none")
	rep, err := Replay(up.URL, recs, AuthzOptions{Identity: id, Expect: []int{401}})
	if err != nil || rep.Checked != 2 {
		t.Fatalf("checked = %d %v", rep.Checked, err)
	}
	if strings.Join(targets, " ") != "/files/a%2Fb /files/a/b" {
		t.Fatalf("replayed %v", targets)
	}
}

// TestReplayKeepsEscapedPath: /files/a%3Fb must not be rebuilt as /files/a?b.
func TestReplayKeepsEscapedPath(t *testing.T) {
	var targets []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targets = append(targets, r.URL.RequestURI())
	}))
	defer up.Close()
	rec := filepath.Join(t.TempDir(), "p.rec.jsonl")
	s, _ := startHTTP(t, up.URL, rec)
	s.SetRecording(true)
	if _, _, err := get(t, "http://"+s.Addr().String()+"/files/a%3Fb?id=1"); err != nil {
		t.Fatal(err)
	}
	recs, _ := ReadRecords(rec)
	if len(recs) != 1 || recs[0].Path != "/files/a?b" || recs[0].RawPath != "/files/a%3Fb" {
		t.Fatalf("record paths: %+v", recs)
	}
	targets = nil
	id, _ := ParseIdentity("none")
	if _, err := Replay(up.URL, recs, AuthzOptions{Identity: id, Expect: []int{401}, Only: "/files/*"}); err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0] != "/files/a%3Fb?id=1" {
		t.Fatalf("replay target = %v", targets)
	}
}

// TestSlowAbortsWhenClientLeaves: a throttled response to a client that
// hangs up must not keep the handler (and the upstream copy) alive.
func TestSlowAbortsWhenClientLeaves(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 200*1024)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(big) }))
	defer up.Close()
	s, _ := startHTTP(t, up.URL, "")
	s.SetFault(&Fault{Kind: "slow", Bytes: 1000}) // 200 KiB at 1 KB/s: ~200 s if not aborted
	c, err := net.Dial("tcp", s.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = c.Write([]byte("GET /big HTTP/1.1\r\nHost: x\r\n\r\n"))
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	_, _ = c.Read(make([]byte, 64)) // headers + first chunk arrived: the copy is running
	_ = c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for s.inFlight.Load() != 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if s.inFlight.Load() != 0 {
		t.Fatal("slow handler still running after the client left")
	}
}

// TestTailLines pins the bounded tail read across chunk boundaries.
func TestTailLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.log")
	var b strings.Builder
	for i := 1; i <= 5000; i++ {
		b.WriteString(strings.Repeat("x", 40) + " line " + strconv.Itoa(i) + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, err := tailLines(path, 3)
	if err != nil || len(lines) != 3 || !strings.HasSuffix(lines[0], "line 4998") || !strings.HasSuffix(lines[2], "line 5000") {
		t.Fatalf("tail = %v, %v", lines, err)
	}
	if lines, err := tailLines(path, 10000); err != nil || len(lines) != 5000 || !strings.HasSuffix(lines[0], "line 1") {
		t.Fatalf("whole-file tail = %d lines, %v", len(lines), err)
	}
	if lines, err := tailLines(filepath.Join(t.TempDir(), "missing"), 3); err != nil || lines != nil {
		t.Fatalf("missing file: %v %v", lines, err)
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

// syncBuffer is a bytes.Buffer safe to read while handler goroutines log to it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}
