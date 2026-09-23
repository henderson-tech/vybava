package claudeguards

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBadFilePatterns(t *testing.T) {
	block := []string{
		"id_rsa", "keys/id_ed25519.bak", "cert.pem", "server.key", "app.p12",
		"deploy.pfx", "site.crt", "ca.cer", "x.der", "release.jks", "app.keystore",
		"putty.ppk", "cluster.kubeconfig", ".env", ".env.local", "api/.netrc",
		"ssh/known_hosts", "ssh/authorized_keys",
	}
	pass := []string{".env.example", "src/main.go", "docs/keys.md", "monkey.ts", "envelope.env.example"}
	for _, f := range block {
		if !reBadFile.MatchString(f) || reEnvExample.MatchString(f) {
			t.Errorf("should block staged file %q", f)
		}
	}
	for _, f := range pass {
		if reBadFile.MatchString(f) && !reEnvExample.MatchString(f) {
			t.Errorf("should pass staged file %q", f)
		}
	}
}

func TestSecretPatterns(t *testing.T) {
	block := []string{
		"+-----BEGIN RSA PRIVATE KEY-----",
		"+aws_key = AKIAIOSFODNN7EXAMPLE",
		"+token: ghp_abcdefghijklmnopqrstuv123456",
		"+github_pat_11ABCDEFG0123456789abcdef",
		"+slack: xoxb-1234567890-abcdef",
		"+key = sk-ant-api03-abcdefghijklmnopqrst",
		"+g = AIzaSyA-abcdefghijklmnopqrstuvwxyz0123456",
		"+jwt eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIx",
	}
	pass := []string{"+const skill = 'sk-illful'", "+// mention AKIA keys in docs", "+x = 1"}
	for _, l := range block {
		if !reSecret.MatchString(l) {
			t.Errorf("should flag %q", l)
		}
	}
	for _, l := range pass {
		if reSecret.MatchString(l) {
			t.Errorf("should pass %q", l)
		}
	}
}

func TestPasswordAssignments(t *testing.T) {
	if !credentialAssignment(`+password = "hunter2hunter2"`) {
		t.Error("literal password assignment should flag")
	}
	for _, l := range []string{
		`+password = "${DB_PASSWORD}"`,
		`+api_key = "process.env.KEY"`,
		`+secret: "changeme-please"`,
		`+token = "<your-token-here>"`,
	} {
		if credentialAssignment(l) {
			t.Errorf("placeholder/env form should pass: %q", l)
		}
	}
}

// An env-var NAME constant is the KEY handed to os.Getenv, not the secret —
// flagging it blocked real commits (vitrinka t/1312). Suppression demands BOTH
// signals, so a credential that merely happens to be SCREAMING_SNAKE, or an
// env-named identifier holding a real token, keeps flagging.
func TestEnvVarNameConstantsAreNotCredentials(t *testing.T) {
	flag := []string{
		`+var password = "hunter2hunter2"`,
		`+var apiKey = "sk_live_9f8a7b6c5d4e3f2a1b"`,
		`+var secret = "correct horse battery staple"`,
		`+var accessToken = "aG9yc2ViYXR0ZXJ5c3RhcGxlMTIz"`,
		// SCREAMING_SNAKE value, but the identifier names no env var.
		`+var password = "ADMIN_PASSWORD"`,
		`+var secret = "SUPER_SECRET_VALUE"`,
		`+var apiKey = "MY_API_KEY_VALUE_9"`,
		`+var password = "A1B2_C3D4_E5F6"`,
		// Env-named identifier, but the value is a real token, not a var name.
		`+var envPassword = "sk_live_9f8a7b6c5d4e3f2a1b"`,
		// Caps with no underscore keeps its entropy: a base32 TOTP seed.
		`+var secret = "JBSWY3DPEHPK3PXP"`,
	}
	pass := []string{
		`+const EnvPassword = "POSTA_APP_PASSWORD"`,
		`+const EnvAPIKey = "FIXIT_API_KEY"`,
		`+var passwordEnv = "DB_PASSWORD"`,
	}
	for _, l := range flag {
		if !credentialAssignment(l) {
			t.Errorf("should flag %q", l)
		}
	}
	for _, l := range pass {
		if credentialAssignment(l) {
			t.Errorf("should pass %q", l)
		}
	}
}

// rePasswordSkip knew Python's os.environ and JS's process.env but not Go's,
// Java's or Deno's spelling, so the same "this is the env var's name" line
// flagged in Go and passed in Python.
func TestEnvAccessorSpellingsAreSkipped(t *testing.T) {
	const base = `+	"password": "SMTP_PASSWORD",`
	if !credentialAssignment(base) {
		t.Fatalf("control must flag without an env accessor: %q", base)
	}
	for _, accessor := range []string{"os.Getenv", "System.getenv", "Deno.env.get", "process.env", "os.environ"} {
		if l := base + " // read via " + accessor; credentialAssignment(l) {
			t.Errorf("%s form should pass: %q", accessor, l)
		}
	}
}

func TestCommitCalls(t *testing.T) {
	for _, tc := range []struct {
		cmd, dir string
		worktree bool
		paths    []string
	}{
		{`git commit -m m`, "/cwd", false, nil},
		{`git -C /r -c user.name=x commit -m "a b"`, "/r", false, nil},
		{`cd /x/y && git commit -am wip`, "/x/y", true, nil},
		{`(cd "/a b" && git commit -m m)`, "/a b", false, nil},
		{`git -C sub commit -m m -- a.go b.go`, "/cwd/sub", true, []string{"a.go", "b.go"}},
		{`git commit src/x.go -m m`, "/cwd", true, []string{"src/x.go"}},
		{`git commit -q -F - <<'EOF' 2>&1`, "/cwd", false, nil},
		{`timeout 5 git commit --message x`, "/cwd", false, nil},
	} {
		calls := commitCalls(tc.cmd, "/cwd")
		if len(calls) != 1 {
			t.Errorf("%q: %d commit calls, want 1", tc.cmd, len(calls))
			continue
		}
		c := calls[0]
		if c.dir != tc.dir || c.worktree != tc.worktree || strings.Join(c.paths, " ") != strings.Join(tc.paths, " ") {
			t.Errorf("%q: got dir=%q worktree=%v paths=%v", tc.cmd, c.dir, c.worktree, c.paths)
		}
	}
	for _, cmd := range []string{
		`grep -rn "git commit" docs`,
		`echo "git commit -m x"`,
		`gh pr create --body "run git commit"`,
		`git commit-tree HEAD^{tree}`,
		`ssh box 'git commit -am x'`,
	} {
		if calls := commitCalls(cmd, "/cwd"); len(calls) != 0 {
			t.Errorf("%q is no local commit, got %+v", cmd, calls)
		}
	}
}

// Every way a commit takes content is scanned — `-a` and a pathspec pick up
// an unstaged tracked change, `git -C` and `cd` reach the repo from elsewhere
// — while a commit of nothing sensitive and a mere mention of `git commit`
// pass.
func TestCommitSecretsScansWhatTheCommitTakes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo, "-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	check := func(cmd, cwd string, blocked bool) {
		t.Helper()
		in := &HookInput{CWD: cwd}
		in.ToolInput.Command = cmd
		if d := guardCommitSecrets(in); (d != nil) != blocked {
			t.Errorf("%q from %s: blocked=%v, want %v", cmd, cwd, d != nil, blocked)
		}
	}
	run("init", "-q")
	write("clean\n")
	run("add", "a.txt")
	run("commit", "-q", "-m", "init")

	write("clean\n" + "ghp_" + strings.Repeat("a1", 12) + "\n") // tracked, not staged
	check(`git commit -m x`, repo, false)
	check(`git commit -am x`, repo, true)
	check(`git commit -m x a.txt`, repo, true)
	check(`grep -rn "git commit" .`, repo, false)
	run("add", "a.txt")
	check(`git -C `+repo+` commit -m x`, t.TempDir(), true)
	check(`cd `+repo+` && git commit -m x`, "/", true)
}

// A lookup gh cannot answer (no GitHub remote, offline) is cached as unknown,
// so the next commit does not pay for gh again.
func TestUnknownVisibilityIsCached(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script gh stub")
	}
	bin, gitdir := t.TempDir(), t.TempDir()
	calls := filepath.Join(bin, "calls")
	stub := "#!/bin/sh\necho x >> '" + calls + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	g := func(...string) string { return gitdir }
	for i := 0; i < 2; i++ {
		if vis := repoVisibility(t.TempDir(), g); vis != "" {
			t.Fatalf("visibility %q, want unknown", vis)
		}
	}
	if b, _ := os.ReadFile(calls); strings.Count(string(b), "x") != 1 {
		t.Errorf("gh ran %d times, want 1", strings.Count(string(b), "x"))
	}
}

func TestPrivateIP(t *testing.T) {
	private := []string{"10.0.0.1", "127.0.0.1", "172.16.0.1", "172.31.9.9", "192.168.1.1", "169.254.0.1", "0.0.0.0"}
	public := []string{"95.216.27.220", "8.8.8.8", "172.32.0.1"}
	for _, ip := range private {
		if !rePrivateIP.MatchString(ip) {
			t.Errorf("%s should be private", ip)
		}
	}
	for _, ip := range public {
		if rePrivateIP.MatchString(ip) {
			t.Errorf("%s should be public", ip)
		}
	}
}
