package claudeguards

// Hook wiring manifest and its self-check — SessionStart hook.
//
// On 2026-09-18 22:26 Claude Code rewrote ~/.claude/settings.json outside any
// tool call and dropped 89 lines, among them the claude-guards PreToolUse
// hooks. Every session ran unguarded for 36 h, which is how a whole-disk crawl
// and a per-look Appium loop went through and pushed the Mac to load 680. The
// guards can only hold if something checks that they are still wired, so the
// wiring lives here as data and `claude-guards doctor` diffs the live file
// against it on every SessionStart. `--fix` re-inserts what is missing as a
// surgical merge: only the `hooks` key is re-marshalled, every other top-level
// key is written back from its raw bytes.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// HookWiring is one settings.json hook entry claude-guards expects to exist.
// Matcher is empty for lifecycle events (SessionStart, SessionEnd).
type HookWiring struct {
	Event   string `json:"event"`
	Matcher string `json:"matcher,omitempty"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// hookBin is the path every wired command starts with. An older
// ~/.local/bin/claude-guards path counts as present too — the match is on
// the verb after `claude-guards `, never on the path.
const hookBin = "~/.claude/hooks/claude-guards"

const browserMatcher = "mcp__playwright__.*|mcp__plugin_chrome-devtools-mcp_chrome-devtools__.*"

// Hooks is the manifest: the wiring documented in docs/claude-guards.md and
// in the CLI's Long help, as data.
var Hooks = []HookWiring{
	{Event: "PreToolUse", Matcher: "Bash", Command: hookBin + " bash", Timeout: 10},
	{Event: "PreToolUse", Matcher: "Read", Command: hookBin + " read", Timeout: 5},
	{Event: "PreToolUse", Matcher: browserMatcher, Command: hookBin + " browser"},
	{Event: "SessionStart", Command: hookBin + " doctor --fix", Timeout: 10},
	{Event: "SessionStart", Command: hookBin + " weather --reap", Timeout: 20},
	{Event: "SessionStart", Command: hookBin + " swarm-teardown --dead-only", Timeout: 20},
	{Event: "SessionEnd", Command: hookBin + " swarm-teardown", Timeout: 20},
	{Event: "SessionEnd", Command: hookBin + " browser-teardown", Timeout: 10},
	{Event: "SessionEnd", Command: hookBin + " reap", Timeout: 20},
}

// retiredHooks are wirings an older manifest installed; `doctor --fix`
// removes them. SessionStart ran `weather` and `reap` as two processes, each
// paying its own `ps -axo` (~0.5 s under load) for the same table —
// `weather --reap` reads it once.
var retiredHooks = []HookWiring{
	{Event: "SessionStart", Command: hookBin + " weather"},
	{Event: "SessionStart", Command: hookBin + " reap"},
}

// Verb returns the part of the command after `claude-guards ` — the identity
// a live entry is matched on.
func (w HookWiring) Verb() string { return hookVerb(w.Command) }

func hookVerb(command string) string {
	command = strings.TrimSpace(command)
	i := strings.Index(command, "claude-guards ")
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(command[i+len("claude-guards "):])
}

// hookGroup is one matcher group as settings.json stores it.
type hookGroup struct {
	Matcher string            `json:"matcher,omitempty"`
	Hooks   []json.RawMessage `json:"hooks"`
}

type hookEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// DefaultSettingsPath is ~/.claude/settings.json.
func DefaultSettingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// MissingHooks reads settings.json and returns every manifest entry with no
// live counterpart (same event, same matcher, same verb).
func MissingHooks(settingsPath string) ([]HookWiring, error) {
	_, groups, err := readSettings(settingsPath)
	if err != nil {
		return nil, err
	}
	return missingHooks(groups), nil
}

func readSettings(path string) (map[string]json.RawMessage, map[string][]hookGroup, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	groups := map[string][]hookGroup{}
	if h, ok := top["hooks"]; ok && len(h) > 0 {
		if err := json.Unmarshal(h, &groups); err != nil {
			return nil, nil, fmt.Errorf("%s: hooks: %w", path, err)
		}
	}
	return top, groups, nil
}

func missingHooks(groups map[string][]hookGroup) []HookWiring {
	var out []HookWiring
	for _, w := range Hooks {
		if !hookPresent(groups, w) {
			out = append(out, w)
		}
	}
	return out
}

func hookPresent(groups map[string][]hookGroup, w HookWiring) bool {
	for _, g := range groups[w.Event] {
		if g.Matcher != w.Matcher {
			continue
		}
		for _, raw := range g.Hooks {
			var e hookEntry
			if json.Unmarshal(raw, &e) == nil && hookVerb(e.Command) == w.Verb() {
				return true
			}
		}
	}
	return false
}

// Doctor is the hook entry point. It never fails the session: an unreadable
// or malformed file is one warning on stderr and a nil return. Missing hooks
// are printed to stdout — at SessionStart that text enters the model's
// context, so the session itself learns it is running unguarded.
func Doctor(settingsPath string, fix bool, stdout, stderr io.Writer) error {
	if settingsPath == "" {
		p, err := DefaultSettingsPath()
		if err != nil {
			fmt.Fprintf(stderr, "claude-guards doctor: %v\n", err)
			return nil
		}
		settingsPath = p
	}
	top, groups, err := readSettings(settingsPath)
	if err != nil {
		fmt.Fprintf(stderr, "claude-guards doctor: cannot check hooks: %v\n", err)
		return nil
	}
	missing, retired := missingHooks(groups), retiredWired(groups)
	if len(missing) == 0 && len(retired) == 0 {
		return nil
	}
	if !fix {
		if len(missing) > 0 {
			fmt.Fprintf(stdout, "🚨 claude-guards: %d hook(s) missing from %s — this session runs partly unguarded. Fix: claude-guards doctor --fix\n", len(missing), settingsPath)
			for _, w := range missing {
				fmt.Fprintf(stdout, "   %s\n", w.describe())
			}
		}
		if len(retired) > 0 {
			fmt.Fprintf(stdout, "ℹ️ claude-guards: %d retired hook(s) still wired in %s. Fix: claude-guards doctor --fix\n", len(retired), settingsPath)
			for _, w := range retired {
				fmt.Fprintf(stdout, "   %s\n", w.describe())
			}
		}
		return nil
	}
	for _, w := range retired {
		groups[w.Event] = removeHook(groups[w.Event], w)
	}
	for _, w := range missing {
		groups[w.Event] = insertHook(groups[w.Event], w)
	}
	if err := writeSettings(settingsPath, top, groups); err != nil {
		fmt.Fprintf(stderr, "claude-guards doctor: could not rewrite hooks: %v\n", err)
		if len(missing) > 0 {
			fmt.Fprintf(stdout, "🚨 claude-guards: %d hook(s) missing from %s and the fix failed: %v\n", len(missing), settingsPath, err)
		}
		return nil
	}
	if len(retired) > 0 {
		// A manifest upgrade, not a harness rewrite: the retired entries' successors are the missing ones.
		fmt.Fprintf(stdout, "ℹ️ claude-guards: rewired %s to the current hook manifest (restart the session to load it):\n", settingsPath)
		for _, w := range retired {
			fmt.Fprintf(stdout, "   − %s\n", w.describe())
		}
		for _, w := range missing {
			fmt.Fprintf(stdout, "   + %s\n", w.describe())
		}
	} else {
		fmt.Fprintf(stdout, "🚨 claude-guards: re-inserted %d missing hook(s) into %s (the file had been rewritten; restart the session to load them):\n", len(missing), settingsPath)
		for _, w := range missing {
			fmt.Fprintf(stdout, "   %s\n", w.describe())
		}
	}
	fmt.Fprintf(stdout, "   settings.json is git-tracked: review with `git -C %s diff settings.json`\n", filepath.Dir(settingsPath))
	return nil
}

// retiredWired returns the retired wirings the live file still carries.
func retiredWired(groups map[string][]hookGroup) []HookWiring {
	var out []HookWiring
	for _, w := range retiredHooks {
		if hookPresent(groups, w) {
			out = append(out, w)
		}
	}
	return out
}

func (w HookWiring) describe() string {
	if w.Matcher == "" {
		return w.Event + " → " + w.Command
	}
	return w.Event + "/" + w.Matcher + " → " + w.Command
}

// insertHook appends the entry to the group with the same matcher, or opens
// a new group when none exists.
func insertHook(groups []hookGroup, w HookWiring) []hookGroup {
	entry, _ := json.Marshal(hookEntry{Type: "command", Command: w.Command, Timeout: w.Timeout})
	for i := range groups {
		if groups[i].Matcher == w.Matcher {
			groups[i].Hooks = append(groups[i].Hooks, entry)
			return groups
		}
	}
	return append(groups, hookGroup{Matcher: w.Matcher, Hooks: []json.RawMessage{entry}})
}

// removeHook drops every entry with w's matcher and verb; a group left empty
// goes with it.
func removeHook(groups []hookGroup, w HookWiring) []hookGroup {
	var out []hookGroup
	for _, g := range groups {
		if g.Matcher == w.Matcher {
			var kept []json.RawMessage
			for _, raw := range g.Hooks {
				var e hookEntry
				if json.Unmarshal(raw, &e) == nil && hookVerb(e.Command) == w.Verb() {
					continue
				}
				kept = append(kept, raw)
			}
			if len(kept) == 0 {
				continue
			}
			g.Hooks = kept
		}
		out = append(out, g)
	}
	return out
}

// writeSettings re-marshals only the hooks key; every other key is written
// from its raw bytes, in sorted key order (Claude Code sorts on its own
// rewrites, so this keeps diffs small).
func writeSettings(path string, top map[string]json.RawMessage, groups map[string][]hookGroup) error {
	hooks, err := json.Marshal(groups)
	if err != nil {
		return err
	}
	top["hooks"] = hooks
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{\n")
	for i, k := range keys {
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, top[k], "  ", "  "); err != nil {
			return fmt.Errorf("%s: %w", k, err)
		}
		key, _ := json.Marshal(k)
		fmt.Fprintf(&b, "  %s: %s", key, pretty.String())
		if i < len(keys)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n")
	tmp := path + ".claude-guards.tmp"
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ErrHooksMissing is what callers that want a failing status (vybava doctor)
// get from CheckHooks.
var ErrHooksMissing = errors.New("claude-guards hooks missing from settings.json")

// CheckHooks is the read-only form for other tools: the missing entries and
// ErrHooksMissing when there are any.
func CheckHooks(settingsPath string) ([]HookWiring, error) {
	if settingsPath == "" {
		p, err := DefaultSettingsPath()
		if err != nil {
			return nil, err
		}
		settingsPath = p
	}
	missing, err := MissingHooks(settingsPath)
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		return missing, ErrHooksMissing
	}
	return nil, nil
}
