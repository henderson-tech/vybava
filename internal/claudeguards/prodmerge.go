package claudeguards

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/gitkit"
)

// ---------------------------------------------------------------------------
// guardProdMerge — prod-merge: landing work on a production branch is the
// user's call, never an agent's. A repo opts in by naming its production
// branches in .claude/.claude.git.config (`PROD_BRANCHES=canary release
// master`; the gitignored .local overrides), read from the MAIN clone the
// way gitkit reads every other key, so every worktree of the repo is covered
// whatever branch it holds. Blocked there, unless the command carries
// CLAUDE_ALLOW_PROD_MERGE=1:
//
//   - `gh pr merge` (--auto and --admin included; prm merges this way) whose
//     PR's base is a production branch;
//   - `gh api` writing …/pulls/<n>/merge into one, or a git/refs write
//     naming one;
//   - `gh api graphql` mergePullRequest / enablePullRequestAutoMerge (the
//     base hides behind a node id, so any of them in an opted-in repo);
//   - `git push` whose destination is a production branch — an explicit
//     refspec, --all/--mirror, or a bare push from one.
//
// A merge naming another repo (`--repo`, or a repos/<o>/<r>/ API path) than
// the checkout's origin cannot read that repo's config: it gets
// defaultProdBranches. Repos that set nothing pay nothing: no fork, no gh.
// The PR base comes from gh; when that read fails in an opted-in repo the
// rule fails closed — the merge would need the same gh anyway.
// ---------------------------------------------------------------------------

const prodMergeEscapeVar = "CLAUDE_ALLOW_PROD_MERGE"

const prodMergeEscape = "Only after the user explicitly said go for THIS merge in this conversation: re-run the same command prefixed with " +
	prodMergeEscapeVar + "=1 and quote their go in your reply. A general \"merge when green\" is not a go for a production branch."

// defaultProdBranches is the policy for a repo whose config cannot be read
// (a merge aimed at another repo than the checkout's).
var defaultProdBranches = []string{"canary", "release", "master"}

// Seams for the tests: the repo policy, the PR base and the origin slug.
var (
	prodBranchesFor = readProdBranches
	prBaseFor       = ghPRBase
	apiPRBaseFor    = ghAPIPRBase
	originSlugFor   = gitOriginSlug
	currentBranchOf = gitCurrentBranch
)

var (
	reProdMergeHint = regexp.MustCompile(`\bgh\b|\bgit\b`)
	reAPIPullMerge  = regexp.MustCompile(`^/?repos/([^/]+)/([^/]+)/pulls/([0-9]+)/merge$`)
	reAPIRepoPath   = regexp.MustCompile(`^/?repos/([^/]+)/([^/]+)/`)
	reGraphQLMerge  = regexp.MustCompile(`\b(mergePullRequest|enablePullRequestAutoMerge)\b`)
	reListSep       = regexp.MustCompile(`[,\s]+`)
)

func guardProdMerge(in *HookInput) *Denial {
	cmd := strings.ReplaceAll(in.ToolInput.Command, "\\\n", " ")
	if !strings.Contains(cmd, "merge") && !strings.Contains(cmd, "push") &&
		!strings.Contains(cmd, "git/refs") && !strings.Contains(cmd, "AutoMerge") {
		return nil
	}
	if !reProdMergeHint.MatchString(cmd) || escapeHatch(cmd, prodMergeEscapeVar) {
		return nil
	}
	home, _ := os.UserHomeDir()
	dir := in.CWD
	for _, seg := range segments(cmd) {
		if textOnly(seg) {
			continue
		}
		f := shellFields(seg)
		if len(f) == 0 {
			continue
		}
		if commandWord(seg) == "cd" {
			if len(f) > 1 {
				dir = resolveDir(f[1], dir, home)
			}
			continue
		}
		if args := afterCommand(f, "gh"); args != nil {
			if d := prodMergeGH(args, seg, dir); d != nil {
				return d
			}
		}
		if args := afterCommand(f, "git"); args != nil {
			if d := prodMergeGitPush(args, dir, home); d != nil {
				return d
			}
		}
	}
	return nil
}

// afterCommand returns the arguments after name when the fields RUN it — as
// the command word or through a launcher chain (`sudo gh …`); nil otherwise.
func afterCommand(f []string, name string) []string {
	for i, t := range f {
		if assignPrefix.MatchString(t) {
			continue
		}
		if j := strings.LastIndexByte(t, '/'); j >= 0 {
			t = t[j+1:]
		}
		if t == name {
			return f[i+1:]
		}
		if !commandRunners[t] {
			return nil
		}
	}
	return nil
}

// prodPolicy is the production-branch set a command in dir aimed at repo
// (owner/name, "" = the checkout's own) answers to, with where it came from.
func prodPolicy(dir, repo string) ([]string, string) {
	if repo != "" {
		if origin := originSlugFor(dir); !strings.EqualFold(origin, repo) {
			return defaultProdBranches, "the default for a repo whose .claude.git.config this checkout cannot read"
		}
	}
	branches, err := prodBranchesFor(dir)
	if err != nil {
		// An unreadable policy file in a repo is never "no policy".
		return defaultProdBranches, "the default, because the repo's .claude.git.config could not be read (" + err.Error() + ")"
	}
	return branches, "PROD_BRANCHES in the repo's .claude/.claude.git.config"
}

func prodMergeGH(args []string, seg, dir string) *Denial {
	switch {
	case len(args) >= 2 && args[0] == "pr" && args[1] == "merge":
		sel, repo := prMergeArgs(args[2:])
		branches, source := prodPolicy(dir, repo)
		if len(branches) == 0 {
			return nil
		}
		base, err := prBaseFor(dir, repo, sel)
		if err != nil {
			return prodMergeUnknown("gh pr merge", err)
		}
		if slices.Contains(branches, base) {
			return prodMergeDeny("gh pr merge into "+base, base, source)
		}
	case len(args) >= 1 && args[0] == "api":
		return prodMergeAPI(args[1:], seg, dir)
	}
	return nil
}

// prMergeArgs picks the PR selector and --repo out of `gh pr merge` flags.
func prMergeArgs(args []string) (sel, repo string) {
	valued := map[string]bool{"-R": true, "--repo": true, "-b": true, "--body": true, "-F": true, "--body-file": true,
		"-t": true, "--subject": true, "-A": true, "--author-email": true, "--match-head-commit": true}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-R" || a == "--repo":
			if i+1 < len(args) {
				repo = args[i+1]
			}
			i++
		case strings.HasPrefix(a, "--repo="):
			repo = strings.TrimPrefix(a, "--repo=")
		case valued[a]:
			i++
		case strings.HasPrefix(a, "-"):
		case sel == "":
			sel = a
		}
	}
	// A PR URL names its repo; the checkout's config does not answer for it.
	if m := rePRURL.FindStringSubmatch(sel); m != nil && repo == "" {
		repo = m[1]
	}
	return sel, normalizeSlug(repo)
}

var rePRURL = regexp.MustCompile(`github\.com/([^/]+/[^/]+)/pull/[0-9]+`)

func prodMergeAPI(args []string, seg, dir string) *Denial {
	endpoint, method, writes := "", "", false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-X" || a == "--method":
			if i+1 < len(args) {
				method = strings.ToUpper(args[i+1])
			}
			i++
		case strings.HasPrefix(a, "--method="):
			method = strings.ToUpper(strings.TrimPrefix(a, "--method="))
		case slices.Contains([]string{"-f", "-F", "--field", "--raw-field", "--input"}, a):
			writes = true
			i++
		case slices.Contains([]string{"-H", "--header", "-q", "--jq", "-t", "--template", "--hostname", "--cache", "-p", "--preview"}, a):
			i++
		case strings.HasPrefix(a, "-"):
		case endpoint == "":
			endpoint = a
		}
	}
	if method == "" && writes {
		method = "POST"
	}
	if method == "" || method == "GET" || method == "HEAD" {
		return nil
	}
	if endpoint == "graphql" {
		if reGraphQLMerge.MatchString(seg) {
			if branches, source := prodPolicy(dir, ""); len(branches) > 0 {
				return prodMergeDeny("a GraphQL merge mutation (its base hides behind a node id)", strings.Join(branches, "/"), source)
			}
		}
		return nil
	}
	repo := ""
	if m := reAPIRepoPath.FindStringSubmatch(endpoint); m != nil && !strings.HasPrefix(m[1], "{") {
		repo = normalizeSlug(m[1] + "/" + m[2])
	}
	branches, source := prodPolicy(dir, repo)
	if len(branches) == 0 {
		return nil
	}
	if m := reAPIPullMerge.FindStringSubmatch(endpoint); m != nil {
		base, err := apiPRBaseFor(dir, strings.TrimPrefix(endpoint[:len(endpoint)-len("/merge")], "/"))
		if err != nil {
			return prodMergeUnknown("gh api "+endpoint, err)
		}
		if slices.Contains(branches, base) {
			return prodMergeDeny("gh api "+method+" "+endpoint+" into "+base, base, source)
		}
		return nil
	}
	if strings.Contains(endpoint, "git/refs") {
		for _, b := range branches {
			if strings.HasSuffix(endpoint, "heads/"+b) || strings.Contains(seg, "refs/heads/"+b) {
				return prodMergeDeny("gh api "+method+" "+endpoint+" (a ref write on "+b+")", b, source)
			}
		}
	}
	return nil
}

// prodMergeGitPush checks `git [globals] push …`: every destination ref, and
// a bare push (or HEAD) as the current branch.
func prodMergeGitPush(args []string, dir, home string) *Denial {
	sub := -1
	for i := 0; i < len(args) && sub < 0; i++ {
		switch a := args[i]; {
		case a == "-C":
			if i+1 < len(args) {
				dir = resolveDir(args[i+1], dir, home)
			}
			i++
		case a == "-c" || a == "--git-dir" || a == "--work-tree" || a == "--namespace":
			i++
		case !strings.HasPrefix(a, "-"):
			sub = i
		}
	}
	if sub < 0 || args[sub] != "push" {
		return nil
	}
	var pos []string
	all := false
	rest := args[sub+1:]
	for i := 0; i < len(rest); i++ {
		switch a := rest[i]; {
		case a == "--all" || a == "--mirror" || a == "--branches":
			all = true
		case a == "-o" || a == "--push-option" || a == "--receive-pack" || a == "--exec" || a == "--repo":
			i++
		case strings.HasPrefix(a, "-"):
		default:
			pos = append(pos, a)
		}
	}
	branches, source := prodPolicy(dir, "")
	if len(branches) == 0 {
		return nil
	}
	if all {
		return prodMergeDeny("git push --all/--mirror (it pushes every local branch, production ones included)", strings.Join(branches, "/"), source)
	}
	refspecs := []string{}
	if len(pos) > 1 {
		refspecs = pos[1:]
	}
	if len(refspecs) == 0 {
		refspecs = []string{"HEAD"}
	}
	for _, r := range refspecs {
		dst := strings.TrimPrefix(r, "+")
		if _, after, ok := strings.Cut(dst, ":"); ok {
			dst = after
		}
		if dst == "HEAD" || dst == "@" {
			dst = currentBranchOf(dir)
		}
		dst = strings.TrimPrefix(dst, "refs/heads/")
		if slices.Contains(branches, dst) {
			return prodMergeDeny("git push to "+dst, dst, source)
		}
	}
	return nil
}

func prodMergeDeny(what, base, source string) *Denial {
	return deny("prod-merge:merge", fmt.Sprintf(`%s lands work on a production branch (%s — %s).

Landing on a production branch is the user's decision, never an agent's: a merge
or push there ships to production. prm stops here too.

Do this instead: leave the PR open and green, and hand it back with its URL and
what it promotes. The user merges it (or tells you to, for this one merge).`, what, base, source), prodMergeEscape)
}

func prodMergeUnknown(what string, err error) *Denial {
	return deny("prod-merge:merge", fmt.Sprintf(`%s: this repo declares production branches, and the PR's base could not
be read (%v), so the guard cannot tell whether this merge lands on one.

Retry once gh works (gh auth status). If the base is a production branch, the
merge is the user's decision, not yours.`, what, err), prodMergeEscape)
}

// --- lookups (each a seam above) -------------------------------------------

// readProdBranches returns PROD_BRANCHES of dir's main clone; nil when the
// repo sets none (or dir is not a repo).
func readProdBranches(dir string) ([]string, error) {
	common := git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if common == "" {
		return nil, nil
	}
	root := common
	if filepath.Base(common) == ".git" {
		root = filepath.Dir(common)
	}
	cfg, err := gitkit.ReadGitConfig(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, b := range reListSep.Split(cfg["PROD_BRANCHES"], -1) {
		if b = strings.TrimSpace(b); b != "" {
			out = append(out, b)
		}
	}
	return out, nil
}

func ghOutput(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, "gh", args...)
	c.Dir = dir
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	s := strings.TrimSpace(out.String())
	if s == "" {
		return "", errors.New("gh returned no base")
	}
	return s, nil
}

func ghPRBase(dir, repo, sel string) (string, error) {
	args := []string{"pr", "view"}
	if sel != "" {
		args = append(args, sel)
	}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	return ghOutput(dir, append(args, "--json", "baseRefName", "--jq", ".baseRefName")...)
}

func ghAPIPRBase(dir, pullPath string) (string, error) {
	return ghOutput(dir, "api", pullPath, "--jq", ".base.ref")
}

var reRemoteSlug = regexp.MustCompile(`github\.com[:/]([^/]+/[^/]+?)(\.git)?/?$`)

func gitOriginSlug(dir string) string {
	if m := reRemoteSlug.FindStringSubmatch(git(dir, "config", "--get", "remote.origin.url")); m != nil {
		return m[1]
	}
	return ""
}

func gitCurrentBranch(dir string) string {
	return git(dir, "symbolic-ref", "--quiet", "--short", "HEAD")
}

// normalizeSlug turns a --repo value (owner/name, host/owner/name or a URL)
// into owner/name.
func normalizeSlug(s string) string {
	s = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(s), "/"), ".git")
	if s == "" {
		return ""
	}
	parts := strings.Split(s, "/")
	if len(parts) < 2 {
		return s
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1]
}
