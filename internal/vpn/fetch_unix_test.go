//go:build !windows

package vpn

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"syscall"
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

// Deliver's FIFO argument comes from whoever asked Onyx to run `_apply`: it
// opens nothing but a real FIFO with a reader, and never blocks on one.
func TestDeliverOpensOnlyAFIFOItsReaderHolds(t *testing.T) {
	dir := t.TempDir()
	if err := Save(dir, "admin", Profile{Ref: "onyx://WireGuard/x/Configuration", Probes: []string{"10.8.1.1:443"}}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(SecretEnv, fixture())
	grace := pipeGrace
	pipeGrace = 100 * time.Millisecond
	t.Cleanup(func() { pipeGrace = grace })
	deliver := func(path string) error {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- Deliver(dir, "admin", path) }()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatalf("Deliver blocked on %s", path)
			return nil
		}
	}

	lonely := filepath.Join(dir, "lonely.fifo")
	if err := syscall.Mkfifo(lonely, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := deliver(lonely); err == nil {
		t.Fatal("a pipe nobody reads must fail, not swallow the profile")
	}

	fifo, link := filepath.Join(dir, "real.fifo"), filepath.Join(dir, "link.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(fifo, link); err != nil {
		t.Fatal(err)
	}
	got := make(chan []byte, 1)
	go func() {
		f, err := os.Open(fifo)
		if err != nil {
			got <- nil
			return
		}
		defer f.Close()
		b, _ := io.ReadAll(f)
		got <- b
	}()
	if err := deliver(link); err == nil {
		t.Fatal("a symlink must be refused, even to a FIFO")
	}
	// Release the reader with an empty writer, retried: it may not be in its
	// open yet (the same dance Fetch does).
	var b []byte
	for waiting := true; waiting; {
		if fd, err := syscall.Open(fifo, syscall.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			syscall.Close(fd)
		}
		select {
		case b = <-got:
			waiting = false
		case <-time.After(20 * time.Millisecond):
		}
	}
	if len(b) != 0 {
		t.Fatal("the profile went through a symlink")
	}
}
