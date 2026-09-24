//go:build !windows

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
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

// `project` reads the committed store without an index pass — a held lock
// never stops it — prices what it can, names what it cannot, and refuses a
// root it never indexed with exit 2.
func TestTokentimeProjectReadsUnderTheLockAndRefusesUnknownRoots(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	state, claude, repo := filepath.Join(base, "state"), filepath.Join(base, "claude"), filepath.Join(base, "app")
	line := func(id, model string, input int) string {
		return fmt.Sprintf(`{"type":"assistant","sessionId":"s","cwd":%q,"timestamp":"2026-09-23T12:00:00Z","message":{"id":%q,"model":%q,"usage":{"input_tokens":%d,"output_tokens":0}}}`+"\n",
			repo, id, model, input)
	}
	for _, dir := range []string{repo, filepath.Join(claude, "-app")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(claude, "-app", "s.jsonl"), []byte(line("m1", "claude-opus-5-5", 1_000_000)+line("m2", "house-model", 7)), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) (map[string]any, error) {
		t.Helper()
		var out bytes.Buffer
		cmd, err := (App{Stdout: &out, Stderr: &out}).Command("tokentime")
		if err != nil {
			t.Fatal(err)
		}
		cmd.SetArgs(append(args, "--json", "--state-dir", state, "--claude-root", claude, "--codex-dir", filepath.Join(base, "codex")))
		err = cmd.Execute()
		var env map[string]any
		if jsonErr := json.Unmarshal(out.Bytes(), &env); jsonErr != nil {
			t.Fatalf("%v: not an envelope: %s", args, out.String())
		}
		return env, err
	}
	diag := func(env map[string]any, code string) bool {
		for _, d := range env["diagnostics"].([]any) {
			if d.(map[string]any)["code"] == code {
				return true
			}
		}
		return false
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

	env, err := run("project", "--root", repo, "--from", "2026-09-22", "--to", "2026-09-24")
	data, _ := env["data"].(map[string]any)
	if err != nil || data["usd"] != 4.0 || data["bucket"] != "day" || len(data["series"].([]any)) != 3 || !diag(env, diagUnpricedModel) {
		t.Fatalf("project = %v, %v; want $4 from the priced model alone, 3 daily entries and %s", env, err, diagUnpricedModel)
	}
	env, err = run("project", "--root", filepath.Join(base, "nowhere"), "--from", "2026-09-23", "--to", "2026-09-23")
	var exit runx.ExitCoder
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || env["data"] != nil || !diag(env, diagUnknownProj) {
		t.Fatalf("unknown root = %v, %v; want exit 2, no data and %s", env, err, diagUnknownProj)
	}
}
