package memorylint

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The exact shape Claude Code writes back when its Edit tool touches a note in
// the agent-managed memory home (captured 2026-08-22).
const harnessRewritten = `---
name: zz-probe
description: Temporary probe note.
metadata: 
  node_type: memory
  type: reference
  status: active
  originSessionId: fb778e3b-6574-44fa-a2f5-f063c7890e87
  modified: 2026-08-22T20:15:15.860Z
---

# Probe

MARKER
`

func TestNormalizeRepairsHarnessRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zz-probe.md")
	if err := os.WriteFile(path, []byte(harnessRewritten), 0o644); err != nil {
		t.Fatal(err)
	}
	got, changed, err := Normalize(path)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected the legacy envelope to be rewritten")
	}
	out := string(got)
	for _, want := range []string{"name: zz-probe", "type: reference", "status: active", `last-verified: "2026-08-22"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "metadata:") || strings.Contains(out, "originSessionId") || strings.Contains(out, "node_type") {
		t.Errorf("legacy envelope survived:\n%s", out)
	}
	if !strings.Contains(out, "# Probe") || !strings.Contains(out, "MARKER") {
		t.Errorf("body lost:\n%s", out)
	}
	if strings.Contains(out, "---\n---") {
		t.Errorf("body extraction mangled the delimiters:\n%s", out)
	}
}

func TestNormalizeIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "zz-probe.md")
	if err := os.WriteFile(path, []byte(harnessRewritten), 0o644); err != nil {
		t.Fatal(err)
	}
	first, _, err := Normalize(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, first, 0o644); err != nil {
		t.Fatal(err)
	}
	second, changed, err := Normalize(path)
	if err != nil {
		t.Fatal(err)
	}
	if changed || string(second) != string(first) {
		t.Fatalf("second pass changed a normalized note:\n%s", second)
	}
}

func TestRefuseManagedBlocksOnlyTheAgentManagedHome(t *testing.T) {
	managed := "/Users/x/.claude/projects/-Users-x-repo/memory/feedback-a.md"
	team := "/Users/x/repo/.claude/memory/project-a.md"

	edit := HookPayload{ToolName: "Edit"}
	if _, refused := RefuseManaged(edit, []string{managed}); !refused {
		t.Error("Edit on the agent-managed home must be refused")
	}
	if _, refused := RefuseManaged(edit, []string{team}); refused {
		t.Error("Edit on the committed team home must NOT be refused — the harness leaves it alone")
	}
	for _, tool := range []string{"Bash", "apply_patch"} {
		if _, refused := RefuseManaged(HookPayload{ToolName: tool}, []string{managed}); refused {
			t.Errorf("%s writes the bytes it is given; it must not be refused", tool)
		}
	}
}

func TestRefsFlagsOnlyUnresolvedReferences(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "project-payments.md"), []byte("---\nname: project-payments\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "service.ts")
	body := "// see `notes/memory/project-payments.md`\n" +
		"// see `notes/memory/" + goneFixture + "`\n" +
		"// see notes/docs/payments/overview.md\n" +
		"// see " + goneFixture + "\n"
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Refs(home, []string{src}, RefOptions{Prefix: "notes/memory"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 1 || report.Findings[0].Line != 2 {
		t.Fatalf("path mode should flag exactly the qualified path on line 2: %#v", report.Findings)
	}

	bare, err := Refs(home, []string{src}, RefOptions{Prefix: "notes/memory", Bare: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(bare.Findings) != 2 {
		t.Fatalf("bare mode should add the unqualified name on line 4: %#v", bare.Findings)
	}
}

func TestNoteNamePatternIgnoresBarePrefixWords(t *testing.T) {
	// `reference.md` is a real sibling document in several skills.
	for _, quiet := range []string{" see reference.md#anchor", " see user.md", " see project.md", " see .claude/docs/project-old.md"} {
		if m := noteNamePatternFor(nil).FindStringSubmatch(quiet); m != nil {
			t.Errorf("false positive on %q: %v", quiet, m)
		}
	}
	if noteNamePatternFor(nil).FindStringSubmatch(" see project-a.md") == nil {
		t.Error("a separator-qualified name must still match")
	}
}

func TestReindexIsDeterministicAndSkipsSuperseded(t *testing.T) {
	home := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(home, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("project-b.md", "---\nname: project-b\ndescription: Use when b.\ntype: project\nstatus: active\n---\n")
	write("project-a.md", "---\nname: project-a\ndescription: Use when a.\ntype: project\nstatus: active\n---\n")
	write("project-old.md", "---\nname: project-old\ndescription: Gone.\ntype: project\nstatus: superseded\n---\n")

	first, err := Reindex(home, "", false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Reindex(home, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("reindex is not deterministic")
	}
	out := string(first)
	if strings.Index(out, "project-a") > strings.Index(out, "project-b") {
		t.Errorf("entries must be sorted:\n%s", out)
	}
	if strings.Contains(out, "project-old") {
		t.Errorf("superseded notes must not be indexed:\n%s", out)
	}
}

// Assembled at runtime so a repo-wide `refs --bare` sweep does not read this
// synthetic name as a real reference from committed source.
var goneFixture = "project" + "_gone_from_the_home.md"

// Each of these reproduces a defect the pre-merge audit found by running the
// real binary; they fail against the first draft of this port.

func TestReindexRefusesToDropNotesItCannotClassify(t *testing.T) {
	home := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(home, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("project-ok.md", "---\nname: project-ok\ndescription: Use when ok.\ntype: project\nstatus: active\n---\n")

	for name, body := range map[string]string{
		"project-broken.md":  "no frontmatter at all\n",
		"project-badyaml.md": "---\nname: project-badyaml\ndescription: [unclosed\n---\n",
		"project-weird.md":   "---\nname: project-weird\ndescription: Use when weird.\ntype: something-else\nstatus: active\n---\n",
	} {
		write(name, body)
		if _, err := Reindex(home, "", false); err == nil {
			t.Errorf("%s: reindex silently omitted an unclassifiable note instead of failing", name)
		}
		if err := os.Remove(filepath.Join(home, name)); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := Reindex(home, "", false); err != nil {
		t.Fatalf("a clean home must still index: %v", err)
	}
}

func TestReindexEmitsTheTeamRoutingLine(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "feedback-a.md"),
		[]byte("---\nname: feedback-a\ndescription: Use when a.\ntype: feedback\nstatus: active\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rendered, err := Reindex(home, ".claude/memory/MEMORY.md", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), "Team memory: `.claude/memory/MEMORY.md`") {
		t.Fatalf("the routing line to the companion home was dropped:\n%s", rendered)
	}
}

func TestNormalizeRepairsANameStemMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "project-new-name.md")
	if err := os.WriteFile(path,
		[]byte("---\nname: project-old-name\ndescription: Use when x.\ntype: project\nstatus: active\n---\n\nBody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, changed, err := Normalize(path)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("fix reported success while leaving the note failing check")
	}
	if !strings.Contains(string(got), "name: project-new-name") {
		t.Fatalf("name was not repaired from the filename stem:\n%s", got)
	}
}

func TestFixSkipsUnparseableNotesInsteadOfAbortingTheRun(t *testing.T) {
	home := t.TempDir()
	legacy := "---\nname: %s\ndescription: Use when x.\nmetadata:\n  type: project\n---\n\nBody\n"
	for _, name := range []string{"project-a", "project-z"} {
		if err := os.WriteFile(filepath.Join(home, name+".md"), []byte(fmt.Sprintf(legacy, name)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Sorts between the two, so an abort here would leave the corpus half-fixed.
	if err := os.WriteFile(filepath.Join(home, "project-readme.md"), []byte("no frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, failures, err := Fix([]string{home}, false)
	if err != nil {
		t.Fatalf("one bad file must not abort the run: %v", err)
	}
	if len(changed) != 2 {
		t.Errorf("both good notes must be fixed, got %v", changed)
	}
	if len(failures) != 1 {
		t.Errorf("the bad file must be reported, got %v", failures)
	}
}

func TestNewNoteStaysInsideTheHome(t *testing.T) {
	home := t.TempDir()
	victim := filepath.Join(filepath.Dir(home), "victim")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewNote(home, "project", "../victim/project-pwned", "Use when x.", false); err == nil {
		t.Fatal("a traversing slug must be refused")
	}
	if entries, _ := os.ReadDir(victim); len(entries) != 0 {
		t.Fatal("a note was written outside the home")
	}
	for _, bad := range []string{"Project-Upper", "project name", "project/sub", ".."} {
		if _, err := NewNote(home, "project", bad, "Use when x.", false); err == nil {
			t.Errorf("slug %q must be refused", bad)
		}
	}
	if _, err := NewNote(home, "project", "project-good", "Use when good.", false); err != nil {
		t.Errorf("a valid slug must be accepted: %v", err)
	}
}

func TestHookTargetsRequireARealMemoryHome(t *testing.T) {
	// This hook is registered globally, so a directory merely NAMED memory/ in an
	// unrelated repo must not be linted as a note corpus.
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	notAHome := filepath.Join(home, "memory", "_index.md")
	if err := os.WriteFile(notAHome, []byte("# Docs index\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := HookPayload{ToolName: "Edit"}
	p.ToolInput.FilePath = notAHome
	if got := HookTargets(p); len(got) != 0 {
		t.Fatalf("a memory/ directory without MEMORY.md is not a home: %v", got)
	}

	if err := os.WriteFile(filepath.Join(home, "memory", "MEMORY.md"), []byte("# Memory Index\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := HookTargets(p); len(got) != 1 {
		t.Fatalf("once MEMORY.md exists it is a home: %v", got)
	}
}

func TestReindexReadsThroughTheLegacyEnvelope(t *testing.T) {
	// A note that has not been `fix`ed yet is still readable, so refusing on it
	// would make the tool unusable on the live corpus rather than safe.
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "project-legacy.md"),
		[]byte("---\nname: project-legacy\ndescription: Use when legacy.\nmetadata:\n  type: project\n  status: active\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rendered, err := Reindex(home, "", false)
	if err != nil {
		t.Fatalf("a legacy-envelope note must still index: %v", err)
	}
	if !strings.Contains(string(rendered), "project-legacy") {
		t.Fatalf("legacy note missing from the index:\n%s", rendered)
	}
}

func TestReindexHonoursTheHomesConfiguredTypes(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".memorylint.yaml"),
		[]byte("version: 1\nallowed_types: [user, feedback, project, reference, runbook]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "runbook-b.md"),
		[]byte("---\nname: runbook-b\ndescription: Use when b.\ntype: runbook\nstatus: active\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// `new` and `check` accept this type, so `reindex` refusing it forever would
	// make the home permanently un-indexable.
	rendered, err := Reindex(home, "", false)
	if err != nil {
		t.Fatalf("a configured type must be indexable: %v", err)
	}
	if !strings.Contains(string(rendered), "runbook-b") {
		t.Fatalf("configured type missing from the index:\n%s", rendered)
	}
}

func TestFixPreservesFrontmatterKeysOutsideTheSchema(t *testing.T) {
	// No lint rule forbids extra keys, so the documented repair command must not
	// destroy them.
	home := t.TempDir()
	path := filepath.Join(home, "project-extra.md")
	if err := os.WriteFile(path, []byte("---\nname: project-extra\ndescription: Use when x.\n"+
		"metadata:\n  type: project\nsource: https://example.com/spec\nowner: lukas\nrelated:\n  - project-other\n---\n\nBody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Fix([]string{home}, false); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := string(got)
	for _, want := range []string{"type: project", "source: https://example.com/spec", "owner: lukas", "related:", "project-other", "Body"} {
		if !strings.Contains(out, want) {
			t.Errorf("fix destroyed %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "metadata:") {
		t.Errorf("the legacy envelope should be gone:\n%s", out)
	}
	// And it must still round-trip to the same bytes.
	second, changed, err := Normalize(path)
	if err != nil {
		t.Fatal(err)
	}
	if changed || string(second) != out {
		t.Errorf("not idempotent with extra keys:\n%s", second)
	}
}

func TestGraphRendersEdgesAndSurvivesDegenerateHomes(t *testing.T) {
	home := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(home, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("project-a.md", "---\nname: project-a\ndescription: Use when a.\ntype: project\nstatus: active\n---\n\nSee [[project-b]].\n")
	write("project-b.md", "---\nname: project-b\ndescription: Use when b.\ntype: project\nstatus: active\n---\n\nNo links.\n")

	dot, err := Graph([]string{home}, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"digraph memory {", `"project-a" -> "project-b"`, "}"} {
		if !strings.Contains(dot, want) {
			t.Errorf("missing %q in:\n%s", want, dot)
		}
	}

	data, err := GraphData([]string{home}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Edges) != 1 || data.Edges[0].From != "project-a" || data.Edges[0].To != "project-b" {
		t.Errorf("unexpected edges %#v", data.Edges)
	}

	// Degenerate inputs must not panic or divide by zero.
	empty := t.TempDir()
	for _, similar := range []bool{false, true} {
		if _, err := Graph([]string{empty}, similar); err != nil {
			t.Errorf("empty home similar=%v: %v", similar, err)
		}
	}
	if err := os.WriteFile(filepath.Join(empty, "project-zero.md"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Graph([]string{empty}, true); err != nil {
		t.Errorf("zero-byte note: %v", err)
	}
	if _, err := Graph([]string{filepath.Join(home, "nope")}, false); err == nil {
		t.Error("a missing home must be an error, not silence")
	}
}

func TestGraphSimilarFindsNearDuplicates(t *testing.T) {
	home := t.TempDir()
	shared := strings.Repeat("dispatch realtime provider accept queue coverage ", 12)
	for _, name := range []string{"project-one", "project-two"} {
		if err := os.WriteFile(filepath.Join(home, name+".md"),
			[]byte("---\nname: "+name+"\ndescription: Use when x.\ntype: project\nstatus: active\n---\n\n"+shared), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := GraphData([]string{home}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Pairs) != 1 || data.Pairs[0].Score < 0.42 {
		t.Fatalf("two near-identical notes must pair: %#v", data.Pairs)
	}
}

func TestFixDoesNotReportANoteItFailedToWrite(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "project-ro.md")
	if err := os.WriteFile(path,
		[]byte("---\nname: project-ro\ndescription: Use when x.\nmetadata:\n  type: project\n---\n\nBody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// atomicWrite renames into the directory, so a read-only directory fails the
	// write while leaving the note readable.
	if err := os.Chmod(home, 0o555); err != nil {
		t.Skip("cannot make the directory read-only here")
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o755) })

	changed, failures, err := Fix([]string{home}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Errorf("a note that failed to write must not be reported as changed: %v", changed)
	}
	if len(failures) != 1 {
		t.Errorf("the failure must be reported: %v", failures)
	}
}

func TestNewNoteRefusesANameTheLinterWouldReject(t *testing.T) {
	home := t.TempDir()
	// `topic.md` does not match the `<type>-<slug>.md` filename rule, so `new`
	// must not be able to create a note `check` then complains about.
	if _, err := NewNote(home, "project", "topic", "Use when x.", false); err == nil {
		t.Error("a name without the type prefix must be refused")
	}
	if _, err := NewNote(home, "project", "project-topic", "Use when x.", false); err != nil {
		t.Errorf("a conventional name must be accepted: %v", err)
	}
}

func TestHookGuardsUnindexedCodexHomesToo(t *testing.T) {
	// This binary is the hook for BOTH agents, and Discover already treats
	// .codex/memory as a home — so leaving it out meant a Codex home's first
	// note was never scanned.
	for _, home := range []string{
		"/u/.codex/memory/project-a.md",
		"/u/.Codex/memory/project-a.md",
		"/u/.codex/projects/-u-repo/memory/project-a.md",
		"/u/.claude/memory/inbox/project-a.md",
	} {
		p := HookPayload{ToolName: "Write"}
		p.ToolInput.FilePath = home
		if got := HookTargets(p); len(got) != 1 {
			t.Errorf("%s must be guarded, got %v", home, got)
		}
	}
	// ...while a lookalike that is not a home still is not one.
	for _, other := range []string{"/u/.claude/memory-notes/project-a.md", "/u/docs/memory/project-a.md"} {
		p := HookPayload{ToolName: "Write"}
		p.ToolInput.FilePath = other
		if got := HookTargets(p); len(got) != 0 {
			t.Errorf("%s must not be treated as a home, got %v", other, got)
		}
	}
}

func TestConfigErrorsPropagateInsteadOfSilentlyUsingDefaults(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".memorylint.yaml"), []byte("version: 99\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "project-a.md"),
		[]byte("---\nname: project-a\ndescription: Use when a.\ntype: project\nstatus: active\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Lint rejects this config, so reindex must not rewrite the index under a
	// different policy, and new must not create notes under one either.
	if _, err := Reindex(home, "", true); err == nil {
		t.Error("reindex must refuse an unsupported config version")
	}
	if _, err := NewNote(home, "project", "project-b", "Use when b.", false); err == nil {
		t.Error("new must refuse an unsupported config version")
	}
	if _, err := os.Stat(filepath.Join(home, "MEMORY.md")); err == nil {
		t.Error("no index should have been written")
	}
}

func TestSubdirectoryNotesAreReachableByFixAndReindex(t *testing.T) {
	// check and the write hook both see subdirectory notes, so fix and reindex
	// must too — otherwise `fix` reports "0 changed" on a note check calls an
	// error, and reindex omits it without the omission guard firing.
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(home, "inbox", "project-sub.md")
	if err := os.WriteFile(nested,
		[]byte("---\nname: project-wrong-stem\ndescription: Use when sub.\nmetadata:\n  type: project\n---\n\nBody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, failures, err := Fix([]string{home}, false)
	if err != nil || len(failures) != 0 {
		t.Fatalf("fix: %v %v", err, failures)
	}
	if len(changed) != 1 {
		t.Fatalf("a subdirectory note must be reachable by fix, got %v", changed)
	}
	rendered, err := Reindex(home, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), "(inbox/project-sub.md)") {
		t.Fatalf("a subdirectory note must be indexed by its path:\n%s", rendered)
	}
}

func TestHookTreatsAnUnknownEventAsPreWrite(t *testing.T) {
	// Failing open on an unrecognised event name let a secret through: the
	// post-write branch lints a file that is not on disk yet and finds nothing.
	payload := `{"hook_event_name":"pre_tool_use","tool_name":"Write","tool_input":` +
		`{"file_path":"/u/.claude/memory/project-a.md","content":"ghp_abcdefghijklmnopqrstuvwxyz012345"}}`
	if got := RunHook(strings.NewReader(payload)); !got.Block {
		t.Fatal("an unrecognised event must be judged as pre-write, not waved through")
	}
}

func TestHookPreWriteReadsTheAllowlistFromTheHomeRoot(t *testing.T) {
	// A ledger home keeps its notes in notes/; the pre-write judged them against
	// notes/.memory-lint-allow, which never exists, so an allowlisted value blocked.
	home := filepath.Join(t.TempDir(), ".claude", "memory")
	if err := os.MkdirAll(filepath.Join(home, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".memory-lint-allow"), []byte("10.8.0.10\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	write := func(content string) HookDecision {
		payload, err := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Write",
			"tool_input": map[string]any{"file_path": filepath.Join(home, "notes", "wireguard.md"), "content": content}})
		if err != nil {
			t.Fatal(err)
		}
		return RunHook(strings.NewReader(string(payload)))
	}
	if got := write("The box answers on 10.8.0.10 over WireGuard."); got.Block {
		t.Fatalf("an address the home allowlists must pass under notes/: %s", got.Message)
	}
	if got := write("The box answers on 10.8.0.11 over WireGuard."); !got.Block {
		t.Fatal("an address the home does not allowlist must still block")
	}

	// A patch across two homes passes only what BOTH allowlist.
	other := filepath.Join(t.TempDir(), ".claude", "memory")
	if err := os.MkdirAll(filepath.Join(other, "notes"), 0o755); err != nil {
		t.Fatal(err)
	}
	patch := "*** Begin Patch\n*** Add File: " + filepath.Join(home, "notes", "a.md") + "\n+The box answers on 10.8.0.10.\n" +
		"*** Add File: " + filepath.Join(other, "notes", "b.md") + "\n+Nothing here.\n*** End Patch\n"
	payload, err := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "tool_name": "apply_patch",
		"tool_input": map[string]any{"command": patch}})
	if err != nil {
		t.Fatal(err)
	}
	if got := RunHook(strings.NewReader(string(payload))); !got.Block {
		t.Fatal("one home's allowlist must not clear a value for another home in the same patch")
	}
}

func TestReindexSurvivesAnEmptyConfiguredType(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".memorylint.yaml"),
		[]byte("version: 1\nallowed_types: [\"\", project]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "project-a.md"),
		[]byte("---\nname: project-a\ndescription: Use when a.\ntype: project\nstatus: active\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Reindex(home, "", false); err != nil {
		t.Fatalf("an empty configured type must not panic or fail: %v", err)
	}
}

func TestGraphJSONListsOrphanNodes(t *testing.T) {
	home := t.TempDir()
	for name, body := range map[string]string{
		"project-linked": "See [[project-target]].",
		"project-target": "No links.",
		"project-orphan": "Nothing links here.",
	} {
		if err := os.WriteFile(filepath.Join(home, name+".md"),
			[]byte("---\nname: "+name+"\ndescription: Use when x.\ntype: project\nstatus: active\n---\n\n"+body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data, err := GraphData([]string{home}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Nodes) != 3 {
		t.Fatalf("every note must appear as a node, got %v", data.Nodes)
	}
	var orphan bool
	for _, n := range data.Nodes {
		if n == "project-orphan" {
			orphan = true
		}
	}
	if !orphan {
		t.Errorf("an unlinked note must be visible to a JSON consumer: %v", data.Nodes)
	}
}

func TestFixContinuesPastAnUnreadableHome(t *testing.T) {
	good := t.TempDir()
	if err := os.WriteFile(filepath.Join(good, "project-a.md"),
		[]byte("---\nname: project-a\ndescription: Use when a.\nmetadata:\n  type: project\n---\n\nBody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, failures, err := Fix([]string{filepath.Join(good, "missing"), good}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 1 {
		t.Errorf("a later home must still be processed, got %v", changed)
	}
	if len(failures) != 1 {
		t.Errorf("the unreadable home must be reported, got %v", failures)
	}
}

func TestPostWriteLintsFromTheHomeRootNotTheNotesDirectory(t *testing.T) {
	// Linting `inbox/` as if it were a home makes a wikilink to a root sibling an
	// unresolved M006, so the hook refuses a perfectly valid write.
	home := t.TempDir()
	mem := filepath.Join(home, ".claude", "memory")
	if err := os.MkdirAll(filepath.Join(mem, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(mem, "project-root.md"), "---\nname: project-root\ndescription: Use when root.\ntype: project\nstatus: active\n---\n")
	write(filepath.Join(mem, "MEMORY.md"), "# Memory Index\n\n- [project-root](project-root.md) — Use when root.\n- [project-sub](inbox/project-sub.md) — Use when sub.\n")
	nested := filepath.Join(mem, "inbox", "project-sub.md")
	write(nested, "---\nname: project-sub\ndescription: Use when sub.\ntype: project\nstatus: active\n---\n\nSee [[project-root]].\n")

	payload := `{"hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"file_path":"` + nested + `"}}`
	if got := RunHook(strings.NewReader(payload)); got.Block {
		t.Fatalf("a valid nested note must not be refused: %s", got.Message)
	}
}

func TestNewNoteRefusesToFollowAnEntryCreatedUnderIt(t *testing.T) {
	home := t.TempDir()
	target := filepath.Join(home, "project-taken.md")
	if err := os.WriteFile(target, []byte("existing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewNote(home, "project", "project-taken", "Use when x.", false); err == nil {
		t.Fatal("an existing path must not be overwritten")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "existing\n" {
		t.Fatalf("the existing file was clobbered: %q", got)
	}
}

func TestFilenameRuleFollowsTheHomesConfiguredTypes(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".memorylint.yaml"),
		[]byte("version: 1\nallowed_types: [user, feedback, project, reference, runbook]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := NewNote(home, "runbook", "runbook-topic", "Use when topic.", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "MEMORY.md"),
		[]byte("# Memory Index\n\n- [runbook-topic](runbook-topic.md) — Use when topic.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Lint([]string{home})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range report.Findings {
		if f.Rule == "M002" {
			t.Errorf("new created %s and check then rejected its name: %s", filepath.Base(path), f.Message)
		}
	}
}

func TestPostWriteDoesNotFailOpenWhenTheHomeIsPartlyUnreadable(t *testing.T) {
	// Post-write is the only check covering a writer PreToolUse cannot see (a
	// Bash heredoc), so swallowing a lint error there ships the secret.
	home := t.TempDir()
	mem := filepath.Join(home, ".claude", "memory")
	if err := os.MkdirAll(filepath.Join(mem, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mem, "MEMORY.md"), []byte("# Memory Index\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(mem, "inbox", "project-sub.md")
	if err := os.WriteFile(nested,
		[]byte("---\nname: project-sub\ndescription: Use when sub.\ntype: project\nstatus: active\n---\n\ntoken: ghp_abcdefghijklmnopqrstuvwxyz012345\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(mem, "project-locked.md")
	if err := os.WriteFile(locked, []byte("---\nname: project-locked\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Skip("cannot make a file unreadable here")
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })

	payload := `{"hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"file_path":"` + nested + `"}}`
	if got := RunHook(strings.NewReader(payload)); !got.Block {
		t.Fatal("an unreadable sibling must not disarm the guard for this note")
	}
}

func TestPostWriteIgnoresPreExistingIndexDefects(t *testing.T) {
	// A payload that touches a note AND the index must not be refused because the
	// index already had a dangling entry the write did not introduce.
	home := t.TempDir()
	mem := filepath.Join(home, ".claude", "memory")
	if err := os.MkdirAll(mem, 0o755); err != nil {
		t.Fatal(err)
	}
	note := filepath.Join(mem, "project-new.md")
	if err := os.WriteFile(note,
		[]byte("---\nname: project-new\ndescription: Use when new.\ntype: project\nstatus: active\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(mem, "MEMORY.md")
	if err := os.WriteFile(index,
		[]byte("# Memory Index\n\n- [project-new](project-new.md) — Use when new.\n- [project-gone](project-gone.md) — stale.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// `file_path` AND `path` are both read by HookPayload, so this genuinely
	// makes the index a co-target. An earlier version of this test hung the index
	// off a top-level "extra" key, which the payload struct never reads — so
	// MEMORY.md was never a target, the skip under test was never reached, and
	// the test passed against the unfixed code. It measured nothing.
	payload := `{"hook_event_name":"PostToolUse","tool_name":"Write","tool_input":` +
		`{"file_path":"` + note + `","path":"` + index + `"}}`
	if targets := hookTargetsFor(payload); len(targets) != 2 {
		t.Fatalf("the index must really be a co-target, got %v", targets)
	}
	if got := RunHook(strings.NewReader(payload)); got.Block {
		t.Fatalf("a pre-existing index defect must not refuse this write: %s", got.Message)
	}
}

func hookTargetsFor(payload string) []string {
	var p HookPayload
	if err := json.Unmarshal([]byte(payload), &p); err != nil {
		return nil
	}
	return HookTargets(p)
}

func TestPostWriteRefusesEveryNoteInAHomeItCannotValidate(t *testing.T) {
	// The previous shape fell back to the first target's own directory, so the
	// remaining notes in that home were silently skipped and whether a secret
	// shipped depended on patch hunk order. Both orders must refuse now.
	home := t.TempDir()
	mem := filepath.Join(home, ".claude", "memory")
	if err := os.MkdirAll(filepath.Join(mem, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(mem, "MEMORY.md"), "# Memory Index\n")
	nested := filepath.Join(mem, "inbox", "project-sub.md")
	write(nested, "---\nname: project-sub\ndescription: Use when sub.\ntype: project\nstatus: active\n---\n")
	secret := filepath.Join(mem, "project-secret.md")
	write(secret, "---\nname: project-secret\ndescription: Use when secret.\ntype: project\nstatus: active\n---\n\ntoken: ghp_abcdefghijklmnopqrstuvwxyz012345\n")

	// Make the home un-lintable, the way a dangling symlink or a locked file does.
	locked := filepath.Join(mem, "project-locked.md")
	write(locked, "---\nname: project-locked\n---\n")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Skip("cannot make a file unreadable here")
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })

	for _, order := range [][2]string{{nested, secret}, {secret, nested}} {
		payload := `{"hook_event_name":"PostToolUse","tool_name":"Write","tool_input":` +
			`{"file_path":"` + order[0] + `","path":"` + order[1] + `"}}`
		if got := RunHook(strings.NewReader(payload)); !got.Block {
			t.Errorf("target order %s first: an un-lintable home must refuse, not pass silently", filepath.Base(order[0]))
		}
	}
}

func TestPostWriteDoesNotInventAWikilinkFailure(t *testing.T) {
	// The fallback linted the note's own subdirectory, so a valid link to a root
	// sibling was reported as missing — a fabricated diagnosis on a valid write.
	home := t.TempDir()
	mem := filepath.Join(home, ".claude", "memory")
	if err := os.MkdirAll(filepath.Join(mem, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(mem, "MEMORY.md"), "# Memory Index\n")
	write(filepath.Join(mem, "project-root.md"), "---\nname: project-root\ndescription: Use when root.\ntype: project\nstatus: active\n---\n")
	nested := filepath.Join(mem, "inbox", "project-sub.md")
	write(nested, "---\nname: project-sub\ndescription: Use when sub.\ntype: project\nstatus: active\n---\n\nSee [[project-root]].\n")
	locked := filepath.Join(mem, "project-locked.md")
	write(locked, "---\nname: project-locked\n---\n")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Skip("cannot make a file unreadable here")
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })

	payload := `{"hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"file_path":"` + nested + `"}}`
	got := RunHook(strings.NewReader(payload))
	if !got.Block {
		t.Fatal("an un-lintable home must refuse")
	}
	if strings.Contains(got.Message, "M006") || strings.Contains(got.Message, "wikilink") {
		t.Errorf("refusal must name the real cause, not invent a link failure: %s", got.Message)
	}
	if !strings.Contains(got.Message, "could not validate") {
		t.Errorf("refusal must say why: %s", got.Message)
	}
}

func TestRefsSeesSubdirectoryNotesAndConfiguredTypes(t *testing.T) {
	// refs is the only walker that was flat, so a valid reference to a
	// subdirectory note was reported as pointing at a note that does not exist —
	// a false failure in the CI lane that runs it.
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, "inbox"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".memorylint.yaml"),
		[]byte("version: 1\nallowed_types: [user, feedback, project, reference, runbook]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "inbox", "project-sub.md"),
		[]byte("---\nname: project-sub\ndescription: Use when sub.\ntype: project\nstatus: active\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "runbook-deploy.md"),
		[]byte("---\nname: runbook-deploy\ndescription: Use when deploying.\ntype: runbook\nstatus: active\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(t.TempDir(), "guide.md")
	body := "See `notes/memory/project-sub.md` and `notes/memory/runbook-deploy.md`.\n" +
		"Also see project-sub.md and runbook-deploy.md.\n" +
		"But " + goneFixture + " is gone.\n"
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Refs(home, []string{src}, RefOptions{Prefix: "notes/memory", Bare: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range report.Findings {
		if strings.Contains(f.Message, "project-sub") {
			t.Errorf("a subdirectory note must resolve: %s", f.Message)
		}
		if strings.Contains(f.Message, "runbook-deploy") {
			t.Errorf("a configured-type note must resolve: %s", f.Message)
		}
	}
	var sawGone bool
	for _, f := range report.Findings {
		if strings.Contains(f.Message, "gone_from_the_home") {
			sawGone = true
		}
	}
	if !sawGone {
		t.Errorf("a genuinely missing note must still be reported: %#v", report.Findings)
	}
}

func TestNewNoteProvisionalIsBornWithExpires(t *testing.T) {
	home := t.TempDir()
	path, err := NewNote(home, "project", "project-fresh-lesson", "Use when x.", true)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "status: provisional") {
		t.Errorf("a --provisional note must be born provisional: %q", raw)
	}
	want := time.Now().AddDate(0, 0, 60).Format("2006-01-02")
	if !strings.Contains(string(raw), want) {
		t.Errorf("expires must be today+60d (%s): %q", want, raw)
	}
	if err := os.WriteFile(filepath.Join(home, "MEMORY.md"),
		[]byte("# Memory Index\n\n- [project-fresh-lesson](project-fresh-lesson.md) — Use when x.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Lint([]string{home})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Findings) != 0 {
		t.Errorf("a scaffolded provisional note must lint clean: %#v", report.Findings)
	}
}

func TestFixCarriesTheLifecycleFieldsThrough(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "project-lesson.md")
	if err := os.WriteFile(path, []byte("---\nname: project-lesson\ndescription: Use when x.\ntype: project\nstatus: provisional\nexpires: 2999-01-02\nlast-used: 2999-01-03\n---\n\nBody.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rendered, _, err := Normalize(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), "status: provisional") {
		t.Errorf("fix must not strip a provisional status: %q", rendered)
	}
	if got := strings.Count(string(rendered), "expires:"); got != 1 {
		t.Errorf("fix must carry expires through exactly once, got %d: %q", got, rendered)
	}
	if !strings.Contains(string(rendered), "2999-01-02") {
		t.Errorf("fix must keep the expires date: %q", rendered)
	}
	if got := strings.Count(string(rendered), "last-used:"); got != 1 {
		t.Errorf("fix must carry last-used through exactly once, got %d: %q", got, rendered)
	}
	if !strings.Contains(string(rendered), "2999-01-03") {
		t.Errorf("fix must keep the last-used date: %q", rendered)
	}
}

func TestPostWriteAcceptsProvisionalNotes(t *testing.T) {
	// A live provisional is a valid note, and an EXPIRED one is only an M013
	// warning — deletable on sight is prune's job, not a reason to block a write.
	home := t.TempDir()
	mem := filepath.Join(home, ".claude", "memory")
	if err := os.MkdirAll(mem, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, body string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(mem, "MEMORY.md"),
		"# Memory Index\n\n- [project-live](project-live.md) — Use when x.\n- [project-expired](project-expired.md) — Use when x.\n")
	live := filepath.Join(mem, "project-live.md")
	write(live, "---\nname: project-live\ndescription: Use when x.\ntype: project\nstatus: provisional\nexpires: 2999-01-02\n---\n\nBody.\n")
	expired := filepath.Join(mem, "project-expired.md")
	write(expired, "---\nname: project-expired\ndescription: Use when x.\ntype: project\nstatus: provisional\nexpires: 2001-01-02\n---\n\nBody.\n")

	for _, target := range []string{live, expired} {
		payload := `{"hook_event_name":"PostToolUse","tool_name":"Write","tool_input":{"file_path":"` + target + `"}}`
		if got := RunHook(strings.NewReader(payload)); got.Block {
			t.Errorf("a provisional note must not be refused (%s): %s", filepath.Base(target), got.Message)
		}
	}
}
