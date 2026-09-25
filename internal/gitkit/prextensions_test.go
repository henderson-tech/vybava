package gitkit

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runPRExtensionsJSON(t *testing.T, args ...string) PRExtensions {
	t.Helper()
	var stdout, stderr strings.Builder
	if code := runPRExtensions(args, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var got PRExtensions
	if err := json.Unmarshal([]byte(stdout.String()), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

// Only merged content runs: the key and the files come from the main clone's
// tracked tree, so a PR branch — a foreign one included — can never write the
// steps prm executes on it, and an untracked draft never runs either.
// --stage filters; the main clone's .local can switch the key off.
func TestPRExtensionsRunOnlyMergedContent(t *testing.T) {
	main := t.TempDir()
	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git(main, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(main, ".claude/.claude.git.config"), "PR_EXTENSIONS=.claude/prm/*.md\n")
	writeFile(t, filepath.Join(main, ".claude/prm/notes.md"), "---\nname: notes\nstage: [ensure-pr, round]\ndescription: Draft release notes.\n---\nDraft them.\n")
	writeFile(t, filepath.Join(main, ".claude/prm/audit.md"), "---\nname: audit\nstage: merge\ndescription: Re-check before merge.\n---\nCheck.\n")
	git(main, "add", ".")
	git(main, "commit", "-q", "-m", "c1")
	writeFile(t, filepath.Join(main, ".claude/prm/draft.md"), "---\nname: draft\nstage: round\ndescription: Untracked.\n---\nNo.\n")
	wt := filepath.Join(main, ".worktrees", "feat-x")
	git(main, "worktree", "add", "-q", "-b", "feat/x", wt)
	writeFile(t, filepath.Join(wt, ".claude/prm/branch.md"), "---\nname: branch\nstage: round\ndescription: Branch only.\n---\nNo.\n")
	git(wt, "add", ".")
	git(wt, "commit", "-q", "-m", "c2")

	got := runPRExtensionsJSON(t, "--stage", "round", "--repo", wt)
	if got.PRExtensions == nil || *got.PRExtensions != ".claude/prm/*.md" || got.Stage == nil || *got.Stage != "round" ||
		len(got.Extensions) != 1 || got.Extensions[0].Name != "notes" || got.Extensions[0].Path != filepath.Join(got.MainClone, ".claude/prm/notes.md") {
		t.Fatalf("--stage round = %+v", got)
	}
	all := runPRExtensionsJSON(t, "--repo", wt)
	if len(all.Extensions) != 2 || all.Extensions[0].RelPath != ".claude/prm/audit.md" || strings.Join(all.Extensions[0].Stages, ",") != "merge" {
		t.Fatalf("no stage = %+v", all)
	}
	writeFile(t, filepath.Join(main, ".claude/.claude.git.config.local"), "PR_EXTENSIONS=\n")
	if off := runPRExtensionsJSON(t, "--repo", wt); off.PRExtensions != nil || len(off.Extensions) != 0 {
		t.Fatalf(".local override = %+v", off)
	}
}

// A repo that ships an extension expects it to run: every defect is an
// error naming the file, never a silently skipped extension.
func TestPRExtensionsRefusesWhatCannotRun(t *testing.T) {
	ok := "---\nname: notes\nstage: round\ndescription: Notes.\n---\nDo it.\n"
	for _, tc := range []struct {
		name, glob string
		files      map[string]string
		want       string
	}{
		{"no match", "prm/*.md", map[string]string{"other/x.md": ok}, "matches no file"},
		{"escapes the repo", "../prm/*.md", nil, "relative to the repository root"},
		{"no frontmatter", "prm/*.md", map[string]string{"prm/a.md": "Do it.\n"}, "prm/a.md: no frontmatter"},
		{"misspelt key", "prm/*.md", map[string]string{"prm/a.md": strings.Replace(ok, "stage:", "stages:", 1)}, "field stages not found"},
		{"unknown stage", "prm/*.md", map[string]string{"prm/a.md": strings.Replace(ok, "stage: round", "stage: [round, deploy]", 1)}, `unknown stage "deploy"`},
		{"no description", "prm/*.md", map[string]string{"prm/a.md": strings.Replace(ok, "Notes.", "", 1)}, "description must be one non-empty line"},
		{"no instructions", "prm/*.md", map[string]string{"prm/a.md": strings.TrimSuffix(ok, "Do it.\n")}, "has no instructions"},
		{"duplicate name", "prm/*.md", map[string]string{"prm/a.md": ok, "prm/b.md": ok}, `prm/b.md: name "notes" is already used by prm/a.md`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			files := []string{}
			for rel, text := range tc.files {
				writeFile(t, filepath.Join(root, rel), text)
				files = append(files, rel)
			}
			slices.Sort(files)
			_, err := resolvePRExtensions(tc.glob, files, root)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
