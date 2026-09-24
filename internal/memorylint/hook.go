package memorylint

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// HookDecision is what a hook invocation concluded, independent of how the
// caller chooses to signal it. Exit-code mapping lives in the CLI.
type HookDecision struct {
	Block   bool
	Message string
}

// HookPayload is the subset of a Claude Code / Codex hook event this tool reads.
type HookPayload struct {
	HookEventName string `json:"hook_event_name"`
	ToolName      string `json:"tool_name"`
	Cwd           string `json:"cwd"`
	ToolInput     struct {
		FilePath  string `json:"file_path"`
		Path      string `json:"path"`
		Content   string `json:"content"`
		NewString string `json:"new_string"`
		Command   string `json:"command"`
	} `json:"tool_input"`
}

var patchPathPattern = regexp.MustCompile(`(?m)^\*\*\* (?:Add|Update|Delete) File: (.+\.md)\s*$`)

// managedHomePattern matches the agent-managed memory home. Claude Code owns
// this directory and rewrites frontmatter it writes there — see RefuseManaged.
var managedHomePattern = regexp.MustCompile(`/\.claude/projects/[^/]+/memory(/|$)`)

// rewritingTools are the tools whose writes Claude Code post-processes. Bash and
// Codex's apply_patch write the bytes given to them.
var rewritingTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}

func memoryPath(path, cwd string) (string, bool) {
	path = strings.TrimSpace(path)
	if path == "" || !strings.HasSuffix(strings.ToLower(path), ".md") {
		return "", false
	}
	if !filepath.IsAbs(path) && cwd != "" {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	slash := filepath.ToSlash(path)
	if IsHandoffHome(slash) {
		return path, true
	}
	if !strings.Contains(slash, "/memory/") && !strings.HasSuffix(slash, "/memory") {
		return "", false
	}
	if !insideMemoryHome(slash) {
		return "", false
	}
	return path, true
}

// canonicalHome matches the homes the memory doctrine defines: the committed
// team home `<repo>/.claude/memory`, the agent-managed personal home
// `~/.claude/projects/<slug>/memory`, and the Codex equivalents that Discover
// already recognises — including anything nested under any of them.
//
// Both agents matter here: this binary is the hook for Claude Code AND Codex, so
// leaving `.codex` out would mean an unindexed Codex home's first note is never
// scanned and a secret write goes through.
var canonicalHome = regexp.MustCompile(`/\.[cC]odex/(?:projects/[^/]+/)?memory(?:/|$)|/\.claude/(?:projects/[^/]+/)?memory(?:/|$)`)

// insideMemoryHome decides whether a path under some directory called `memory`
// is really a note.
//
// A directory merely NAMED memory/ is not a home: this hook is registered
// globally, and an ordinary `docs/memory/_index.md` in an unrelated repo was
// being refused with exit 2. But the converse trap is worse — keying on "the
// note's own directory contains MEMORY.md" silently disabled the hook for every
// note in a subdirectory (`.claude/memory/inbox/` exists in FixIt's team home),
// for a home not yet indexed, and for the very first note that creates one.
// Secrets sailed through exactly where the guard was most needed.
//
// So: a canonical home always counts, indexed or not, at any depth; any other
// `memory/` directory counts only if it, or an ancestor up to and including the
// directory named `memory`, actually carries a MEMORY.md.
func insideMemoryHome(slash string) bool {
	if canonicalHome.MatchString(slash) {
		return true
	}
	dir := filepath.FromSlash(slash)
	for {
		dir = filepath.Dir(dir)
		if dir == "" || dir == "." || dir == string(filepath.Separator) {
			return false
		}
		if _, err := os.Stat(filepath.Join(dir, "MEMORY.md")); err == nil {
			return true
		}
		if filepath.Base(dir) == "memory" {
			return false
		}
	}
}

// HookTargets returns the memory notes a hook payload is about to write.
func HookTargets(p HookPayload) []string {
	seen := map[string]bool{}
	var out []string
	for _, candidate := range []string{p.ToolInput.FilePath, p.ToolInput.Path} {
		if path, ok := memoryPath(candidate, p.Cwd); ok && !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	for _, m := range patchPathPattern.FindAllStringSubmatch(p.ToolInput.Command, -1) {
		if path, ok := memoryPath(m[1], p.Cwd); ok && !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	return out
}

// addedPatchText keeps only the lines a Codex patch ADDS, so a secret being
// deleted is never reported as a secret being introduced.
func addedPatchText(command string) string {
	var added []string
	for _, line := range strings.Split(command, "\n") {
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			added = append(added, strings.TrimPrefix(line, "+"))
		}
	}
	return strings.Join(added, "\n")
}

// RefuseManaged reports whether a tool write must be refused outright because
// the harness would rewrite the file afterwards.
//
// Claude Code owns ~/.claude/projects/<slug>/memory/ and normalizes frontmatter
// on every Edit/Write it performs there, converting the flat v2 properties this
// linter requires back into a nested `metadata:` envelope and stamping
// originSessionId/modified. That rewrite lands AFTER the post-write hook, so no
// hook can detect it and no `check` run in the same session will see it. The
// only reliable defence is to refuse the tool and send the caller to a writer
// the harness leaves alone.
//
// Verified 2026-08-22: an identical note edited outside that directory is
// untouched, and the same edit made with Bash preserves the frontmatter exactly.
func RefuseManaged(p HookPayload, targets []string) (HookDecision, bool) {
	if !rewritingTools[p.ToolName] {
		return HookDecision{}, false
	}
	for _, path := range targets {
		if !managedHomePattern.MatchString(filepath.ToSlash(path)) {
			continue
		}
		return HookDecision{Block: true, Message: fmt.Sprintf(
			"%s rewrites frontmatter in the agent-managed memory home, which silently "+
				"reverts the flat v2 properties to a nested metadata: envelope.\n"+
				"  refused: %s\n"+
				"  write it with Bash instead (heredoc, sed, perl), or scaffold with `memorylint new`,\n"+
				"  then re-run `memorylint check` on the home.", p.ToolName, path)}, true
	}
	return HookDecision{}, false
}

// RunHook reads one hook payload and decides whether the write may proceed.
func RunHook(stdin io.Reader) HookDecision {
	raw, err := io.ReadAll(stdin)
	if err != nil {
		return HookDecision{}
	}
	var p HookPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return HookDecision{}
	}
	targets := HookTargets(p)
	if len(targets) == 0 {
		return HookDecision{}
	}

	if decision, refused := RefuseManaged(p, targets); refused {
		return decision
	}

	// Pre-write: judge the proposed content, which is not on disk yet.
	//
	// Anything that is not explicitly the post-write event is treated as
	// pre-write. Naming the pre-write case instead meant an unrecognised event
	// name fell through to the post-write branch, which lints a file that is not
	// on disk yet, finds nothing, and lets the secret through — a guard failing
	// OPEN on an unknown input.
	if p.HookEventName != "PostToolUse" {
		content := p.ToolInput.Content + "\n" + p.ToolInput.NewString + "\n" + addedPatchText(p.ToolInput.Command)
		// A home's config and .memory-lint-allow live at its ROOT, like the
		// post-write lint below reads them: the note's own directory (a ledger
		// home's notes/) holds neither, so an allowlisted value blocked the write.
		// A patch can touch notes in several homes and its added lines cannot be
		// told apart, so the content is judged once per home and must pass them
		// ALL: one home's allowlist never clears a value for another.
		judged := map[string]bool{}
		for _, target := range targets {
			root, scan := memoryHomeRoot(target), fixtureFindings
			if IsHandoffHome(target) {
				root, scan = handoffHomeRoot(target), secretFindings
			}
			if judged[root] {
				continue
			}
			judged[root] = true
			config, err := loadConfig(root)
			if err != nil {
				config = DefaultConfig()
			}
			if findings := scan(target, []byte(content), config); len(findings) > 0 {
				return HookDecision{Block: true, Message: "blocked write: " + formatFinding(findings[0])}
			}
		}
		return HookDecision{}
	}

	// Post-write: the notes exist, so lint them for real — from the HOME ROOT,
	// not the note's own directory. Linting `inbox/` as if it were a home makes
	// every wikilink to a sibling at the root an unresolved M006, refusing a
	// perfectly valid write.
	//
	// Targets are grouped by home and each home is linted exactly once. If a home
	// cannot be linted at all, the write is REFUSED and the reason is named: this
	// is the only check covering a writer PreToolUse cannot see (a Bash heredoc),
	// so a silent pass here is a secret shipped.
	//
	// An earlier attempt to soften that by falling back to the note's own
	// directory was worse on both counts. It re-created the M006 false-block it
	// was meant to remove — reporting "wikilink target does not exist" for a
	// target that exists one directory up — and because the fallback only ever
	// covered the FIRST target, the remaining notes in that home were silently
	// skipped, so whether a secret shipped depended on patch hunk order.
	//
	// MEMORY.md is excluded here, once, when the groups are built: a payload that
	// touches the index alongside a note must not be refused over index defects
	// that predate the write.
	var messages []string
	byRoot := map[string][]string{}
	var roots []string
	for _, path := range targets {
		if filepath.Base(path) == "MEMORY.md" {
			continue
		}
		root := memoryHomeRoot(path)
		if IsHandoffHome(path) {
			root = handoffHomeRoot(path)
		}
		if _, seen := byRoot[root]; !seen {
			roots = append(roots, root)
		}
		byRoot[root] = append(byRoot[root], path)
	}
	for _, root := range roots {
		report, err := Lint([]string{root})
		if err != nil {
			messages = append(messages, fmt.Sprintf("could not validate %s: %v", root, err))
			continue
		}
		for _, f := range report.Findings {
			if f.Severity != SeverityError {
				continue
			}
			for _, target := range byRoot[root] {
				if sameFile(f.Path, target) {
					messages = append(messages, formatFinding(f))
				}
			}
		}
	}
	if len(messages) > 0 {
		return HookDecision{Block: true, Message: strings.Join(messages, "\n")}
	}
	return HookDecision{}
}

// memoryHomeRoot returns the directory a note's home is rooted at — the
// `memory` directory itself, not whatever subdirectory the note happens to sit
// in.
func memoryHomeRoot(path string) string {
	dir := filepath.Dir(path)
	for cur := dir; ; {
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		if filepath.Base(cur) == "memory" {
			return cur
		}
		if _, err := os.Stat(filepath.Join(cur, "MEMORY.md")); err == nil {
			return cur
		}
		cur = parent
	}
	return dir
}

func sameFile(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

func formatFinding(f Finding) string {
	if f.Line > 0 {
		return fmt.Sprintf("%s %s:%d [%s] %s", strings.ToUpper(string(f.Severity)), f.Path, f.Line, f.Rule, f.Message)
	}
	return fmt.Sprintf("%s %s [%s] %s", strings.ToUpper(string(f.Severity)), f.Path, f.Rule, f.Message)
}
