package memorylint_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/henderson-tech/vybava/internal/memorylint"
)

func TestLintFindsBrokenLinksLegacyFrontmatterAndFixtures(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write(t, filepath.Join(root, "MEMORY.md"), "# Memory\n\n- [Broken](missing.md)\n- [Actual](project-actual.md)\n")
	write(t, filepath.Join(root, "project-actual.md"), `---
name: project-actual
description: When testing memorylint.
metadata:
  type: project
---

See [[missing-memory]]. Contact qa@fixit.invalid from 10.0.0.8.
`)

	report, err := memorylint.Lint([]string{root})
	if err != nil {
		t.Fatalf("Lint() error = %v", err)
	}
	wanted := map[string]bool{"M001": false, "M005": false, "M006": false, "M007": false, "M008": false}
	for _, finding := range report.Findings {
		if _, exists := wanted[finding.Rule]; exists {
			wanted[finding.Rule] = true
		}
	}
	for rule, found := range wanted {
		if !found {
			t.Errorf("Lint() did not report %s: %#v", rule, report.Findings)
		}
	}
}

func TestLintAcceptsHealthyMemoryAndAllowlist(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write(t, filepath.Join(root, ".memorylint.yaml"), "version: 1\nallowed_emails: [qa@fixit.test]\nallowed_ips: [10.0.0.8]\n")
	write(t, filepath.Join(root, "MEMORY.md"), "# Memory\n\n- [Actual](project-actual.md) — When testing memorylint.\n")
	write(t, filepath.Join(root, "project-actual.md"), `---
name: project-actual
description: When testing memorylint.
type: project
status: active
---

Contact qa@fixit.test from 10.0.0.8.
`)

	report, err := memorylint.Lint([]string{root})
	if err != nil {
		t.Fatalf("Lint() error = %v", err)
	}
	if len(report.Findings) != 0 {
		t.Fatalf("Lint() findings = %#v, want none", report.Findings)
	}
}

func TestLintEnforcesTheProvisionalLifecycle(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	note := func(name, status, expires string) {
		front := "---\nname: " + name + "\ndescription: Use when testing lifecycle.\ntype: project\nstatus: " + status + "\n"
		if expires != "" {
			front += "expires: " + expires + "\n"
		}
		write(t, filepath.Join(root, name+".md"), front+"---\n\nBody.\n")
	}
	note("project-no-expires", "provisional", "")
	note("project-bad-expires", "provisional", "someday")
	note("project-expired", "provisional", "2001-01-02")
	note("project-promoted", "active", "2999-01-02")
	note("project-live", "provisional", "2999-01-02")
	index := "# Memory\n"
	for _, name := range []string{"project-no-expires", "project-bad-expires", "project-expired", "project-promoted", "project-live"} {
		index += "- [" + name + "](" + name + ".md) — Use when testing lifecycle.\n"
	}
	write(t, filepath.Join(root, "MEMORY.md"), index)

	report, err := memorylint.Lint([]string{root})
	if err != nil {
		t.Fatalf("Lint() error = %v", err)
	}
	got := map[string]memorylint.Finding{}
	for _, finding := range report.Findings {
		got[filepath.Base(finding.Path)] = finding
	}
	for _, name := range []string{"project-no-expires.md", "project-bad-expires.md", "project-promoted.md"} {
		finding, exists := got[name]
		if !exists || finding.Rule != "M012" || finding.Severity != memorylint.SeverityError {
			t.Errorf("%s: want an M012 error, got %#v", name, got[name])
		}
	}
	expired, exists := got["project-expired.md"]
	if !exists || expired.Rule != "M013" || expired.Severity != memorylint.SeverityWarning {
		t.Errorf("project-expired.md: want an M013 warning, got %#v", expired)
	}
	if exists && !strings.Contains(expired.Message, "deletable on sight") {
		t.Errorf("M013 message must make prune mechanical, got %q", expired.Message)
	}
	if finding, exists := got["project-live.md"]; exists {
		t.Errorf("a live provisional must lint clean, got %#v", finding)
	}
}

func TestLintFlagsEvictionCandidates(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	note := func(name string, extra ...string) {
		front := "---\nname: " + name + "\ndescription: Use when testing eviction.\ntype: project\nstatus: active\n"
		for _, line := range extra {
			front += line + "\n"
		}
		write(t, filepath.Join(root, name+".md"), front+"---\n\nBody.\n")
	}
	note("project-stale", "last-verified: 2020-01-01")
	note("project-recent", "last-used: 2999-01-02")
	note("project-fresh-wins", "last-verified: 2000-01-01", "last-used: 2999-01-02")
	note("project-undated")
	note("project-bad-used", "last-used: someday")
	write(t, filepath.Join(root, "MEMORY.md"), "# Memory\n")

	report, err := memorylint.Lint([]string{root})
	if err != nil {
		t.Fatalf("Lint() error = %v", err)
	}
	byRule := map[string][]string{}
	for _, finding := range report.Findings {
		byRule[finding.Rule] = append(byRule[finding.Rule], filepath.Base(finding.Path))
	}
	if got := byRule["M014"]; len(got) != 1 || got[0] != "project-stale.md" {
		t.Errorf("M014 must flag exactly the stale note, got %v", got)
	}
	if got := byRule["M012"]; len(got) != 1 || got[0] != "project-bad-used.md" {
		t.Errorf("a malformed last-used must be an M012 error, got %v", got)
	}
	for _, finding := range report.Findings {
		if finding.Rule == "M014" && !strings.Contains(finding.Message, "eviction candidate") {
			t.Errorf("M014 message must name the eviction, got %q", finding.Message)
		}
	}
}

func TestLintFlagsNoteCountCeilings(t *testing.T) {
	t.Parallel()

	build := func(count int, noteType string) string {
		root := t.TempDir()
		write(t, filepath.Join(root, "MEMORY.md"), "# Memory\n")
		for i := 0; i < count; i++ {
			name := noteType + "-note-" + string(rune('a'+i/10)) + string(rune('a'+i%10))
			write(t, filepath.Join(root, name+".md"),
				"---\nname: "+name+"\ndescription: Use when testing ceilings.\ntype: "+noteType+"\nstatus: active\n---\n\nBody.\n")
		}
		return root
	}
	ceilings := func(root string) []memorylint.Finding {
		t.Helper()
		report, err := memorylint.Lint([]string{root})
		if err != nil {
			t.Fatal(err)
		}
		var out []memorylint.Finding
		for _, finding := range report.Findings {
			if finding.Rule == "M015" {
				out = append(out, finding)
			}
		}
		return out
	}

	over := ceilings(build(16, "feedback"))
	if len(over) != 1 || over[0].Severity != memorylint.SeverityWarning ||
		!strings.Contains(over[0].Message, "16") || !strings.Contains(over[0].Message, "15") {
		t.Errorf("a personal home over 15 notes must get one M015 warning naming count and ceiling, got %#v", over)
	}
	if got := ceilings(build(16, "project")); len(got) != 0 {
		t.Errorf("16 notes in a team-typed home is under the 30 ceiling, got %#v", got)
	}
	if got := ceilings(build(31, "project")); len(got) != 1 {
		t.Errorf("a team home over 30 notes must get one M015 warning, got %#v", got)
	}
}

func write(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A description containing " #" is a YAML comment when unquoted, so it parses
// truncated. `fix` must recover the full line instead of writing the truncation
// back — the loss is silent and permanent otherwise (observed 2026-08-30).
func TestFixRecoversCommentTruncatedDescription(t *testing.T) {
	root := t.TempDir()
	note := filepath.Join(root, "project-se-cache.md")
	write(t, note, "---\nname: project-se-cache\ndescription: After PR #22, the store cached the public half but hasKey checked only the private item\ntype: project\nstatus: active\n---\n\nbody\n")

	if _, _, err := memorylint.Fix([]string{root}, false); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	got, err := os.ReadFile(note)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(got), "hasKey checked only the private item") {
		t.Errorf("description truncated at the comment marker; got:\n%s", got)
	}
	// And the rewritten value must survive a round-trip as real YAML.
	var props struct {
		Description string `yaml:"description"`
	}
	front := string(got)
	front = front[4 : strings.Index(front[4:], "\n---")+4]
	if err := yaml.Unmarshal([]byte(front), &props); err != nil {
		t.Fatalf("rewritten frontmatter is not valid YAML: %v", err)
	}
	if !strings.Contains(props.Description, "hasKey") {
		t.Errorf("round-trip lost the tail: %q", props.Description)
	}
}

// A genuine trailing comment after a quoted scalar is NOT a truncation.
func TestFixLeavesQuotedScalarWithCommentAlone(t *testing.T) {
	root := t.TempDir()
	note := filepath.Join(root, "project-quoted.md")
	write(t, note, "---\nname: project-quoted\ndescription: \"a real description\" # trailing note\ntype: project\nstatus: active\n---\n\nbody\n")

	if _, _, err := memorylint.Fix([]string{root}, false); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	got, _ := os.ReadFile(note)
	if strings.Contains(string(got), "trailing note") {
		t.Errorf("a trailing comment on a quoted scalar was absorbed into the value:\n%s", got)
	}
}

func TestLintOfNestedNoteResolvesItsHome(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write(t, filepath.Join(root, "MEMORY.md"), "# Memory\n\n- [Root](project-root.md) — When testing memorylint.\n- [Sub](inbox/project-sub.md) — When testing nested notes.\n")
	write(t, filepath.Join(root, "project-root.md"), "---\nname: project-root\ndescription: When testing memorylint.\ntype: project\nstatus: active\n---\n\nRoot.\n")
	writeDeep(t, filepath.Join(root, "inbox", "project-sub.md"), "---\nname: project-sub\ndescription: When testing nested notes.\ntype: project\nstatus: active\n---\n\nSee [[project-root]].\n")

	report, err := memorylint.Lint([]string{filepath.Join(root, "inbox", "project-sub.md")})
	if err != nil {
		t.Fatalf("Lint() error = %v", err)
	}
	if len(report.Findings) != 0 || report.Files != 1 {
		t.Fatalf("Lint(nested note) = files %d, findings %#v; want a clean single note linted against its home", report.Files, report.Findings)
	}
}

// TestLintLedgerHome pins ledger mode: LEDGER.md is not a note, notes/ names
// are plain kebab-case, MEMORY.md drift is an error, and a v2 home without a
// ledger is untouched by any of it.
func TestLintLedgerHome(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	write(t, filepath.Join(root, "LEDGER.md"), "---\nmemo: 1\nalias: t\nkind: team\n---\n- #t1 project/api Fact. -> [[notes/detail]] ^t1\n")
	write(t, filepath.Join(root, "MEMORY.md"), "# stale\n")
	if err := os.MkdirAll(filepath.Join(root, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "notes", "detail.md"), "---\nname: detail\ndescription: The long form.\ntype: project\nstatus: active\n---\n\nSee [[LEDGER#^t1]] and [[other-home/notes/x]].\n")

	report, err := memorylint.Lint([]string{root})
	if err != nil {
		t.Fatalf("Lint() error = %v", err)
	}
	seen := map[string]bool{}
	for _, f := range report.Findings {
		seen[f.Rule] = true
	}
	if !seen["L006"] {
		t.Errorf("ledger mode must report MEMORY.md drift: %#v", report.Findings)
	}
	for _, rule := range []string{"M001", "M002", "M006", "M009"} {
		if seen[rule] {
			t.Errorf("ledger mode must not report %s: %#v", rule, report.Findings)
		}
	}
}

// TestLintLedgerTeamHomeLocalSurface pins the 2026-09-21 decision: a team
// home's committed tree carries no MEMORY.md / usage.jsonl and lints clean;
// a team home that leaves them unignored earns an L008 warning; gitignored
// they are silent again.
func TestLintLedgerTeamHomeLocalSurface(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatal(string(out))
		}
	}
	root := filepath.Join(repo, ".claude", "memory")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "LEDGER.md"), "---\nmemo: 1\nalias: t\nkind: team\n---\n- #t1 project/api Fact. ^t1\n")

	rules := func() map[string]bool {
		t.Helper()
		report, err := memorylint.Lint([]string{root})
		if err != nil {
			t.Fatalf("Lint() error = %v", err)
		}
		seen := map[string]bool{}
		for _, f := range report.Findings {
			seen[f.Rule] = true
		}
		return seen
	}
	if seen := rules(); seen["L006"] || seen["L008"] {
		t.Errorf("a team home without its hot surface must lint clean: %v", seen)
	}

	write(t, filepath.Join(root, "MEMORY.md"), "# stale\n")
	write(t, filepath.Join(root, "usage.jsonl"), "")
	if seen := rules(); !seen["L008"] {
		t.Errorf("an unignored hot surface in a team home must warn L008: %v", seen)
	}

	write(t, filepath.Join(root, ".gitignore"), "MEMORY.md\nusage.jsonl\n")
	if seen := rules(); seen["L008"] {
		t.Errorf("a gitignored hot surface must not warn: %v", seen)
	}
}
