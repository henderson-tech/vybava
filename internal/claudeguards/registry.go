package claudeguards

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
)

// Rule is one row of the registry: the closed id every deny() site uses, the
// family it belongs to, the hook event that evaluates it, a one-line reason
// and the escape hatch (an env var, or "none" when the sanctioned form is
// cheaper than the blocked one). The registry is the single source for
// `claude-guards list`, the docs' family table and the drift test that keeps
// every deny() literal accounted for.
type Rule struct {
	ID      string `json:"id"`
	Family  string `json:"family"`
	Event   string `json:"event"`
	Summary string `json:"summary"`
	Escape  string `json:"escape"`
}

const (
	eventBash    = "PreToolUse:Bash"
	eventRead    = "PreToolUse:Read"
	eventBrowser = "PreToolUse:browser"
	eventBoth    = "PreToolUse:Bash|Read"

	escapeNone       = "none"
	escapeDangerous  = "CLAUDE_ALLOW_DANGEROUS=1"
	escapeDump       = "CLAUDE_ALLOW_CONTEXT_DUMP=1"
	escapeShellEdit  = "CLAUDE_ALLOW_SHELL_EDIT=1"
	escapeCommit     = "COMMIT_GUARD_ALLOW=1"
	escapeWorkers    = "CLAUDE_GUARDS_ALLOW_TEST_WORKERS=1"
	escapeLocalStack = "CLAUDE_GUARDS_ALLOW_LOCAL_STACK=1"
	escapeMachineCap = "CLAUDE_GUARDS_ALLOW_MACHINE_CAP=1"
)

// Rules is every rule the package can deny with, ordered by family then id.
var Rules = []Rule{
	{"browser:onyx-first", "browser", eventBrowser, "a playwright/chrome-devtools call while this session has no Onyx browser", escapeNone},
	{"browser:screenshot-dir", "browser", eventBrowser, "a browser screenshot written anywhere but .vitrinka/mcp/", escapeNone},
	{"commit-secrets", "commit-secrets", eventBash, "git commit when any change it could take (staged, unstaged, or untracked with git add) holds key files, secret-shaped lines or private infra strings", escapeCommit},
	{"context:budget-read", "context", eventBoth, "a read above the remaining context budget", escapeDump},
	{"context:heredoc-overwrite", "context", eventBash, "cat/tee heredoc over an existing file instead of Edit", escapeShellEdit},
	{"context:inline-script-write", "context", eventBash, "an inline python/node script that writes files", escapeShellEdit},
	{"context:locale-catalog", "context", eventBoth, "a raw read of a lok locale catalog; use lok get/grep/add", escapeDump},
	{"context:no-read", "context", eventBoth, "a raw read of a generated megafile listed in guards.noRead", escapeDump},
	{"context:root-walk", "context", eventBash, "find/bfs/fd rooted at /, ~ or /Users without -maxdepth", escapeNone},
	{"context:transcript-dump", "context", eventBoth, "dumping a ~/.claude/projects transcript", escapeDump},
	{"context:unbounded-output", "context", eventBash, "docker logs, git log/diff/show or a listed command with no cap or filter", escapeDump},
	{"context:whole-file-dump", "context", eventBash, "cat/sed/head/tail above guards.maxDumpLines with no range or reducing pipe", escapeDump},
	{"destructive:compose-down-volumes", "destructive", eventBash, "docker compose down -v outside a named worktree stack", escapeDangerous},
	{"destructive:git-restore-dot", "destructive", eventBash, "git restore . in the primary clone", escapeDangerous},
	{"destructive:git-stash", "destructive", eventBash, "git stash in a tree shared by parallel sessions", escapeDangerous},
	{"destructive:git-switch", "destructive", eventBash, "git checkout/switch of the primary clone's branch", escapeDangerous},
	{"destructive:keychain-secret-dump", "destructive", eventBash, "security … -w keychain value read", escapeDangerous},
	{"destructive:system-prune-volumes", "destructive", eventBash, "docker system prune --volumes", escapeDangerous},
	{"destructive:volume-prune", "destructive", eventBash, "docker volume prune", escapeDangerous},
	{"destructive:volume-rm-db", "destructive", eventBash, "docker volume rm of a database volume", escapeDangerous},
	{"e2e:raw-png-read", "e2e", eventRead, "Read of a raw PNG under .e2e/; snap makes a JPEG first", escapeNone},
	{"e2e:raw-screenshot", "e2e", eventBash, "raw xcrun simctl screenshot inside /e2e; use snap", escapeNone},
	{"e2e:screencapture", "e2e", eventBash, "screencapture inside /e2e; use snap", escapeNone},
	{"machine:dev-server-cap", "machine", eventBash, "a Metro/next/API dev server start while guards.devServerCap already run", escapeMachineCap},
	{"machine:devbox-only", "machine", eventBash, "a command a repo's guards.devboxOnly routes to the Devbox ran locally", escapeLocalStack},
	{"machine:devbox-workspace", "machine", eventBash, "a command a repo's guards.devboxWhenWorkspace routes to the Devbox ran locally in a checkout that has a workspace", escapeLocalStack},
	{"machine:sim-cap", "machine", eventBash, "a simulator boot while guards.simCap simulators are already booted", escapeMachineCap},
	{"machine:test-worker-cap", "machine", eventBash, "playwright/vitest/jest on this Mac with no worker cap or one above guards.testWorkerCap", escapeWorkers},
	{"plugincache:package-install", "plugincache", eventBash, "a package install targeting ~/.claude/plugins/cache", escapeNone},
	{"secrets:env-dump", "secrets", eventBash, "env/printenv/export with no name-only projection", escapeDangerous},
	{"secrets:inspect-config-env", "secrets", eventBash, "docker inspect templating .Config.Env", escapeDangerous},
	{"secrets:proc-environ", "secrets", eventBash, "a read of /proc/*/environ", escapeDangerous},
	{"simulator:appium-session-churn", "simulator", eventBash, "a script that opens and deletes a webdriverio session per look", escapeNone},
	{"simulator:host-input", "simulator", eventBash, "cliclick/System Events driving the Simulator window", escapeNone},
}

// Families lists the known family names, sorted.
func Families() []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range Rules {
		if !seen[r.Family] {
			seen[r.Family] = true
			out = append(out, r.Family)
		}
	}
	sort.Strings(out)
	return out
}

// RenderRules writes the registry, filtered to one family when given, as an
// aligned table or as JSON. An unknown family is an error naming the known ones.
func RenderRules(w io.Writer, family string, asJSON bool) error {
	var rows []Rule
	for _, r := range Rules {
		if family == "" || r.Family == family {
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		return fmt.Errorf("unknown family %q; known: %s", family, strings.Join(Families(), ", "))
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RULE\tEVENT\tBLOCKS\tESCAPE")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.ID, r.Event, r.Summary, r.Escape)
	}
	return tw.Flush()
}
