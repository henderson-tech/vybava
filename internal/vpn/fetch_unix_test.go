//go:build !windows

package vpn

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeOnyx stands in for Onyx's run_command: it "injects" the profile and
// runs the child in-process, or refuses without ever opening the pipe.
func fakeOnyx(t *testing.T, refuse bool) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req onyxRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		exit := 1
		if argv := req.Params.Arguments.Argv; !refuse && len(argv) == 6 && argv[2] == "_apply" {
			if err := Deliver(argv[4], argv[3], argv[5]); err == nil {
				exit = 0
			}
		}
		text, _ := json.Marshal(map[string]int{"exit": exit})
		fmt.Fprintf(w, `{"result":{"content":[{"type":"text","text":%q}]}}`, text)
	}))
	t.Cleanup(server.Close)
	u, _ := url.Parse(server.URL)
	token := filepath.Join(t.TempDir(), "token")
	os.WriteFile(token, []byte("t"), 0o600)
	t.Setenv("ONYX_MCP_HTTP_TOKEN_FILE", token)
	t.Setenv("ONYX_MCP_HTTP_PORT", u.Port())
	t.Setenv(SecretEnv, fixture())
}

func TestFetchTakesTheProfileThroughThePipeAndNeverHangs(t *testing.T) {
	dir := t.TempDir()
	p := Profile{Ref: "onyx://WireGuard/x/Configuration", Probes: []string{"10.8.1.1:443"}}
	if err := Save(dir, "admin", p); err != nil {
		t.Fatal(err)
	}
	want, _ := Prepare(fixture(), nil)
	for _, refuse := range []bool{false, true} {
		fakeOnyx(t, refuse)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		got, err := Fetch(ctx, "/usr/local/bin/vybava", dir, "admin", p)
		cancel()
		if refuse && err == nil {
			t.Fatal("a refused child must fail the fetch")
		}
		if !refuse && (err != nil || got != want) {
			t.Fatalf("fetch: %v\n%q", err, got)
		}
	}
	plain := filepath.Join(dir, "plain.conf")
	os.WriteFile(plain, nil, 0o600)
	if err := Deliver(dir, "admin", plain); err == nil {
		t.Fatal("the child must refuse a regular file")
	}
	if b, _ := os.ReadFile(plain); len(b) != 0 {
		t.Fatal("the profile reached a regular file")
	}
}
