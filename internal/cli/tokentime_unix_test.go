//go:build !windows

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// An orphaned or concurrent tokentime holding the index lock must never make
// `rollup` block or fail: it skips its pass and serves the store it has.
func TestTokentimeRollupServesTheStoreWhileTheIndexIsLocked(t *testing.T) {
	base := t.TempDir()
	state := filepath.Join(base, "state")
	run := func(args ...string) (map[string]any, time.Duration) {
		t.Helper()
		var out bytes.Buffer
		cmd, err := (App{Stdout: &out, Stderr: &out}).Command("tokentime")
		if err != nil {
			t.Fatal(err)
		}
		cmd.SetArgs(append(args, "--json", "--state-dir", state,
			"--claude-root", filepath.Join(base, "claude"), "--codex-dir", filepath.Join(base, "codex")))
		started := time.Now()
		_ = cmd.Execute()
		var env map[string]any
		if err := json.Unmarshal(out.Bytes(), &env); err != nil {
			t.Fatalf("%v: not an envelope: %s", args, out.String())
		}
		return env, time.Since(started)
	}
	if env, _ := run("index"); env["ok"] != true {
		t.Fatalf("seeding index = %v", env)
	}

	lock, err := os.OpenFile(filepath.Join(state, "index.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	env, took := run("rollup", "--days", "1", "--hours", "1")
	if env["ok"] != true || took > 5*time.Second {
		t.Fatalf("rollup under a held lock = %v after %v; want ok, promptly", env, took)
	}
	data, _ := env["data"].(map[string]any)
	if _, ok := data["coverage"]; !ok {
		t.Fatalf("rollup served no coverage from the stored state: %v", env)
	}
	busy := false
	for _, d := range env["diagnostics"].([]any) {
		busy = busy || d.(map[string]any)["code"] == diagIndexBusy
	}
	if !busy {
		t.Fatalf("diagnostics = %v, want %s", env["diagnostics"], diagIndexBusy)
	}
}
