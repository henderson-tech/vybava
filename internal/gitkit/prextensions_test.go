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

// Only merged content runs: the key and the files are read at
// origin/<default>, so a PR branch — a foreign one included — never writes
// the steps prm executes on it, and neither does anything in a working tree:
// an uncommitted edit, an untracked draft, a local commit never pushed.
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
	git(main, "update-ref", "refs/remotes/origin/main", "HEAD")
	git(main, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	merged, _ := exec.Command("git", "-C", main, "rev-parse", "HEAD").Output()
	writeFile(t, filepath.Join(main, ".claude/prm/local.md"), "---\nname: local\nstage: round\ndescription: Never pushed.\n---\nNo.\n")
	git(main, "add", ".")
	git(main, "commit", "-q", "-m", "local only")
	writeFile(t, filepath.Join(main, ".claude/prm/notes.md"), "---\nname: notes\nstage: [ensure-pr, round]\ndescription: Draft release notes.\n---\nUncommitted edit.\n")
	writeFile(t, filepath.Join(main, ".claude/prm/draft.md"), "---\nname: draft\nstage: round\ndescription: Untracked.\n---\nNo.\n")
	wt := filepath.Join(main, ".worktrees", "feat-x")
	git(main, "worktree", "add", "-q", "-b", "feat/x", wt)
	writeFile(t, filepath.Join(wt, ".claude/prm/branch.md"), "---\nname: branch\nstage: round\ndescription: Branch only.\n---\nNo.\n")
	git(wt, "add", ".")
	git(wt, "commit", "-q", "-m", "c2")

	got := runPRExtensionsJSON(t, "--stage", "round", "--repo", wt)
	if got.PRExtensions == nil || *got.PRExtensions != ".claude/prm/*.md" || got.Stage == nil || *got.Stage != "round" ||
		got.Ref != "origin/main" || got.Commit != strings.TrimSpace(string(merged)) ||
		len(got.Extensions) != 1 || got.Extensions[0].Name != "notes" || got.Extensions[0].Instructions != "Draft them." {
		t.Fatalf("--stage round = %+v", got)
	}
	all := runPRExtensionsJSON(t, "--json", "--repo="+wt) // --json and the = form are accepted
	if len(all.Extensions) != 2 || all.Extensions[0].RelPath != ".claude/prm/audit.md" || strings.Join(all.Extensions[0].Stages, ",") != "merge" {
		t.Fatalf("no stage = %+v", all)
	}
	writeFile(t, filepath.Join(main, ".claude/.claude.git.config.local"), "PR_EXTENSIONS=\n")
	if off := runPRExtensionsJSON(t, "--repo", wt); off.PRExtensions != nil || len(off.Extensions) != 0 {
		t.Fatalf(".local override = %+v", off)
	}
	// A tracked .local is branch content: it could pin DEFAULT_BRANCH to a PR.
	git(main, "add", "-f", ".claude/.claude.git.config.local")
	var stdout, stderr strings.Builder
	if code := runPRExtensions([]string{"--repo", wt}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "is tracked by git") {
		t.Fatalf("tracked .local: exit %d, %s", code, stderr.String())
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
		{"symlink", "prm/*.md", map[string]string{"prm/link.md 120000": "/home/x/anything.md"}, "prm/link.md: must be a regular file, not mode 120000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tree := []treeEntry{}
			for key := range tc.files {
				path, mode, found := strings.Cut(key, " ")
				if !found {
					mode = "100644"
				}
				tree = append(tree, treeEntry{mode: mode, oid: key, path: path})
			}
			slices.SortFunc(tree, func(a, b treeEntry) int { return strings.Compare(a.path, b.path) })
			_, err := resolvePRExtensions(tc.glob, tree, func(oid string) (string, error) { return tc.files[oid], nil })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// A caller passing an argument the verb does not take must not believe it
// validated something: every one is GITKIT_BAD_ARGS, exit 2, before any
// repo is read — and --help is real help, never a silent default run.
func TestPRExtensionsRefusesArgumentsItDoesNotTake(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--stage", "ensure-pr", "--ref", "HEAD"}, `unknown argument "--ref"`},
		{[]string{"extra"}, `unknown argument "extra"`},
		{[]string{"--stage"}, "--stage needs a value"},
		{[]string{"--stage=deploy"}, `--stage "deploy" is not a stage`},
		{[]string{"--repo", "--stage", "round"}, "--repo needs a value"},
		{[]string{"--repo", ""}, "--repo needs a value"}, // never a silent fallback to cwd
		{[]string{"--repo=/a", "--repo", "/b"}, "--repo is given twice"},
		{[]string{"help"}, `unknown argument "help"`},
	} {
		var stdout, stderr strings.Builder
		code := runPRExtensions(tc.args, &stdout, &stderr)
		if code != 2 || !strings.Contains(stdout.String(), "GITKIT_BAD_ARGS: "+tc.want) || !strings.Contains(stdout.String(), prExtensionsUsage) {
			t.Errorf("%v: exit %d, stdout %q", tc.args, code, stdout.String())
		}
	}

	var stdout, stderr strings.Builder
	if code := runPRExtensions([]string{"--json", "--bogus"}, &stdout, &stderr); code != 2 {
		t.Fatalf("--json --bogus: exit %d", code)
	}
	var env struct {
		OK          bool `json:"ok"`
		Diagnostics []struct{ Code, Fix string }
	}
	if err := json.Unmarshal([]byte(stdout.String()), &env); err != nil || env.OK || len(env.Diagnostics) != 1 || env.Diagnostics[0].Code != DiagBadArgs {
		t.Fatalf("--json --bogus envelope = %s (%v)", stdout.String(), err)
	}

	stdout.Reset()
	if code := runPRExtensions([]string{"--help", "--ref", "HEAD"}, &stdout, &stderr); code != 0 || !strings.HasPrefix(stdout.String(), "usage: "+prExtensionsUsage) {
		t.Fatalf("--help: exit %d, %q", code, stdout.String())
	}
}
