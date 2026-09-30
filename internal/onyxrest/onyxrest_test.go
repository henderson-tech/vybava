package onyxrest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type seen struct {
	method, path, auth, contentType, body string
}

func server(t *testing.T, status int, reply string) (*httptest.Server, *seen) {
	t.Helper()
	got := &seen{}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*got = seen{r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(body)}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func call(t *testing.T, srv *httptest.Server, args ...string) (int, string, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out.json")
	req, err := Parse(append([]string{"--base", srv.URL + "/"}, append(args, "--out", out)...))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	status, err := Do(context.Background(), srv.Client(), req, "s3cret")
	return status, out, err
}

func readOut(t *testing.T, path string) Response {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestBearerByDefault(t *testing.T) {
	srv, got := server(t, 200, `{"ok":true}`)
	if _, _, err := call(t, srv, "GET", "/rest/api/3/myself"); err != nil {
		t.Fatal(err)
	}
	if got.auth != "Bearer s3cret" || got.method != "GET" || got.path != "/rest/api/3/myself" {
		t.Fatalf("got %+v", got)
	}
}

func TestBasicUserComposesUserAndToken(t *testing.T) {
	srv, got := server(t, 201, `{"id":"1"}`)
	if _, _, err := call(t, srv, "--basic-user", "me@example.cz", "post", "/rest/api/3/issue/PFF-1/worklog", "--data", `{"a":1}`); err != nil {
		t.Fatal(err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("me@example.cz:s3cret"))
	if got.auth != want || got.method != "POST" || got.contentType != "application/json" || got.body != `{"a":1}` {
		t.Fatalf("got %+v", got)
	}
}

func TestOutFileShapeAndMode(t *testing.T) {
	srv, _ := server(t, 404, `{"errorMessages":["gone"]}`)
	status, out, err := call(t, srv, "GET", "/x")
	if err != nil || status != 404 {
		t.Fatalf("status=%d err=%v", status, err)
	}
	info, _ := os.Stat(out)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode().Perm())
	}
	if r := readOut(t, out); r.Status != 404 || r.Body.(map[string]any)["errorMessages"] == nil {
		t.Fatalf("out %+v", r)
	}

	srv, _ = server(t, 200, "plain")
	if _, out, _ = call(t, srv, "GET", "/x"); readOut(t, out).Body != "plain" {
		t.Fatal("a non-JSON body must come back as a string")
	}
	for _, binary := range []string{"\x89PNG\r\n\x1a\n\x00\xff", "\"\xff\""} {
		srv, _ = server(t, 200, binary)
		if _, out, _ = call(t, srv, "GET", "/x"); readOut(t, out).BodyBase64 != base64.StdEncoding.EncodeToString([]byte(binary)) {
			t.Fatalf("non-UTF-8 body %q must come back byte-exact in body_base64", binary)
		}
	}
	srv, _ = server(t, 204, "")
	if _, out, _ = call(t, srv, "DELETE", "/x"); readOut(t, out).Body != nil {
		t.Fatal("an empty body must come back as null")
	}
}

func TestBaseIsPinned(t *testing.T) {
	for _, args := range [][]string{
		{"GET", "/x", "--base", "https://a.test/", "--out", "o"},
		{"--out", "o", "--base", "https://a.test/", "GET", "/x"},
		{"--base", "https://a.test/", "GET", "/x", "--base=https://evil.test", "--out", "o"},
		{"--base", "https://a.test/", "--base", "https://evil.test/", "GET", "/x", "--out", "o"},
		{"--base", "http://a.test/", "GET", "/x", "--out", "o"},
		{"--base", "https://a.test/.evil", "GET", "/x", "--out", "o"},
		{"--base", "https://u@evil.test/", "GET", "/x", "--out", "o"},
	} {
		if _, err := Parse(args); !errors.Is(err, ErrUsage) {
			t.Errorf("%v: want a usage refusal, got %v", args, err)
		}
	}
}

func TestPathCannotLeaveTheBase(t *testing.T) {
	for _, path := range []string{"//evil.test/x", "https://evil.test/x", "rest/api", "/x/https://evil.test", `/\evil.test`, ""} {
		if _, err := Parse([]string{"--base", "https://a.test/", "GET", path, "--out", "o"}); !errors.Is(err, ErrUsage) {
			t.Errorf("%q: want a usage refusal, got %v", path, err)
		}
	}
	req, err := Parse([]string{"--base", "https://a.test", "GET", "/rest/api/3/search?jql=a%3Db", "--out", "o"})
	if err != nil || req.URL.String() != "https://a.test/rest/api/3/search?jql=a%3Db" {
		t.Fatalf("url=%v err=%v", req.URL, err)
	}
}

func TestMisuseIsRefused(t *testing.T) {
	for _, args := range [][]string{
		{"--base", "https://a.test/", "GET", "/x"},
		{"--base", "https://a.test/", "TRACE", "/x", "--out", "o"},
		{"--base", "https://a.test/", "GET", "/x", "--out", "o", "--out", "p"},
		{"--base", "https://a.test/", "GET", "/x", "--data", "{nope", "--out", "o"},
		{"--base", "https://a.test/", "GET", "/x", "--verbose", "--out", "o"},
	} {
		if _, err := Parse(args); !errors.Is(err, ErrUsage) || !strings.Contains(err.Error(), "usage: onyx-rest") {
			t.Errorf("%v: got %v", args, err)
		}
	}
}

func TestMissingTokenAndTransportFailureWriteNothing(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out.json")
	req, _ := Parse([]string{"--base", "https://127.0.0.1:1/", "GET", "/x", "--out", out})
	if _, err := Do(context.Background(), http.DefaultClient, req, ""); !errors.Is(err, ErrUsage) {
		t.Fatalf("missing token: %v", err)
	}
	if _, err := Do(context.Background(), http.DefaultClient, req, "t"); err == nil || strings.Contains(err.Error(), "t@") {
		t.Fatalf("transport: %v", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("a failed call must not write --out")
	}
}
