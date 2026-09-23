package claudeguards

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
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

	reSecret = regexp.MustCompile(`BEGIN [A-Z ]*PRIVATE KEY|AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[baprs]-[A-Za-z0-9-]{10,}|sk-ant-[A-Za-z0-9_-]{20,}|sk-[A-Za-z0-9]{40,}|AIza[0-9A-Za-z_-]{35}|eyJ[A-Za-z0-9_-]{20,}\.eyJ`)

	rePassword     = regexp.MustCompile(`(?i)(password|passwd|secret|api[_-]?key|access[_-]?token)["']?[[:space:]]*[:=][[:space:]]*["'][^"']{8,}`)
	rePasswordSkip = regexp.MustCompile(`\$\{|\$\(|process\.env|os\.Getenv|os\.environ|System\.getenv|Deno\.env\.get|secrets\.|vars\.|example|placeholder|changeme|<[^>]+>`)

	// An assignment whose IDENTIFIER names an environment variable ("…env…")
	// and whose VALUE is a bare SCREAMING_SNAKE identifier. That value is the
	// KEY handed to os.Getenv / process.env[…], never a credential:
	//   const EnvPassword = "POSTA_APP_PASSWORD"   → not a secret
	//   var   password    = "ADMIN_PASSWORD"       → STILL a secret
	// BOTH signals are required, because either one alone blinds the rule to a
	// real leak. At least one underscore is required too, so a caps-only token
	// with real entropy (base32 TOTP seed, uppercase hex key) keeps counting as
	// a secret.
	// The (?i) is scoped to the identifier on purpose: letting it reach the
	// value would make the SCREAMING_SNAKE alternation case-insensitive, and
	// `envKey = "sk_live_9f8a…"` would suppress a real leak.
	reEnvNameConst = regexp.MustCompile(`\b(?i:[a-z0-9_]*env[a-z0-9_]*)[[:space:]]*:?=[[:space:]]*` +
		`(?:"[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+"|'[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+')`)

	reIPv4      = regexp.MustCompile(`\b[0-9]{1,3}(\.[0-9]{1,3}){3}\b`)
	rePrivateIP = regexp.MustCompile(`^(0\.|10\.|127\.|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.|255\.|169\.254\.)`)
)

// credentialAssignment reports whether one added diff line hardcodes a
// credential. Pure — unit-testable, and the one place the three signals
// (shape, placeholder, env-var name) are weighed together.
func credentialAssignment(l string) bool {
	return rePassword.MatchString(l) &&
		!rePasswordSkip.MatchString(l) &&
		!reEnvNameConst.MatchString(l)
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
		diff = append(diff, untrackedLines(t.dir, untracked)...)
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
		if hits := grepN(added, 10, func(l string) bool { return reSecret.MatchString(l) }); len(hits) > 0 {
			fmt.Fprintf(findings, "Secret-shaped content in the changes:\n%s\n", strings.Join(hits, "\n"))
		}
		if hits := grepN(added, 10, credentialAssignment); len(hits) > 0 {
			fmt.Fprintf(findings, "Hardcoded credential assignments:\n%s\n", strings.Join(hits, "\n"))
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
// diff lines; binary files and anything past the budget are skipped.
func untrackedLines(dir string, files []string) []string {
	var out []string
	budget := untrackedBudget
	for _, f := range files {
		if budget <= 0 {
			break
		}
		p := filepath.Join(dir, f)
		if st, err := os.Lstat(p); err != nil || !st.Mode().IsRegular() || st.Size() > int64(budget) {
			continue // `git add` stages a symlink itself, never what it points to
		}
		b, err := os.ReadFile(p)
		if err != nil || bytes.IndexByte(b[:min(len(b), 8000)], 0) >= 0 {
			continue
		}
		budget -= len(b)
		for _, l := range strings.Split(string(b), "\n") {
			out = append(out, "+"+l)
		}
	}
	return out
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
