package vconfig

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitDir answers what `git rev-parse --absolute-git-dir` would — in a main
// checkout, a subdirectory and a linked worktree — without running git.
func TestGitDirMatchesGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	main := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...).Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	run(main, "init", "-q")
	run(main, "commit", "-q", "--allow-empty", "-m", "init")
	sub := filepath.Join(main, "apps", "web")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(t.TempDir(), "wt")
	run(main, "worktree", "add", "-q", linked)
	for _, dir := range []string{main, sub, linked} {
		want, _ := filepath.EvalSymlinks(run(dir, "rev-parse", "--absolute-git-dir"))
		got, _ := filepath.EvalSymlinks(gitDir(dir))
		if got != want {
			t.Errorf("gitDir(%s) = %q, git says %q", dir, got, want)
		}
	}
	// A worktree whose git dir was pruned has none: the cache falls back.
	stale := t.TempDir()
	if err := os.WriteFile(filepath.Join(stale, ".git"), []byte("gitdir: "+filepath.Join(stale, "gone")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if d := gitDir(stale); d != "" {
		t.Errorf("gitDir of a pruned worktree = %q, want none", d)
	}
}

func TestFindAndLoadJSON(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "apps", "web")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, FileJSON), []byte(`{"lok":{"catalogs":{"m":{"style":"english-as-key","files":"l/{locale}.json","locales":["en"]}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(sub)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Root != root {
		t.Fatalf("root %q", cfg.Root)
	}
	var lok map[string]map[string]map[string]any
	if err := cfg.Section("lok", &lok); err != nil || lok["catalogs"]["m"]["style"] != "english-as-key" {
		t.Fatalf("section: %v %+v", err, lok)
	}
	if err := cfg.Section("nope", &lok); err == nil {
		t.Fatal("missing section must error")
	}
	if _, err := Load(t.TempDir()); err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// A worktree nested inside its main checkout (`.worktrees/<name>`) on a
// branch that predates the config must not borrow the main checkout's: its
// Root would aim merge-assist and lok at the wrong tree.
func TestFindStopsAtTheWorktreeRoot(t *testing.T) {
	main := t.TempDir()
	if err := os.WriteFile(filepath.Join(main, FileJSON), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(main, ".worktrees", "old-branch")
	sub := filepath.Join(wt, "apps", "web")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(main, ".git", "worktrees", "old-branch")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Find(sub); err != ErrNotFound {
		t.Fatalf("Find from inside a config-less worktree: want ErrNotFound, got %v", err)
	}
}

func TestLoadTSViaBun(t *testing.T) {
	if _, err := exec.LookPath("bun"); err != nil {
		t.Skip("bun not installed")
	}
	root := t.TempDir()
	if _, err := WriteHelpers(root, false); err != nil {
		t.Fatal(err)
	}
	src := `import { defineConfig, englishAsKey } from './.vybava/config';
export default defineConfig({ lok: { catalogs: { mobile: englishAsKey('apps/client/locales/{locale}.json', ['en', 'cs'], { required: ['en', 'cs'] }) } } });
`
	if err := os.WriteFile(filepath.Join(root, FileTS), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	var lok struct {
		Catalogs map[string]struct {
			Style    string   `json:"style"`
			Files    string   `json:"files"`
			Locales  []string `json:"locales"`
			Required []string `json:"required"`
		} `json:"catalogs"`
	}
	if err := cfg.Section("lok", &lok); err != nil {
		t.Fatal(err)
	}
	if got := lok.Catalogs["mobile"]; got.Style != "english-as-key" || len(got.Locales) != 2 || got.Required[1] != "cs" {
		t.Fatalf("unexpected %+v", got)
	}
	// second load is served from cache — no bun on PATH needed
	t.Setenv("PATH", "")
	if _, err := Load(root); err != nil {
		t.Fatalf("cached load failed: %v", err)
	}
	if err := CheckHelpers(root); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(HelperPath(root), []byte("// drift"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckHelpers(root); err != ErrHelperDrift {
		t.Fatalf("want drift, got %v", err)
	}
}
