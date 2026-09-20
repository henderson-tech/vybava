package claudeguards

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTestWorkerCapPatterns(t *testing.T) {
	block := []string{
		"bunx playwright test",
		"npx playwright test e2e/login.spec.ts",
		"playwright test --project=chromium",
		"node_modules/.bin/playwright test",
		"bunx playwright test --workers 3",
		"bunx playwright test --workers=4",
		"bunx playwright test -j 8",
		"bunx playwright test --workers 50%",
		"npx -p @playwright/test playwright test",
		"bun run playwright test",
		"npm run playwright test",
		"npm exec playwright test",
		"pnpm playwright test",
		"pnpm exec playwright test",
		"yarn playwright test",
		"yarn dlx playwright test",
		"npx vitest run",
		"vitest",
		"vitest --watch",
		"bunx vitest run --maxWorkers 4",
		"bunx vitest run --poolOptions.threads.maxThreads=3",
		"bunx jest",
		"npx jest --maxWorkers=50%",
		"bunx jest -w 3",
		"timeout 600 bunx playwright test",
		"nice -n 5 npx vitest run",
		"env CI=1 bunx jest",
		"CI=1 bunx playwright test",
		"cd apps/web && bunx playwright test",
		"bun install; bunx playwright test",
		"bunx playwright test | tee /tmp/pw.log",
		"bunx playwright test --workers 2 || bunx jest",
		`bash -c "bunx playwright test"`,
	}
	pass := []string{
		"bunx playwright test --workers 2",
		"bunx playwright test --workers=1",
		"bunx playwright test -j 2",
		"bunx playwright test -j2",
		"bunx playwright test --list",
		"bunx playwright test --help",
		"bunx playwright install",
		"bunx playwright codegen",
		"bunx playwright show-report",
		"bunx playwright --version",
		"npx vitest run --maxWorkers 2",
		"npx vitest run --maxWorkers=1",
		"npx vitest run --poolOptions.threads.maxThreads=2",
		"npx vitest run --poolOptions.forks.maxForks 2",
		"npx vitest --no-file-parallelism",
		"npx vitest --version",
		"npx vitest list",
		"bunx jest --maxWorkers 2",
		"bunx jest -w 2",
		"bunx jest --runInBand",
		"bunx jest -i",
		"bunx jest --version",
		"bunx jest --listTests",
		"bun test",
		"bun test --coverage",
		"bun run test",
		"npm test",
		"yarn test",
		"go test ./...",
		"devbox run test",
		"devbox run -- 'bunx playwright test'",
		"devbox run -- bunx playwright test",
		"ssh devulinka-wgadmin 'bunx vitest run'",
		"ssh box bunx jest",
		"devbox run -- 'bash scripts/dev/devbox-web-e2e.sh --grep login'",
		"echo bunx playwright test",
		`git commit -m "bunx playwright test"`,
		"rg 'playwright test' docs",
		"CLAUDE_GUARDS_ALLOW_TEST_WORKERS=1 bunx playwright test",
	}
	for _, c := range block {
		if d := guardTestWorkerCap(hookCmd(c)); d == nil || d.Rule != "machine:test-worker-cap" {
			t.Errorf("should block %q: %v", c, d)
		}
	}
	for _, c := range pass {
		if d := guardTestWorkerCap(hookCmd(c)); d != nil {
			t.Errorf("should pass %q:\n%s", c, d.Text())
		}
	}
}

func hookCmd(cmd string) *HookInput {
	in := &HookInput{CWD: os.TempDir()}
	in.ToolInput.Command = cmd
	return in
}

func TestTestWorkerCapDenialText(t *testing.T) {
	d := guardTestWorkerCap(hookCmd("timeout 600 bunx playwright test --project=chromium"))
	if d == nil {
		t.Fatal("expected a denial")
	}
	for _, want := range []string{
		"🚨 BLOCKED by claude-guards (machine:test-worker-cap)\n\nplaywright test --project=chromium\n\nruns playwright with no worker cap.",
		"40-110% CPU", "devbox run -- '<the same command>'", "devbox-web-e2e.sh",
		"playwright test --workers 2        vitest run --maxWorkers 2        jest --maxWorkers 2",
		"CLAUDE_GUARDS_ALLOW_TEST_WORKERS=1 <command>",
	} {
		if !strings.Contains(d.Text(), want) {
			t.Errorf("stderr must carry %q:\n%s", want, d.Text())
		}
	}
	if d := guardTestWorkerCap(hookCmd("bunx jest --maxWorkers 6")); d == nil || !strings.Contains(d.Message, "runs jest with 6 workers, above this Mac's cap of 2.") {
		t.Fatalf("an over-cap run must say so: %v", d)
	}
}

// guards.testWorkerCap moves the limit through the same loader as noRead; a
// value below 1 is refused and the default stays in force.
func TestTestWorkerCapConfig(t *testing.T) {
	root := t.TempDir()
	in := &HookInput{CWD: root}
	in.ToolInput.Command = "bunx playwright test --workers 4"
	if d := guardTestWorkerCap(in); d == nil {
		t.Fatal("4 workers must block under the default cap")
	}
	write := func(raw string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "vybava.config.json"), []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"guards":{"testWorkerCap":4}}`)
	if d := guardTestWorkerCap(&HookInput{CWD: root, ToolInput: in.ToolInput}); d != nil {
		t.Fatalf("configured cap must pass: %v", d)
	}
	if cfg, err := loadGuardConfig(root); err != nil || cfg.TestWorkerCap != 4 {
		t.Fatalf("cap not loaded: %d %v", cfg.TestWorkerCap, err)
	}
	write(`{"guards":{"testWorkerCap":0}}`)
	if cfg, err := loadGuardConfig(root); err == nil || cfg.TestWorkerCap != defaultTestWorkerCap {
		t.Fatalf("a cap below 1 must be refused: %d %v", cfg.TestWorkerCap, err)
	}
}
