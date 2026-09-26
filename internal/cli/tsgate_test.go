package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"testing"
)

func TestTsgateRunsConfiguredProgramsAndPassesTheExitCode(t *testing.T) {
	root := t.TempDir()
	platform := map[string]string{"windows": "win32"}[goruntime.GOOS]
	if platform == "" {
		platform = goruntime.GOOS
	}
	arch := map[string]string{"amd64": "x64"}[goruntime.GOARCH]
	if arch == "" {
		arch = goruntime.GOARCH
	}
	files := map[string]string{
		".git/HEAD":                             "ref: refs/heads/main\n",
		"vybava.config.json":                    `{"tsgate":{"programs":["app/tsconfig.json"]}}`,
		"app/tsconfig.json":                     `{"compilerOptions":{"module":"esnext","moduleResolution":"bundler"}}`,
		"node_modules/typescript7/package.json": `{"name":"typescript","version":"7.0.2"}`,
		"node_modules/@typescript/typescript-" + platform + "-" + arch + "/lib/tsc": "#!/bin/sh\necho checked \"$@\"\nexit 2\n",
	}
	for name, body := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(root)

	var stdout, stderr bytes.Buffer
	rt := &runtime{stdout: &stdout, stderr: &stderr}
	cmd := rt.tsgateCommand("tsgate")
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); ExitCode(err) != 2 || ErrorText(err) != "" {
		t.Fatalf("exit %d (%v), stdout %q, stderr %q", ExitCode(err), err, stdout.String(), stderr.String())
	}
	if want := "checked -p " + filepath.Join(root, "app/tsconfig.json") + " --noEmit\n"; stdout.String() != want {
		t.Errorf("stdout %q, want %q", stdout.String(), want)
	}

	stdout.Reset()
	rt.json = true
	cmd = rt.tsgateCommand("tsgate")
	cmd.SetArgs([]string{"plan", "app/tsconfig.json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var plans []struct {
		Derive   bool `json:"derive"`
		Compiler struct {
			Dependency string `json:"dependency"`
			Version    string `json:"version"`
		} `json:"compiler"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &plans); err != nil || len(plans) != 1 || plans[0].Derive || plans[0].Compiler.Dependency != "typescript7" || plans[0].Compiler.Version != "7.0.2" {
		t.Fatalf("plan --json = %s (%v)", stdout.String(), err)
	}
}
