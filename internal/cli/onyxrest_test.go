package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// TestOnyxRestExitCodes pins the exit contract callers branch on: 0 on 2xx,
// 1 on any other HTTP status, 2 on a refused invocation.
func TestOnyxRestExitCodes(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	onyxRestClient = srv.Client()
	defer func() { onyxRestClient = &http.Client{} }()
	t.Setenv("ONYX_REST_TOKEN", "t")
	out := filepath.Join(t.TempDir(), "out.json")

	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"--base", srv.URL + "/", "GET", "/ok", "--out", out}, 0},
		{[]string{"--base", srv.URL + "/", "GET", "/missing", "--out", out}, 1},
		{[]string{"GET", "/ok", "--base", srv.URL + "/", "--out", out}, 2},
	} {
		var buf bytes.Buffer
		command, err := (App{Stdout: &buf, Stderr: &buf}).Command("onyx-rest")
		if err != nil {
			t.Fatal(err)
		}
		command.SetArgs(tc.args)
		if got := ExitCode(command.Execute()); got != tc.want {
			t.Errorf("%v: exit %d, want %d (%s)", tc.args, got, tc.want, buf.String())
		}
	}
}
