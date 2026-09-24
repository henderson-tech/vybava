//go:build !windows

package cli

import (
	"bytes"
	"database/sql"
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
// `rollup` block, fail or migrate: it skips its pass and serves the store it
// has — or, when that store's schema is too old for its queries, says
// STALE_SCHEMA with `tokentime index` next. The first rollup the lock lets
// through migrates it.
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

	// schema runs ddl on the store as an older binary would and reads its version.
	schema := func(ddl string) int {
		t.Helper()
		db, err := sql.Open("sqlite", filepath.Join(state, "tokentime.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if ddl != "" {
			if _, err := db.Exec(ddl); err != nil {
				t.Fatal(err)
			}
		}
		var v int
		if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	schema("ALTER TABLE files DROP COLUMN tail; PRAGMA user_version=1")
	env, took = run("rollup", "--days", "1", "--hours", "1")
	stale := false
	for _, d := range env["diagnostics"].([]any) {
		stale = stale || d.(map[string]any)["code"] == diagStaleSchema
	}
	if env["ok"] != false || !stale || fmt.Sprint(env["next"]) != "[tokentime index]" || took > 5*time.Second {
		t.Fatalf("rollup of a schema 1 store under a held lock = %v after %v; want %s, next tokentime index, promptly", env, took, diagStaleSchema)
	}
	if v := schema(""); v != 1 {
		t.Fatalf("schema under a held lock is now %d: migrated without the lock", v)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if env, _ = run("rollup", "--days", "1", "--hours", "1"); env["ok"] != true {
		t.Fatalf("rollup with the lock free = %v", env)
	}
	if v := schema(""); v != 3 {
		t.Fatalf("schema after a rollup with the lock free = %d, want 3", v)
	}
}

// `project` reads the committed store without an index pass — a held lock
// never stops it — prices what it can, names what it cannot, addresses the
// cwd-less "unknown" project as --root "", and refuses a root it never
// indexed with exit 2.
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
	noCwd := `{"type":"assistant","sessionId":"s","timestamp":"2026-09-23T13:00:00Z","message":{"id":"m3","model":"claude-opus-5-5","usage":{"input_tokens":5,"output_tokens":0}}}` + "\n"
	if err := os.WriteFile(filepath.Join(claude, "-app", "s.jsonl"), []byte(line("m1", "claude-opus-5-5", 1_000_000)+line("m2", "house-model", 7)+noCwd), 0o644); err != nil {
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
	// The rollup's "unknown" row — responses without a cwd — expands like any other.
	env, err = run("project", "--root", "", "--from", "2026-09-23", "--to", "2026-09-23")
	data, _ = env["data"].(map[string]any)
	if err != nil || data["name"] != "unknown" || data["root"] != "" || data["responses"] != 1.0 {
		t.Fatalf(`--root "" = %v, %v; want the unknown project and its one response`, env, err)
	}
	env, err = run("project", "--root", filepath.Join(base, "nowhere"), "--from", "2026-09-23", "--to", "2026-09-23")
	var exit runx.ExitCoder
	if !errors.As(err, &exit) || exit.ExitCode() != 2 || env["data"] != nil || !diag(env, diagUnknownProj) {
		t.Fatalf("unknown root = %v, %v; want exit 2, no data and %s", env, err, diagUnknownProj)
	}
}

// The read-only verb never creates a store: a bad range is BAD_FLAG before
// any store is opened, and a store never indexed is NO_STORE — exit 2, no
// data, no state directory made to say so. A range is bad outside
// 2000-01-01..2100-12-31 and one day or month past its bucket's cap; at the
// cap it reaches the store.
func TestTokentimeProjectNeverCreatesAStore(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	state := filepath.Join(base, "state")
	for _, c := range []struct{ from, to, bucket, code string }{
		{"2026-13-01", "2026-09-24", "", diagBadFlag},
		{"2026-09-24", "2026-09-24", "", diagNoStore},
		{"0001-01-01", "9999-12-31", "hour", diagBadFlag},
		{"1999-12-31", "2000-01-01", "", diagBadFlag},
		{"2100-12-31", "2101-01-01", "", diagBadFlag},
		{"2026-01-01", "2026-01-31", "hour", diagNoStore}, // 31 days
		{"2026-01-01", "2026-02-01", "hour", diagBadFlag},
		{"2024-01-01", "2027-01-04", "day", diagNoStore}, // 1100 days
		{"2024-01-01", "2027-01-05", "day", diagBadFlag},
		{"2001-01-01", "2100-12-31", "month", diagNoStore}, // 1200 months
		{"2000-12-01", "2100-12-31", "month", diagBadFlag},
	} {
		var out bytes.Buffer
		cmd, err := (App{Stdout: &out, Stderr: &out}).Command("tokentime")
		if err != nil {
			t.Fatal(err)
		}
		cmd.SetArgs([]string{"project", "--root", base, "--from", c.from, "--to", c.to, "--bucket", c.bucket, "--json", "--state-dir", state})
		err = cmd.Execute()
		var exit runx.ExitCoder
		var env map[string]any
		if jsonErr := json.Unmarshal(out.Bytes(), &env); jsonErr != nil || !errors.As(err, &exit) || exit.ExitCode() != 2 ||
			env["data"] != nil || !bytes.Contains(out.Bytes(), []byte(c.code)) {
			t.Fatalf("%s..%s by %q = %s, %v; want exit 2, no data and %s", c.from, c.to, c.bucket, out.String(), err, c.code)
		}
		if _, statErr := os.Stat(state); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("state dir after %s..%s: %v, want it never created", c.from, c.to, statErr)
		}
	}
}
