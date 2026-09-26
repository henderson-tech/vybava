package claudeguards

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// tempRepo makes a git repo with one staged file and returns its path.
func tempRepo(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		c := exec.Command("git", append([]string{"-C", dir}, args...)...)
		c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", name)
	return dir
}

func TestGuardCommitSecrets(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	in := func(dir, cmd string) *HookInput {
		h := &HookInput{CWD: dir}
		h.ToolInput.Command = cmd
		return h
	}
	secret := tempRepo(t, "config.ts", "export const key = 'AKIAIOSFODNN7EXAMPLE';\n")
	if d := guardCommitSecrets(in(secret, `git commit -m "add config"`)); d == nil || d.Rule != "commit-secrets" {
		t.Fatalf("staged AWS key must block, got %v", d)
	} else if strings.Contains(d.Message, "AKIAIOSFODNN7EXAMPLE") || !strings.Contains(d.Message, "[REDACTED:aws-key]") {
		t.Fatalf("the denial must quote the line without the key (it lands in the transcript):\n%s", d.Message)
	}
	if d := guardCommitSecrets(in(secret, `COMMIT_GUARD_ALLOW=1 git commit -m "add config"`)); d != nil {
		t.Fatalf("escape hatch must pass, got %v", d)
	}
	if d := guardCommitSecrets(in(secret, `git status`)); d != nil {
		t.Fatalf("non-commit command must pass, got %v", d)
	}
	keyFile := tempRepo(t, "deploy.pem", "-----BEGIN RSA PRIVATE KEY-----\nabc\n")
	if d := guardCommitSecrets(in(keyFile, `git commit -m "keys"`)); d == nil {
		t.Fatal("staged key file must block")
	}
	clean := tempRepo(t, "main.go", "package main\n")
	if d := guardCommitSecrets(in(clean, `git commit -m "ok"`)); d != nil {
		t.Fatalf("clean diff must pass, got %v", d)
	}
	if d := guardCommitSecrets(in(t.TempDir(), `git commit -m "x"`)); d != nil {
		t.Fatalf("non-repo dir must pass, got %v", d)
	}
}
