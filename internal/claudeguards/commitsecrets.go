package claudeguards

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/secretscan"
)

// ---------------------------------------------------------------------------
// guardCommitSecrets — blocks `git commit` when what it could commit contains
// secrets or private info: staged and unstaged changes to tracked files in
// every repo the command names, plus untracked files when the command also
// runs `git add`. It fails closed: an unrelated uncommitted secret blocks the
// commit too, and COMMIT_GUARD_ALLOW=1 says it is not part of it.
//
// Always blocked (any repo):    private keys, cloud/API tokens, key-material files
// Blocked only in PUBLIC repos: known-infra strings (private-strings.txt) and
//                               any public IPv4 address
//
// Bypass for an intentional exception: prepend COMMIT_GUARD_ALLOW=1.
//
// Repo visibility comes from `gh repo view`, which is a network call — the one
// thing that ever made this guard slow. Policy here: any cached value is used
// immediately (stale-while-revalidate; a stale one triggers a detached
// background refresh); only a repo with NO cache at all pays a synchronous
// lookup, hard-capped at 2.5s. A failed lookup (no GitHub remote, gh offline
// or slow) is cached as unknown and retried in the background at once and
// then every ten minutes, so it is paid once, not on every commit. Unknown
// visibility → public-only checks are skipped (fail open), same as the shell
// version.
// ---------------------------------------------------------------------------

const (
	visibilityCacheName  = "claude-repo-visibility"
	visibilityTTL        = 24 * time.Hour
	unknownVisibility    = "UNKNOWN"
	unknownVisibilityTTL = 10 * time.Minute
	ghSyncTimeout        = 2500 * time.Millisecond
)

var (
	reBadFile    = regexp.MustCompile(`(^|/)(id_rsa|id_ed25519|id_ecdsa|id_dsa)[^/]*$|\.(pem|key|p12|pfx|crt|cer|der|jks|keystore|ppk|kubeconfig)$|(^|/)\.env(\..*)?$|(^|/)(\.netrc|known_hosts|authorized_keys)$`)
	reEnvExample = regexp.MustCompile(`\.env\.example$`)

	reIPv4 = regexp.MustCompile(`\b[0-9]{1,3}(\.[0-9]{1,3}){3}\b`)
	// Also never routable: the RFC 5737 documentation nets fixtures are meant to use.
	rePrivateIP = regexp.MustCompile(`^(0\.|10\.|127\.|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.|255\.|169\.254\.|192\.0\.2\.|198\.51\.100\.|203\.0\.113\.)`)
)

// Secret shapes and the credential-assignment rule are secretscan's — one
// catalogue for this guard, memorylint and `vybava redact`.
var credentialAssignment = secretscan.CredentialAssignment

// quoteHits renders offending diff lines for the denial without the secret:
// the denial is tool output, and tool output is the session transcript.
func quoteHits(hits []string) string {
	quoted := make([]string, len(hits))
	for i, h := range hits {
		quoted[i] = secretscan.Quote(h)
	}
	return strings.Join(quoted, "\n")
}

// The guard reads the command as text, never as a parsed shell line, and fails
// closed: which of a repo's changes one commit takes (`-a`, a pathspec, a
// `git add` earlier in the same command, `$(…)` in the message) is not
// modelled, so every change it could take is scanned.
var (
	// A `git` word, then only global options (`-C <dir>`, `-c k=v`,
	// `--git-dir=…`, `--no-pager` …), then `commit` as its own word — behind
	// `if`, `then`, `eval`, `bash -c "…"` or a line continuation alike.
	reGitCommit = regexp.MustCompile(`\bgit` + gitGlobals + `\s+commit(?:$|[\s;&|)"'])`)
	reGitAdd    = regexp.MustCompile(`\bgit` + gitGlobals + `\s+add(?:$|[\s;&|)"'])`)

	// Where the command points git: `cd` and `-C` targets, --git-dir and
	// --work-tree (or their GIT_* assignments).
	reDirArg      = regexp.MustCompile(`(?:\bcd|\s-C)\s+` + shellWord)
	reGitDirArg   = regexp.MustCompile(`(?:--git-dir[=\s]\s*|\bGIT_DIR=)` + shellWord)
	reWorkTreeArg = regexp.MustCompile(`(?:--work-tree[=\s]\s*|\bGIT_WORK_TREE=)` + shellWord)
)

const (
	// A word may join quoted, `$(…)` and plain parts: user.name="Claude Code".
	shellWord  = `((?:"[^"]*"|'[^']*'|\$\([^)]*\)|[^\s;&|)"'])+)`
	gitGlobals = `(?:\s+(?:-[Cc]|--git-dir|--work-tree|--namespace|--config-env)(?:\s+|=)` + shellWord + `|\s+--?[A-Za-z][\w-]*(?:=\S+)?)*`
)

func guardCommitSecrets(in *HookInput) *Denial {
	cmd := strings.ReplaceAll(in.ToolInput.Command, "\\\n", " ")
	if !reGitCommit.MatchString(cmd) || escapeHatch(cmd, "COMMIT_GUARD_ALLOW") {
		return nil
	}
	adds := reGitAdd.MatchString(cmd)
	var findings strings.Builder
	seen := map[string]bool{}
	for _, t := range commitTargets(cmd, in.CWD) {
		scanRepo(t, adds, seen, &findings)
	}
	if findings.Len() > 0 {
		scope := "staged and unstaged changes to tracked files"
		if adds {
			scope += ", and untracked files (the command runs git add)"
		}
		return deny("commit-secrets", "the commit could add sensitive content:\n\n"+findings.String()+
			"\nScanned: "+scope+" — whatever this commit can take.\nFix: unstage/redact the flagged content (secrets -> env vars or repo secrets; IPs in public repos -> repo variables).",
			"If this is a false positive, or the flagged lines are not part of this commit, re-run with COMMIT_GUARD_ALLOW=1 prefixed to the command.")
	}
	return nil
}

// repoTarget is a repository the command may commit in: a directory git runs
// in, plus the --git-dir / --work-tree it was given.
type repoTarget struct {
	dir     string
	globals []string
}

// commitTargets lists every repository the command names: the session cwd,
// each `cd` and `-C` target (relative ones against the cwd and against every
// `cd`), and each --git-dir with its work tree. Scanning one too many is a
// few forks; missing the one that commits is a leak.
func commitTargets(cmd, cwd string) []repoTarget {
	home, _ := os.UserHomeDir()
	word := func(m []string) string {
		w := strings.Trim(m[1], `"'`)
		for _, h := range []string{"${HOME}", "$HOME"} {
			if strings.HasPrefix(w, h) {
				return home + w[len(h):]
			}
		}
		return w
	}
	bases := []string{cwd}
	for _, m := range reDirArg.FindAllStringSubmatch(cmd, -1) {
		if strings.HasPrefix(strings.TrimSpace(m[0]), "cd") {
			bases = append(bases, resolveDir(word(m), cwd, home))
		}
	}
	targets := []repoTarget{{dir: cwd}}
	for _, m := range reDirArg.FindAllStringSubmatch(cmd, -1) {
		for _, b := range bases {
			targets = append(targets, repoTarget{dir: resolveDir(word(m), b, home)})
		}
	}
	workTree := cwd
	if m := reWorkTreeArg.FindStringSubmatch(cmd); m != nil {
		workTree = resolveDir(word(m), cwd, home)
	}
	for _, m := range reGitDirArg.FindAllStringSubmatch(cmd, -1) {
		targets = append(targets, repoTarget{dir: workTree, globals: []string{
			"--git-dir=" + resolveDir(word(m), cwd, home), "--work-tree=" + workTree}})
	}
	return targets
}

// scanRepo appends what a commit in t could take that must not be committed;
// seen skips a repository another target already scanned.
func scanRepo(t repoTarget, adds bool, seen map[string]bool, findings *strings.Builder) {
	if st, err := os.Stat(t.dir); err != nil || !st.IsDir() {
		return
	}
	g := func(args ...string) string { return git(t.dir, append(t.globals, args...)...) }
	dirs := splitLines(g("rev-parse", "--path-format=absolute", "--absolute-git-dir", "--git-common-dir"))
	if len(dirs) != 2 || seen[dirs[0]] {
		return
	}
	seen[dirs[0]] = true
	// NUL-separated names: git C-quotes a non-ASCII path otherwise, and a
	// quoted name matches no pattern and opens no file.
	names := splitNUL(g("diff", "--cached", "--name-only", "-z", "--diff-filter=ACM"))
	names = append(names, splitNUL(g("diff", "--name-only", "-z", "--diff-filter=ACM"))...)
	diff := splitLines(g("diff", "--cached", "--no-ext-diff", "-U0"))
	diff = append(diff, splitLines(g("diff", "--no-ext-diff", "-U0"))...)
	if adds {
		// `:/` lists the whole repo from a subdirectory too, as `git add -A`
		// stages it; paths stay relative to t.dir.
		untracked := splitNUL(g("ls-files", "-z", "--others", "--exclude-standard", ":/"))
		names = append(names, untracked...)
		lines, unscanned := untrackedLines(t.dir, untracked)
		diff = append(diff, lines...)
		if len(unscanned) > 0 {
			fmt.Fprintf(findings, "Untracked text past the %d MiB this hook reads, so not scanned:\n%s\nRun `git add` in its own call first: the commit then scans the staged diff in full.\n",
				untrackedBudget>>20, strings.Join(unscanned[:min(len(unscanned), 10)], "\n"))
		}
	}

	// --- 1. Changed files that are key material (regardless of content) ---
	var badFiles []string
	for _, f := range names {
		if reBadFile.MatchString(f) && !reEnvExample.MatchString(f) {
			badFiles = append(badFiles, f)
		}
	}
	if len(badFiles) > 0 {
		fmt.Fprintf(findings, "Key/credential files among the changes:\n%s\n", strings.Join(badFiles, "\n"))
	}

	// --- 2. Secret patterns in added lines (any repo) ---
	var added []string
	for _, l := range diff {
		if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++") {
			added = append(added, l)
		}
	}
	if len(added) > 0 {
		if hits := grepN(added, 10, secretscan.ContainsToken); len(hits) > 0 {
			fmt.Fprintf(findings, "Secret-shaped content in the changes (values withheld):\n%s\n", quoteHits(hits))
		}
		if hits := grepN(added, 10, credentialAssignment); len(hits) > 0 {
			fmt.Fprintf(findings, "Hardcoded credential assignments (values withheld):\n%s\n", quoteHits(hits))
		}
	}

	// --- 3. Public-repo-only checks: infra strings + public IPs ---
	if len(added) > 0 && repoVisibility(t.dir, dirs[1]) == "PUBLIC" {
		home, _ := os.UserHomeDir()
		if denylist, err := os.ReadFile(filepath.Join(home, ".claude", "hooks", "private-strings.txt")); err == nil {
			needles := splitLines(string(denylist))
			if hits := grepN(added, 10, func(l string) bool {
				for _, n := range needles {
					if n != "" && strings.Contains(l, n) {
						return true
					}
				}
				return false
			}); len(hits) > 0 {
				fmt.Fprintf(findings, "Known private infra strings (from private-strings.txt) in a PUBLIC repo:\n%s\n", strings.Join(hits, "\n"))
			}
		}
		seen := map[string]bool{}
		var pubIPs []string
		for _, l := range added {
			for _, ip := range reIPv4.FindAllString(l, -1) {
				if !seen[ip] && !rePrivateIP.MatchString(ip) {
					seen[ip] = true
					pubIPs = append(pubIPs, ip)
				}
			}
		}
		if len(pubIPs) > 10 {
			pubIPs = pubIPs[:10]
		}
		if len(pubIPs) > 0 {
			fmt.Fprintf(findings, "Public IPv4 addresses in a PUBLIC repo (use repo variables instead):\n%s\n", strings.Join(pubIPs, "\n"))
		}
	}
}

// repoVisibility returns "PUBLIC"/"PRIVATE"/… or "" when unknown. The cache
// file lives in the repository's common git dir, so every worktree of it
// shares one lookup (visibility rarely changes; survives clones' lifetime).
func repoVisibility(dir, commonDir string) string {
	cache := filepath.Join(commonDir, visibilityCacheName)
	if b, err := os.ReadFile(cache); err == nil {
		vis, ttl := strings.TrimSpace(string(b)), visibilityTTL
		if vis == unknownVisibility {
			vis, ttl = "", unknownVisibilityTTL
		}
		if st, err := os.Stat(cache); err == nil && time.Since(st.ModTime()) > ttl {
			spawnVisibilityRefresh(dir, cache)
		}
		return vis
	}
	// No cache at all: one synchronous lookup, hard-capped. A failure is
	// cached as unknown, so the next commit does not pay it again, and retried
	// at once in the background — a lookup that merely timed out corrects
	// itself within seconds instead of leaving a public repo unchecked.
	vis := ghVisibility(dir, ghSyncTimeout)
	if vis != "" {
		_ = os.WriteFile(cache, []byte(vis), 0o644)
		return vis
	}
	_ = os.WriteFile(cache, []byte(unknownVisibility), 0o644)
	spawnVisibilityRefresh(dir, cache)
	return ""
}

// spawnVisibilityRefresh is refreshVisibilityDetached; tests replace it, since
// re-executing a test binary would run the tests again.
var spawnVisibilityRefresh = refreshVisibilityDetached

// refreshVisibilityDetached re-execs this binary as a detached child so the
// hook returns immediately; the child owns the (slow) network call.
func refreshVisibilityDetached(dir, cache string) {
	self, err := os.Executable()
	if err != nil {
		return
	}
	c := exec.Command(self, "refresh-visibility", dir, cache)
	c.Args[0] = "claude-guards" // multicall dispatch is on argv[0]
	c.Stdout, c.Stderr, c.Stdin = nil, nil, nil
	c.SysProcAttr = detachedAttr()
	if c.Start() == nil {
		_ = c.Process.Release()
	}
}

// RefreshVisibility is the detached child's entry point.
func RefreshVisibility(dir, cache string) {
	if vis := ghVisibility(dir, 10*time.Second); vis != "" {
		_ = os.WriteFile(cache, []byte(vis), 0o644)
	} else {
		// Keep serving the stale value, but bump mtime so a dead network
		// doesn't spawn a refresh child on every commit.
		now := time.Now()
		_ = os.Chtimes(cache, now, now)
	}
}

func ghVisibility(dir string, timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c := exec.CommandContext(ctx, "gh", "repo", "view", "--json", "visibility", "--jq", ".visibility")
	c.Dir = dir
	var out bytes.Buffer
	c.Stdout = &out
	if c.Run() != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

// git runs a git subcommand in dir and returns trimmed stdout ("" on any error).
func git(dir string, args ...string) string {
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out bytes.Buffer
	c.Stdout = &out
	if c.Run() != nil {
		return ""
	}
	return strings.TrimRight(out.String(), "\n")
}

// untrackedBudget caps how much untracked content one hook call reads, so a
// big generated tree that is not ignored cannot stall it.
const untrackedBudget = 4 << 20

// untrackedLines reads the untracked files `git add` could stage as added
// diff lines. Binary files are skipped, as a diff shows no lines for them
// either. Text past the budget is returned as unscanned: the guard refuses
// it rather than let it through unread.
func untrackedLines(dir string, files []string) (lines, unscanned []string) {
	budget := int64(untrackedBudget)
	for _, f := range files {
		p := filepath.Join(dir, f)
		st, err := os.Lstat(p)
		if err != nil || !st.Mode().IsRegular() {
			continue // `git add` stages a symlink itself, never what it points to
		}
		fh, err := os.Open(p)
		if err != nil {
			continue
		}
		head := make([]byte, 8000)
		n, _ := io.ReadFull(fh, head)
		fh.Close()
		if bytes.IndexByte(head[:n], 0) >= 0 {
			continue
		}
		if st.Size() > budget {
			unscanned = append(unscanned, f)
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			unscanned = append(unscanned, f)
			continue
		}
		budget -= int64(len(b))
		for _, l := range strings.Split(string(b), "\n") {
			lines = append(lines, "+"+l)
		}
	}
	return lines, unscanned
}

func splitNUL(s string) []string {
	var out []string
	for _, f := range strings.Split(s, "\x00") {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// grepN returns up to n lines matching pred.
func grepN(lines []string, n int, pred func(string) bool) []string {
	var out []string
	for _, l := range lines {
		if pred(l) {
			out = append(out, l)
			if len(out) == n {
				break
			}
		}
	}
	return out
}
