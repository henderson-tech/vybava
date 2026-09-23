package claudeguards

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// fullSettings renders a settings.json carrying every manifest entry, minus
// the verbs listed in drop, with the given hook binary path.
func fullSettings(t *testing.T, bin string, drop ...string) string {
	t.Helper()
	dropped := map[string]bool{}
	for _, d := range drop {
		dropped[d] = true
	}
	groups := map[string][]map[string]any{}
	for _, w := range Hooks {
		if dropped[w.Verb()] {
			continue
		}
		var g map[string]any
		for _, existing := range groups[w.Event] {
			matcher, _ := existing["matcher"].(string)
			if matcher == w.Matcher {
				g = existing
			}
		}
		if g == nil {
			g = map[string]any{"hooks": []any{}}
			if w.Matcher != "" {
				g["matcher"] = w.Matcher
			}
			groups[w.Event] = append(groups[w.Event], g)
		}
		g["hooks"] = append(g["hooks"].([]any), map[string]any{"type": "command", "command": bin + " " + w.Verb(), "timeout": 7})
	}
	// An unrelated entry in the Bash group must survive untouched.
	groups["PreToolUse"] = append(groups["PreToolUse"], map[string]any{"matcher": "Write|Edit", "hooks": []any{map[string]any{"type": "command", "command": "memorylint hook"}}})
	doc := map[string]any{
		"permissions": map[string]any{"allow": []any{"Bash(ls:*)"}, "deny": []any{}},
		"env":         map[string]any{"FOO": "bar"},
		"model":       "opus",
		"hooks":       groups,
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func verbs(ws []HookWiring) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w.Verb())
	}
	return out
}

func TestMissingHooksAllPresent(t *testing.T) {
	for _, bin := range []string{"~/.claude/hooks/claude-guards", "~/.local/bin/claude-guards", "/Users/x/.local/bin/claude-guards"} {
		missing, err := MissingHooks(fullSettings(t, bin))
		if err != nil {
			t.Fatal(err)
		}
		if len(missing) != 0 {
			t.Errorf("%s: expected nothing missing, got %v", bin, verbs(missing))
		}
	}
}

func TestMissingHooksReportsDropped(t *testing.T) {
	path := fullSettings(t, "~/.claude/hooks/claude-guards", "bash", "weather --reap", "reap")
	missing, err := MissingHooks(path)
	if err != nil {
		t.Fatal(err)
	}
	got := verbs(missing)
	// The flags are part of the verb: `weather --reap` and `reap` are distinct.
	want := []string{"bash", "weather --reap", "reap"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("missing = %v, want %v", got, want)
	}
}

func TestMissingHooksNoHooksKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"model":"opus"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	missing, err := MissingHooks(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != len(Hooks) {
		t.Errorf("expected all %d missing, got %d", len(Hooks), len(missing))
	}
}

func TestDoctorReportsWithoutFix(t *testing.T) {
	path := fullSettings(t, "~/.claude/hooks/claude-guards", "read")
	before, _ := os.ReadFile(path)
	var out, errOut bytes.Buffer
	if err := Doctor(path, false, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "1 hook(s) missing") || !strings.Contains(out.String(), "PreToolUse/Read → ~/.claude/hooks/claude-guards read") {
		t.Errorf("unexpected report:\n%s", out.String())
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Error("doctor without --fix must not write the file")
	}
}

func TestDoctorFixRoundTrips(t *testing.T) {
	path := fullSettings(t, "~/.claude/hooks/claude-guards", "bash", "doctor --fix", "browser-teardown")
	var beforeDoc map[string]json.RawMessage
	raw, _ := os.ReadFile(path)
	if err := json.Unmarshal(raw, &beforeDoc); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if err := Doctor(path, true, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if errOut.Len() != 0 {
		t.Errorf("unexpected stderr: %s", errOut.String())
	}
	if !strings.Contains(out.String(), "re-inserted 3 missing hook(s)") || !strings.Contains(out.String(), "git -C") {
		t.Errorf("unexpected fix report:\n%s", out.String())
	}

	missing, err := MissingHooks(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Errorf("second run still missing %v", verbs(missing))
	}

	raw, _ = os.ReadFile(path)
	var afterDoc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &afterDoc); err != nil {
		t.Fatalf("rewritten file is not JSON: %v\n%s", err, raw)
	}
	for _, key := range []string{"permissions", "env", "model"} {
		var a, b any
		_ = json.Unmarshal(beforeDoc[key], &a)
		_ = json.Unmarshal(afterDoc[key], &b)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("%s changed: %s → %s", key, beforeDoc[key], afterDoc[key])
		}
	}
	// The bash hook joined the existing Bash group rather than opening a new one,
	// and the unrelated Write|Edit group is still there.
	var groups map[string][]hookGroup
	_ = json.Unmarshal(afterDoc["hooks"], &groups)
	bashGroups, writeGroups := 0, 0
	for _, g := range groups["PreToolUse"] {
		switch g.Matcher {
		case "Bash":
			bashGroups++
		case "Write|Edit":
			writeGroups++
		}
	}
	if bashGroups != 1 || writeGroups != 1 {
		t.Errorf("PreToolUse groups: Bash=%d Write|Edit=%d, want 1/1", bashGroups, writeGroups)
	}
	if len(groups["SessionEnd"]) != 1 || len(groups["SessionEnd"][0].Hooks) != 3 {
		t.Errorf("SessionEnd should be one group of 3 hooks: %+v", groups["SessionEnd"])
	}
	if !strings.HasSuffix(string(raw), "}\n") || !strings.Contains(string(raw), "\n  \"hooks\": {") {
		t.Errorf("file should be 2-space indented with a trailing newline:\n%s", raw)
	}
}

func TestDoctorFailsOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if err := Doctor(path, true, &out, &errOut); err != nil {
		t.Errorf("malformed settings must not fail the session: %v", err)
	}
	if out.Len() != 0 || !strings.Contains(errOut.String(), "cannot check hooks") {
		t.Errorf("stdout=%q stderr=%q", out.String(), errOut.String())
	}
	if err := Doctor(filepath.Join(t.TempDir(), "absent.json"), false, &out, &errOut); err != nil {
		t.Errorf("absent settings must not fail the session: %v", err)
	}
}

// A settings.json wired by the older manifest (separate SessionStart weather
// and reap) is upgraded in place: the retired entries go, their successor
// comes in, and SessionEnd's reap stays.
func TestDoctorFixRetiresSupersededHooks(t *testing.T) {
	path := fullSettings(t, hookBin, "weather --reap")
	top, groups, err := readSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range retiredHooks {
		groups[w.Event] = insertHook(groups[w.Event], w)
	}
	if err := writeSettings(path, top, groups); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if err := Doctor(path, true, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"rewired", "− SessionStart → " + hookBin + " reap", "+ SessionStart → " + hookBin + " weather --reap"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("fix report lacks %q:\n%s", want, out.String())
		}
	}
	_, groups, err = readSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if m, r := missingHooks(groups), retiredWired(groups); len(m) != 0 || len(r) != 0 {
		t.Errorf("after fix: missing %v, retired %v", verbs(m), verbs(r))
	}
}

func TestCheckHooks(t *testing.T) {
	if _, err := CheckHooks(fullSettings(t, "~/.claude/hooks/claude-guards")); err != nil {
		t.Errorf("complete file: %v", err)
	}
	missing, err := CheckHooks(fullSettings(t, "~/.claude/hooks/claude-guards", "browser"))
	if err != ErrHooksMissing || len(missing) != 1 {
		t.Errorf("got %v, %v", verbs(missing), err)
	}
}
