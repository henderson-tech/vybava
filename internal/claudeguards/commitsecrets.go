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
// guardCommitSecrets — blocks `git commit` when the STAGED diff contains
// secrets or private info.
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
// lookup, hard-capped at 2.5s. Unknown visibility → public-only checks are
// skipped (fail open), same as the shell version.
// ---------------------------------------------------------------------------

const (
	visibilityCacheName = "claude-repo-visibility"
	visibilityTTL       = 24 * time.Hour
	ghSyncTimeout       = 2500 * time.Millisecond
)

var (
	reCdPrefix = regexp.MustCompile(`^\(?[[:space:]]*cd[[:space:]]+"?([^"&;)]+)"?[[:space:]]*&&`)

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

func guardCommitSecrets(in *HookInput) *Denial {
	cmd := in.ToolInput.Command
	if !strings.Contains(cmd, "git commit") || escapeHatch(cmd, "COMMIT_GUARD_ALLOW") {
		return nil
	}

	// Repo dir: honor a leading `cd <path> &&` / `(cd <path> &&`, else session cwd.
	dir := in.CWD
	if m := reCdPrefix.FindStringSubmatch(cmd); m != nil {
		dir = strings.TrimSpace(m[1])
		if strings.HasPrefix(dir, "~") {
			home, _ := os.UserHomeDir()
			dir = home + dir[1:]
		}
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return nil
	}
	if git(dir, "rev-parse", "--git-dir") == "" {
		return nil
	}

	var findings strings.Builder

	// --- 1. Staged files that are key material (regardless of content) ---
	var badFiles []string
	for _, f := range splitLines(git(dir, "diff", "--cached", "--name-only", "--diff-filter=ACM")) {
		if reBadFile.MatchString(f) && !reEnvExample.MatchString(f) {
			badFiles = append(badFiles, f)
		}
	}
	if len(badFiles) > 0 {
		fmt.Fprintf(&findings, "Key/credential files staged:\n%s\n", strings.Join(badFiles, "\n"))
	}

	// --- 2. Secret patterns in added lines (any repo) ---
	var added []string
	for _, l := range splitLines(git(dir, "diff", "--cached", "-U0")) {
		if strings.HasPrefix(l, "+") && !strings.HasPrefix(l, "+++") {
			added = append(added, l)
		}
	}
	if len(added) > 0 {
		if hits := grepN(added, 10, func(l string) bool { return reSecret.MatchString(l) }); len(hits) > 0 {
			fmt.Fprintf(&findings, "Secret-shaped content in staged diff:\n%s\n", strings.Join(hits, "\n"))
		}
		if hits := grepN(added, 10, credentialAssignment); len(hits) > 0 {
			fmt.Fprintf(&findings, "Hardcoded credential assignments:\n%s\n", strings.Join(hits, "\n"))
		}
	}

	// --- 3. Public-repo-only checks: infra strings + public IPs ---
	if len(added) > 0 && repoVisibility(dir) == "PUBLIC" {
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
				fmt.Fprintf(&findings, "Known private infra strings (from private-strings.txt) in a PUBLIC repo:\n%s\n", strings.Join(hits, "\n"))
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
			fmt.Fprintf(&findings, "Public IPv4 addresses in a PUBLIC repo (use repo variables instead):\n%s\n", strings.Join(pubIPs, "\n"))
		}
	}

	if findings.Len() > 0 {
		return deny("commit-secrets", "staged changes contain sensitive content:\n\n"+findings.String()+"\nFix: unstage/redact the flagged content (secrets -> env vars or repo secrets; IPs in public repos -> repo variables).", "If this is a false positive and intentional, re-run with COMMIT_GUARD_ALLOW=1 prefixed to the command.")
	}
	return nil
}

// repoVisibility returns "PUBLIC"/"PRIVATE"/… or "" when unknown.
// Cache file lives inside .git/ (visibility rarely changes; survives clones' lifetime).
func repoVisibility(dir string) string {
	gitdir := git(dir, "rev-parse", "--absolute-git-dir")
	if gitdir == "" {
		return ""
	}
	cache := filepath.Join(gitdir, visibilityCacheName)
	if b, err := os.ReadFile(cache); err == nil {
		vis := strings.TrimSpace(string(b))
		if st, err := os.Stat(cache); err == nil && time.Since(st.ModTime()) > visibilityTTL {
			refreshVisibilityDetached(dir, cache)
		}
		return vis
	}
	// No cache at all: one synchronous lookup, hard-capped.
	vis := ghVisibility(dir, ghSyncTimeout)
	if vis != "" {
		_ = os.WriteFile(cache, []byte(vis), 0o644)
	}
	return vis
}

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
