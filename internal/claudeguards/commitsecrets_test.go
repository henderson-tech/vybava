package claudeguards

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestMain keeps every test from spawning the detached visibility refresh: it
// re-executes os.Executable(), which under `go test` is the test binary.
func TestMain(m *testing.M) {
	spawnVisibilityRefresh = func(string, string) {}
	os.Exit(m.Run())
}

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

// The trigger is text-level on purpose: a commit behind a shell keyword,
// eval, a nested shell or a line continuation counts like a plain one.
func TestCommitTrigger(t *testing.T) {
	for _, cmd := range []string{
		`git commit -m m`,
		`git -C /r -c user.name=x commit -m "a b"`,
		`git -c user.name="Claude Code" -c user.email=a@b commit -m x`,
		`git -C "$(git rev-parse --show-toplevel)" commit -am x`,
		`git --no-pager --git-dir=/r/.git --work-tree /r commit -am wip`,
		`if [ -n "$(git status --porcelain)" ]; then git commit -m x; fi`,
		`true && { git commit -m x; }`,
		`eval "git commit -m x"`,
		`bash -c 'cd /r && git commit -m x'`,
		"git diff --cached --stat && \\\n  git commit -m x",
		"git -C /r \\\n  commit -m x",
	} {
		if !reGitCommit.MatchString(strings.ReplaceAll(cmd, "\\\n", " ")) {
			t.Errorf("%q should trigger", cmd)
		}
	}
	for _, cmd := range []string{`git commit-tree HEAD^{tree}`, `git log --grep commit`, `git status && echo commit`} {
		if reGitCommit.MatchString(cmd) {
			t.Errorf("%q should not trigger", cmd)
		}
	}
}

// Every repository the command names is a target, whatever the shell does
// with scoping: cwd, cd and -C (relative ones against cwd and each cd), and
// --git-dir with its work tree.
func TestCommitTargets(t *testing.T) {
	home, _ := os.UserHomeDir()
	has := func(ts []repoTarget, dir string, globals ...string) bool {
		for _, t := range ts {
			if t.dir == dir && strings.Join(t.globals, " ") == strings.Join(globals, " ") {
				return true
			}
		}
		return false
	}
	ts := commitTargets(`(cd "/a b" && git log -1) && git -C sub commit -m x`, "/cwd")
	for _, dir := range []string{"/cwd", "/a b", "/cwd/sub", "/a b/sub"} {
		if !has(ts, dir) {
			t.Errorf("targets lack %s: %+v", dir, ts)
		}
	}
	ts = commitTargets(`git --git-dir $HOME/.cfg --work-tree ~ commit -m x`, "/cwd")
	if !has(ts, home, "--git-dir="+home+"/.cfg", "--work-tree="+home) {
		t.Errorf("bare-repo target missing: %+v", ts)
	}
}

// Fail closed: whatever a commit could take is scanned — an unstaged tracked
// change, an untracked file a same-command `git add` stages, the repo `git -C`
// or `cd` points at — and a clean tree commits.
func TestCommitSecretsFailsClosed(t *testing.T) {
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
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o644); err != nil {
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
	token := "ghp_" + strings.Repeat("a1", 12)
	run("init", "-q")
	write("a.txt", "clean\n")
	run("add", "a.txt")
	run("commit", "-q", "-m", "init")

	write("a.txt", "clean\n"+token+"\n") // tracked, not staged
	check(`git commit -am x`, repo, true)
	check(`git commit -m "$(date)" -- a.txt`, repo, true)
	check(`git -C `+repo+` commit -m x`, t.TempDir(), true)
	check(`(cd `+repo+` && git status) && git commit -m x`, t.TempDir(), true)
	check(`git status`, repo, false)
	write("a.txt", "clean\n")

	write("new.txt", token+"\n") // untracked
	check(`git commit -m x`, repo, false)
	check(`git add new.txt && git commit -m x`, repo, true)
	if err := os.Remove(filepath.Join(repo, "new.txt")); err != nil {
		t.Fatal(err)
	}

	// From a subdirectory `git add -A` still stages the whole repo, and a
	// non-ASCII name is read, not C-quoted away.
	sub := filepath.Join(repo, "apps", "web")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, "nabídka"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join("nabídka", "cfg.txt"), token+"\n")
	check(`git add -A && git commit -m x`, sub, true)
	if err := os.RemoveAll(filepath.Join(repo, "nabídka")); err != nil {
		t.Fatal(err)
	}
	check(`git add -A && git commit -am x`, repo, false)
}

// A lookup gh cannot answer (no GitHub remote, offline, slow) is cached as
// unknown and retried in the background at once, so the next commit neither
// pays for gh nor stays unchecked for long.
func TestUnknownVisibilityIsCached(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script gh stub")
	}
	bin, common := t.TempDir(), t.TempDir()
	calls := filepath.Join(bin, "calls")
	stub := "#!/bin/sh\necho x >> '" + calls + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "gh"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	spawns := 0
	save := spawnVisibilityRefresh
	spawnVisibilityRefresh = func(string, string) { spawns++ }
	t.Cleanup(func() { spawnVisibilityRefresh = save })
	for i := 0; i < 2; i++ {
		if vis := repoVisibility(t.TempDir(), common); vis != "" {
			t.Fatalf("visibility %q, want unknown", vis)
		}
	}
	if b, _ := os.ReadFile(calls); strings.Count(string(b), "x") != 1 || spawns != 1 {
		t.Errorf("gh ran %d times, refresh spawned %d times; want 1 and 1", strings.Count(string(b), "x"), spawns)
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
