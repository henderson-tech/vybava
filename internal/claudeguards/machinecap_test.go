package claudeguards

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withProcTable(t *testing.T, table []machineProc) {
	t.Helper()
	save := machineProcTable
	machineProcTable = func() []machineProc { return table }
	t.Cleanup(func() { machineProcTable = save })
}

func TestStartKind(t *testing.T) {
	dir := t.TempDir() // no package.json: a dev:* script stays a start
	for cmd, want := range map[string]string{
		"xcrun simctl boot ABC":                         kindSim,
		"bunx expo run:ios --device X":                  kindSim,
		"bun run run:sim:ios":                           kindSim,
		"bunx tsx scripts/dev/run.ts sim --pair":        kindSim,
		"emulator -avd Pixel_8":                         kindSim,
		"bunx expo start --dev-client":                  "dev",
		"bun run dev:api":                               "dev",
		"bun run dev":                                   "dev",
		"bun run --filter @fixit/web dev":               "dev",
		"npm run dev":                                   "dev",
		"pnpm dev":                                      "dev",
		"bunx next dev":                                 "dev",
		"turbo dev --filter=@fixit/api":                 "dev",
		"nest start --watch":                            "dev",
		"bunx vite":                                     "dev",
		"bunx tsx scripts/dev/run.ts api":               "dev",
		"xcrun simctl list devices booted":              "",
		"xcrun simctl io ABC screenshot /tmp/a.png":     "",
		"bun run typecheck":                             "",
		"bun run test:unit":                             "",
		"bunx vite build":                               "",
		"bunx next build":                               "",
		"bunx tsx scripts/dev/run.ts --help":            "",
		"bun run dev:mobile --help && echo done":        "dev",
		"cd apps/api && timeout 600 bun run dev:api":    "dev",
		"echo 'bun run dev:api'":                        "",
		"devbox run -- 'bun run dev:api'":               "",
		"ssh box 'bunx expo start'":                     "",
		"git commit -m 'xcrun simctl boot in the loop'": "",
	} {
		if _, got := machineStartMatch(cmd, dir); got != want {
			t.Errorf("%q: got %q want %q", cmd, got, want)
		}
	}
}

// A dev:* script is a dev-server start only when its package.json body is
// one, read where the command runs (`cd` followed).
func TestDevScriptKindReadsTheBody(t *testing.T) {
	dir := t.TempDir()
	pkg := `{"scripts": {
		"dev:api": "nest start --watch",
		"dev:web": "bun run dev:api",
		"dev:server": "bun --watch src/main.ts",
		"dev:all": "turbo run dev --parallel",
		"dev:hmr": "WEBPACK_HMR=true webpack-cli build & sleep 2 && node --enable-source-maps dist/main.js",
		"dev:export-structure": "tree -I node_modules > structure.txt",
		"dev:claude:usage": "bunx ccusage@latest",
		"dev:seed": "bunx tsx scripts/dev-seed/index.ts prep"
	}}`
	if err := os.MkdirAll(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app", "package.json"), []byte(pkg), 0o644); err != nil {
		t.Fatal(err)
	}
	for cmd, want := range map[string]string{
		"cd app && bun run dev:api":              "dev",
		"cd app && bun run dev:web":              "dev",
		"cd app && bun run dev:server":           "dev",
		"cd app && bun run dev:all":              "dev",
		"cd app && bun run dev:hmr":              "dev",
		"cd app && bun run dev:export-structure": "",
		"cd app && bun run dev:claude:usage":     "",
		"cd app && bun run dev:seed":             "",
		"bun run dev:export-structure":           "dev", // no package.json here
	} {
		if _, got := machineStartMatch(cmd, dir); got != want {
			t.Errorf("%q: got %q want %q", cmd, got, want)
		}
	}
}

func TestGuardMachineCap(t *testing.T) {
	withProcTable(t, fakeTable) // 2 sims, 3 dev servers: both at the default caps
	block := map[string]string{
		"xcrun simctl boot ABC":        "machine:sim-cap",
		"bun run run:sim:ios":          "machine:sim-cap",
		"bun run dev:api":              "machine:dev-server-cap",
		"cd apps/web && bunx next dev": "machine:dev-server-cap",
	}
	for c, rule := range block {
		if d := guardMachineCap(hookCmd(c)); d == nil || d.Rule != rule {
			t.Errorf("should block %q with %s: %v", c, rule, d)
		}
	}
	for _, c := range []string{
		"xcrun simctl shutdown ABC",
		"bun run typecheck",
		"CLAUDE_GUARDS_ALLOW_MACHINE_CAP=1 xcrun simctl boot ABC",
		"CLAUDE_GUARDS_ALLOW_MACHINE_CAP=1 bun run dev:api",
		"devbox run -- 'bun run dev:api'",
	} {
		if d := guardMachineCap(hookCmd(c)); d != nil {
			t.Errorf("should pass %q:\n%s", c, d.Text())
		}
	}
	d := guardMachineCap(hookCmd("bun run dev:api"))
	for _, want := range []string{"already runs 3 (cap 3", "next-server", "/wk:pause", "devbox run --", "CLAUDE_GUARDS_ALLOW_MACHINE_CAP=1"} {
		if !strings.Contains(d.Text(), want) {
			t.Errorf("message lacks %q:\n%s", want, d.Text())
		}
	}
	withProcTable(t, fakeTable[:1]) // a quiet Mac
	for _, c := range []string{"xcrun simctl boot ABC", "bun run dev:api"} {
		if d := guardMachineCap(hookCmd(c)); d != nil {
			t.Errorf("quiet Mac blocked %q:\n%s", c, d.Text())
		}
	}
	withProcTable(t, nil) // no table: fail open
	if d := guardMachineCap(hookCmd("xcrun simctl boot ABC")); d != nil {
		t.Errorf("no table should fail open: %s", d.Text())
	}
}
