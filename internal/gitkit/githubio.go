package gitkit

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// github-io — mechanical GitHub I/O the skills call:
// `github-io <subcommand> --key value ...`.

const resolveThreadMutation = "mutation($threadId:ID!){ resolveReviewThread(input:{threadId:$threadId}){ thread { isResolved } } }"

// githubIOUsage answers a bare or help invocation: running the verb bare is
// how a caller discovers its interface.
const githubIOUsage = `github-io — mechanical GitHub I/O for the git skills.

Usage: node github-io.ts <subcommand> --key value ...

  reply             --owner O --repo R --pr N --commentId ID --body TEXT|--body-file F
  comment           --owner O --repo R --pr N --body TEXT|--body-file F
  react             --owner O --repo R --commentId ID [--content +1]
  review            --owner O --repo R --pr N --event request-changes|comment --body TEXT|--body-file F
  resolve-thread    --threadId PRRT_...
  create-pr         --head BRANCH --base BRANCH [--title T] [--body B|--body-file F] [--draft] [--label L]
  find-run          --sha SHA
  watch-run         --runId ID
  failed-logs       --runId ID
  rerun-failed      --runId ID
  detect-workflows  (no flags — scans ./.github/workflows)

Flags are camelCase (--commentId, --threadId, --runId). The API verbs (reply,
comment, react, review) name the repository with --owner/--repo; create-pr and
the run verbs act on the repository of the CURRENT DIRECTORY (cd into its
checkout, or set GIT_SKILL_REPO) and take no --repo. Any flag a subcommand does
not list is refused. ` + "`review`" + ` cannot approve, by design.

The harness shell is zsh, which does NOT word-split unquoted $VAR — packing
flags into one variable sends them as a SINGLE argument. Use an array:
  FLAGS=(--owner o --repo r --pr 1); node github-io.ts reply "${FLAGS[@]}" --commentId 9 --body X
`

type flags map[string]string

func (o flags) req(key string) (string, error) {
	if v := o[key]; v != "" {
		return v, nil
	}
	return "", fmt.Errorf("missing required field: %s", key)
}

// buildGitHubCommand is the gh argv for a subcommand. Every required field
// is checked before any argv is returned.
func buildGitHubCommand(sub string, o flags) ([]string, error) {
	var missing error
	req := func(key string) string {
		v, err := o.req(key)
		if missing == nil {
			missing = err
		}
		return v
	}
	var argv []string
	switch sub {
	case "find-run":
		argv = []string{"run", "list", "--commit", req("sha"), "--json", "databaseId,status,conclusion,workflowName,headSha", "--limit", "20"}
	case "watch-run":
		argv = []string{"run", "watch", req("runId"), "--exit-status"}
	case "failed-logs":
		argv = []string{"run", "view", req("runId"), "--log-failed"}
	case "rerun-failed":
		argv = []string{"run", "rerun", req("runId"), "--failed"}
	case "reply":
		path := fmt.Sprintf("repos/%s/%s/pulls/%s/comments/%s/replies", req("owner"), req("repo"), req("pr"), req("commentId"))
		argv = []string{"api", "--method", "POST", path, "-f", "body=" + req("body")}
	case "resolve-thread":
		argv = []string{"api", "graphql", "-f", "query=" + resolveThreadMutation, "-f", "threadId=" + req("threadId")}
	// ── Closure for the NON-THREAD surfaces ──────────────────────────────
	// A review's summary body and a PR conversation comment have no review
	// thread: resolve-thread cannot touch them and reply does not apply.
	case "comment":
		// A PR-level comment. Quote what you answer — it lands at the bottom
		// of the conversation, not under the comment it addresses.
		path := fmt.Sprintf("repos/%s/%s/issues/%s/comments", req("owner"), req("repo"), req("pr"))
		argv = []string{"api", "--method", "POST", path, "-f", "body=" + req("body")}
	case "react":
		// An idempotent "seen/actioned" marker. GitHub exposes reactions for
		// ISSUE comments only — a review summary is closed by `comment` alone.
		path := fmt.Sprintf("repos/%s/%s/issues/comments/%s/reactions", req("owner"), req("repo"), req("commentId"))
		content := o["content"]
		if content == "" {
			content = "+1"
		}
		argv = []string{"api", "--method", "POST", path, "-f", "content=" + content}
	case "review":
		// A verdict-carrying review — what `comment` cannot be: CHANGES_REQUESTED
		// is the signal an autonomous PR author answers with a revision.
		// APPROVE IS DELIBERATELY UNREACHABLE: "never self-approve" is a hard
		// rule and --auto removes the human who enforced it, so the affordance
		// does not exist rather than existing and being discouraged.
		event, err := o.req("event")
		if err != nil {
			return nil, err
		}
		if event != "request-changes" && event != "comment" {
			return nil, fmt.Errorf("review event must be request-changes|comment (never approve): got %s", event)
		}
		argv = []string{"pr", "review", req("pr"), "--" + event, "--body", req("body"), "--repo", req("owner") + "/" + req("repo")}
	case "create-pr":
		// Ready-for-review by default; --draft is opt-in. --fill stays as the
		// fallback: gh gives an explicit --title/--body precedence over it.
		argv = []string{"pr", "create", "--head", req("head"), "--base", req("base"), "--fill"}
		if o["title"] != "" {
			argv = append(argv, "--title", o["title"])
		}
		if o["body"] != "" {
			argv = append(argv, "--body", o["body"])
		}
		if _, draft := o["draft"]; draft {
			argv = append(argv, "--draft")
		}
		// MERGE_POLICY=self repos: eve-ignore keeps eve off a PR nobody waits on.
		if o["label"] != "" {
			argv = append(argv, "--label", o["label"])
		}
	default:
		return nil, fmt.Errorf("Unknown github-io subcommand: %s", sub)
	}
	if missing != nil {
		return nil, missing
	}
	return argv, nil
}

// Workflow is one detect-workflows entry, keys in wire order.
type Workflow struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

var workflowName = regexp.MustCompile(`(?m)^name:\s*(.+)$`)

// detectWorkflows lists .github/workflows/*.y{a,}ml with their `name:`;
// no workflows directory is an empty list, not an error.
func detectWorkflows(cwd string) ([]Workflow, error) {
	dir := filepath.Join(cwd, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return []Workflow{}, nil
	}
	if err != nil {
		return nil, nodeFSError(err, "scandir", dir)
	}
	out := []Workflow{}
	for _, e := range entries {
		f := e.Name()
		if !strings.HasSuffix(f, ".yml") && !strings.HasSuffix(f, ".yaml") {
			continue
		}
		path := filepath.Join(dir, f)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, nodeFSError(err, "open", path)
		}
		name := f
		if m := workflowName.FindStringSubmatch(string(data)); m != nil {
			name = strings.TrimSpace(m[1])
			if len(name) > 0 && (name[0] == '"' || name[0] == '\'') {
				name = name[1:]
			}
			if len(name) > 0 && (name[len(name)-1] == '"' || name[len(name)-1] == '\'') {
				name = name[:len(name)-1]
			}
		}
		out = append(out, Workflow{Name: name, Path: ".github/workflows/" + f})
	}
	return out, nil
}

// nodeFSError renders a filesystem failure as Node's fs errors read.
func nodeFSError(err error, syscallName, path string) error {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		// libuv's code and message, as Node prints them.
		codes := map[syscall.Errno][2]string{
			syscall.ENOTDIR: {"ENOTDIR", "not a directory"},
			syscall.EACCES:  {"EACCES", "permission denied"},
			syscall.EISDIR:  {"EISDIR", "illegal operation on a directory"},
			syscall.ELOOP:   {"ELOOP", "too many symbolic links encountered"},
		}
		if c, ok := codes[errno]; ok {
			return fmt.Errorf("%s: %s, %s '%s'", c[0], c[1], syscallName, path)
		}
	}
	return err
}

var whitespace = regexp.MustCompile(`\s`)

// githubIOArgs is every flag each subcommand reads (buildGitHubCommand is the
// consumer); anything else is refused. --body-file is resolved into --body
// before the build, so the build never sees it.
var githubIOArgs = map[string]verbArgs{
	"detect-workflows": {},
	"find-run":         {values: []string{"sha"}},
	"watch-run":        {values: []string{"runId"}},
	"failed-logs":      {values: []string{"runId"}},
	"rerun-failed":     {values: []string{"runId"}},
	"reply":            {values: []string{"owner", "repo", "pr", "commentId", "body", "body-file"}},
	"resolve-thread":   {values: []string{"threadId"}},
	"comment":          {values: []string{"owner", "repo", "pr", "body", "body-file"}},
	"react":            {values: []string{"owner", "repo", "commentId", "content"}},
	"review":           {values: []string{"owner", "repo", "pr", "event", "body", "body-file"}},
	"create-pr":        {values: []string{"head", "base", "title", "body", "body-file", "label"}, bools: []string{"draft"}},
}

// githubIOUsageFor is the usage line of one subcommand, for its refusals.
func githubIOUsageFor(sub string) string {
	for _, line := range strings.Split(githubIOUsage, "\n") {
		if strings.HasPrefix(line, "  "+sub+" ") {
			return "usage: vybava gitkit github-io " + strings.Join(strings.Fields(line), " ")
		}
	}
	return githubIOUsage
}

// parseFlags reads one subcommand's argv against its declaration: an unknown
// flag, a stray positional or a --draft given a value is refused, never
// dropped (a dropped --repo sent a PR to the cwd's repository, a dropped
// --body-file gave another the commit log as its body).
func parseFlags(sub string, argv []string) (flags, error) {
	// Whitespace in a flag NAME means several flags arrived glued into ONE
	// argv token — zsh not word-splitting an unquoted $VAR. Name the real
	// cause instead of a baffling `unknown argument` or `missing required
	// field`. An inline value (`--title=Fix the bug`) may hold spaces.
	for _, a := range argv {
		key, ok := strings.CutPrefix(a, "--")
		if name, _, _ := strings.Cut(key, "="); ok && whitespace.MatchString(name) {
			return nil, fmt.Errorf("flag arrived as ONE argument with embedded spaces: \"--%s\"\n"+
				"  The harness shell is zsh, which does NOT word-split unquoted $VAR.\n"+
				"  Pass the flags literally, or use an array:\n"+
				"    FLAGS=(--owner o --repo r --pr 1); node github-io.ts <sub> \"${FLAGS[@]}\"", key)
		}
	}
	spec, ok := githubIOArgs[sub]
	if !ok {
		return nil, fmt.Errorf("Unknown github-io subcommand: %s", sub)
	}
	spec.usage = githubIOUsageFor(sub)
	parsed, _, err := spec.parse("github-io "+sub, argv)
	if err != nil {
		return nil, err
	}
	return flags(parsed), nil
}

// resolveBodyFile turns --body-file into --body: the multi-line body lives in
// a file, never on a command line zsh may mangle. An empty file is refused -
// create-pr would otherwise fall back to the commit log without a word.
func resolveBodyFile(o flags) error {
	path, ok := o["body-file"]
	if !ok {
		return nil
	}
	if _, both := o["body"]; both {
		return errors.New("pass --body or --body-file, not both")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("--body-file: %w", err)
	}
	if strings.TrimSpace(string(raw)) == "" {
		return fmt.Errorf("--body-file %s is empty", path)
	}
	delete(o, "body-file")
	o["body"] = string(raw)
	return nil
}

func runGitHubIO(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		// A bare invocation exits 1, so a `$SUB` that expanded to nothing
		// fails loudly instead of looking successful.
		fmt.Fprint(stderr, githubIOUsage)
		return 1
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "help", "--help", "-h":
		fmt.Fprint(stdout, githubIOUsage)
		return 0
	case "detect-workflows":
		if _, err := parseFlags(sub, rest); err != nil {
			return fail(stderr, err)
		}
		cwd, err := syscall.Getwd()
		if err != nil {
			return fail(stderr, err)
		}
		workflows, err := detectWorkflows(cwd)
		if err != nil {
			return fail(stderr, err)
		}
		if err := writeJSON(stdout, workflows); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	o, err := parseFlags(sub, rest)
	if err != nil {
		return fail(stderr, err)
	}
	if err := resolveBodyFile(o); err != nil {
		return fail(stderr, err)
	}
	argv, err := buildGitHubCommand(sub, o)
	if err != nil {
		return fail(stderr, err)
	}
	// repoRoot(nil) — NEVER the verb's argv: github-io's own --repo is a
	// GitHub repo NAME, not a directory anchor. GIT_SKILL_REPO still applies.
	root, err := repoRoot(nil)
	if err != nil {
		return fail(stderr, err)
	}
	out, err := execFile(execOpts{dir: root, inherit: stderr, timeout: 60 * time.Second, maxBuffer: 64 << 20}, "gh", argv...)
	if err != nil {
		return fail(stderr, err)
	}
	fmt.Fprint(stdout, out)
	return 0
}
