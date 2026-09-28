package gittest

import (
	"os/exec"
	"strings"
	"testing"
)

// A repository that asks for auto maintenance still gets none, and config
// entries the environment already carried survive.
func TestNoDaemonsOutranksRepoConfigAndKeepsEarlierEntries(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	for k, v := range map[string]string{
		"GIT_CONFIG_COUNT": "2",
		"GIT_CONFIG_KEY_0": "user.name", "GIT_CONFIG_VALUE_0": "t",
		"GIT_CONFIG_KEY_1": "user.email", "GIT_CONFIG_VALUE_1": "t@example.com",
		// Registered so the entries NoDaemons adds are unset again after the test.
		"GIT_CONFIG_KEY_2": "", "GIT_CONFIG_VALUE_2": "",
		"GIT_CONFIG_KEY_3": "", "GIT_CONFIG_VALUE_3": "",
		"GIT_CONFIG_KEY_4": "", "GIT_CONFIG_VALUE_4": "",
	} {
		t.Setenv(k, v)
	}
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	git("init", "-q")
	git("config", "maintenance.auto", "true")
	git("config", "gc.auto", "1")

	NoDaemons()

	for _, kv := range daemonConfig {
		if got := strings.TrimSpace(git("config", "--get", kv[0])); got != kv[1] {
			t.Errorf("%s = %q, want %q over the repository's own config", kv[0], got, kv[1])
		}
	}
	cmd := exec.Command("git", "-C", repo, "-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "c")
	cmd.Env = append(cmd.Environ(), "GIT_TRACE=1")
	trace, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("commit with the identity carried by the earlier entries: %v\n%s", err, trace)
	}
	if strings.Contains(string(trace), "maintenance") {
		t.Errorf("commit started auto maintenance:\n%s", trace)
	}
}
