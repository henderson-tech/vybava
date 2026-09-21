package memo

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestEnsureGitignoreTeamHome pins the 2026-09-21 decision: a team home in a
// git work tree gitignores MEMORY.md and usage.jsonl (created, merged into an
// existing file, idempotent), a personal home and a team home outside git
// never get one.
func TestEnsureGitignoreTeamHome(t *testing.T) {
	root := linkedWorktreeRepo(t)
	home := filepath.Join(root, ".claude", "memory")
	l, err := Create(filepath.Join(home, LedgerFile), "fixit-team", KindTeam, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	if _, err := WriteIndex(l, nil, now); err != nil {
		t.Fatal(err)
	}
	ignore := filepath.Join(home, GitignoreFile)
	if got, _ := os.ReadFile(ignore); string(got) != "MEMORY.md\nusage.jsonl\n" {
		t.Fatalf("created .gitignore = %q", got)
	}
	if changed, err := EnsureGitignore(home, KindTeam); err != nil || changed {
		t.Errorf("second ensure must be a no-op: changed=%v err=%v", changed, err)
	}
	for _, f := range LocalFiles {
		if ignored, known := GitIgnored(filepath.Join(home, f)); !known || !ignored {
			t.Errorf("%s must be gitignored (known=%v ignored=%v)", f, known, ignored)
		}
	}

	// Merge: other lines and an existing entry are kept, only the missing one lands.
	if err := os.WriteFile(ignore, []byte("scratch/\nMEMORY.md\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if changed, err := EnsureGitignore(home, KindTeam); err != nil || !changed {
		t.Fatalf("merge: changed=%v err=%v", changed, err)
	}
	if got, _ := os.ReadFile(ignore); string(got) != "scratch/\nMEMORY.md\nusage.jsonl\n" {
		t.Errorf("merged .gitignore = %q", got)
	}

	// Personal home: never, even inside a work tree.
	personal := filepath.Join(root, ".claude", "projects", "x", "memory")
	pl, err := Create(filepath.Join(personal, LedgerFile), "fixit", KindPersonal, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := WriteIndex(pl, nil, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(personal, GitignoreFile)); !os.IsNotExist(err) {
		t.Errorf("personal home must not get a .gitignore: %v", err)
	}

	// Team home outside any work tree: nothing to keep out of git.
	outside := newHome(t, KindTeam, "- #t1 project/api A. ^t1")
	if _, err := WriteIndex(outside, nil, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside.Home(), GitignoreFile)); !os.IsNotExist(err) {
		t.Errorf("team home outside git must not get a .gitignore: %v", err)
	}
}

// TestEnsureIndexMissingStaleCurrent pins `memo ensure`: render when MEMORY.md
// is missing or older than LEDGER.md / usage.jsonl, do nothing when current,
// and bump the mtime of a stale-but-identical render so the next call is fast.
func TestEnsureIndexMissingStaleCurrent(t *testing.T) {
	l := newHome(t, KindTeam, "- #t1 project/api A. ^t1")
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	index := filepath.Join(l.Home(), IndexFile)
	os.Remove(index)

	res, err := EnsureIndex(l, nil, now)
	if err != nil || res.Reason != "missing" || !res.Rendered {
		t.Fatalf("missing: %+v %v", res, err)
	}
	if _, err := os.Stat(index); err != nil {
		t.Fatalf("MEMORY.md not written: %v", err)
	}
	res, err = EnsureIndex(l, nil, now)
	if err != nil || res.Reason != "current" || res.Rendered {
		t.Fatalf("current: %+v %v", res, err)
	}

	// Ledger newer than the render, content unchanged: stale, not rewritten, mtime bumped.
	later := now.Add(2 * time.Second)
	if err := os.Chtimes(l.Path, later, later); err != nil {
		t.Fatal(err)
	}
	res, err = EnsureIndex(l, nil, later.Add(time.Second))
	if err != nil || res.Reason != "stale" || res.Rendered {
		t.Fatalf("stale unchanged: %+v %v", res, err)
	}
	if info, _ := os.Stat(index); !info.ModTime().After(later.Add(-time.Second)) {
		t.Errorf("stale render must bump the mtime, got %v", info.ModTime())
	}
	res, err = EnsureIndex(l, nil, later.Add(2*time.Second))
	if err != nil || res.Reason != "current" {
		t.Fatalf("current after bump: %+v %v", res, err)
	}

	// A new row lands: stale and actually rendered.
	if err := appendLine(l.Path, "- #t2 project/api B. ^t2"); err != nil {
		t.Fatal(err)
	}
	l2, d, err := Load(l.Path)
	if err != nil || d != nil {
		t.Fatal(d, err)
	}
	future := later.Add(10 * time.Second)
	if err := os.Chtimes(l.Path, future, future); err != nil {
		t.Fatal(err)
	}
	res, err = EnsureIndex(l2, nil, future.Add(time.Second))
	if err != nil || res.Reason != "stale" || !res.Rendered {
		t.Fatalf("stale changed: %+v %v", res, err)
	}

	// A usage event lands (the harvest wrote usage.jsonl after the render):
	// the ranking may have moved, so the surface is stale even though the
	// ledger did not change.
	usageAt := future.Add(20 * time.Second)
	events := []Event{NewEvent(2, "cite", "sess", usageAt)}
	if _, err := AppendEvents(l2.Home(), nil, events); err != nil {
		t.Fatal(err)
	}
	usage := filepath.Join(l2.Home(), UsageFile)
	if err := os.Chtimes(usage, usageAt, usageAt); err != nil {
		t.Fatal(err)
	}
	// One cite on a two-row ledger leaves the order as it was, so the file is
	// not rewritten; what matters is that usage.jsonl alone made it stale.
	res, err = EnsureIndex(l2, events, usageAt.Add(time.Second))
	if err != nil || res.Reason != "stale" {
		t.Fatalf("stale after usage: %+v %v", res, err)
	}
	res, err = EnsureIndex(l2, events, usageAt.Add(2*time.Second))
	if err != nil || res.Reason != "current" {
		t.Fatalf("current after usage render: %+v %v", res, err)
	}
}

// TestHookSessionStartRendersHomesAndNeverBlocks: SessionStart renders every
// session home; a home whose ledger does not parse is reported, never fatal,
// and the other home still renders.
func TestHookSessionStartRendersHomesAndNeverBlocks(t *testing.T) {
	user := t.TempDir()
	repo := t.TempDir()
	env := Env{UserHome: user, Cwd: repo}
	personal, team := env.SessionHomes()
	for _, h := range []Home{personal, team} {
		if _, d, err := env.Open(h, true); err != nil || d != nil {
			t.Fatal(d, err)
		}
	}
	if err := appendLine(filepath.Join(team.Path, LedgerFile), "- #t1 project/api A. ^t1"); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC)
	res, err := env.RunHook(HookPayload{HookEventName: "SessionStart", Cwd: repo, SessionID: "s"}, now)
	if err != nil || len(res.Problems) != 0 || len(res.Homes) != 2 || len(res.Rendered) != 2 {
		t.Fatalf("both homes must render: %+v %v", res, err)
	}
	for _, h := range []Home{personal, team} {
		if _, err := os.Stat(filepath.Join(h.Path, IndexFile)); err != nil {
			t.Errorf("%s: %v", h.Path, err)
		}
	}
	// Break the personal ledger and drop the team render: the team home still
	// comes back, the personal one is a problem line, the hook returns no error.
	if err := appendLine(filepath.Join(personal.Path, LedgerFile), "this is not a row"); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(team.Path, IndexFile))
	res, err = env.RunHook(HookPayload{HookEventName: "SessionStart", Cwd: repo, SessionID: "s"}, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("SessionStart must never fail: %v", err)
	}
	if len(res.Problems) != 1 || len(res.Rendered) != 1 || res.Rendered[0] != filepath.Join(team.Path, IndexFile) {
		t.Errorf("broken personal home must not stop the team render: %+v", res)
	}
}
