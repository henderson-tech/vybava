package claudeguards

import (
	"fmt"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// machine:test-worker-cap - a browser or unit test runner started on this Mac
// with no worker cap fans out one browser or worker per core. On 2026-09-19
// six headless Chromes at 40-110% CPU each pinned the machine while 28 Claude
// sessions shared it, and every other session stalled. Heavy suites belong on
// the Devbox; a run that must happen here is capped (`--workers 2`,
// `--maxWorkers 2`), and a deliberate full-parallel run on a quiet Mac sets
// CLAUDE_GUARDS_ALLOW_TEST_WORKERS=1.
// ---------------------------------------------------------------------------

// defaultTestWorkerCap is the highest worker count a local runner may ask for
// when vybava.config.ts sets no guards.testWorkerCap.
const defaultTestWorkerCap = 2

// testRunners are the runners this rule knows how to read a worker cap from.
// `bun test` is not one: it has no worker flag and runs in one process.
var testRunners = map[string]bool{"playwright": true, "vitest": true, "jest": true}

// packageLaunchers execute a package binary named in their arguments.
var packageLaunchers = map[string]bool{
	"bunx": true, "npx": true, "bun": true, "npm": true, "pnpm": true, "yarn": true,
}

// launcherValueFlags take the next token as their value, so that token is
// never the tool (`npx -p @playwright/test playwright test`).
var launcherValueFlags = map[string]bool{"-p": true, "--package": true, "-c": true, "--call": true}

// serialFlags run the suite in one worker, which is the strictest cap.
var serialFlags = map[string]map[string]bool{
	"vitest": {"--no-file-parallelism": true, "--fileParallelism=false": true, "--file-parallelism=false": true},
	"jest":   {"--runInBand": true, "-i": true},
}

// capFlags name a worker count; short forms also accept the glued `-j4`.
var capFlags = map[string]map[string]bool{
	"playwright": {"--workers": true, "-j": true},
	"vitest":     {"--maxWorkers": true, "--max-workers": true},
	"jest":       {"--maxWorkers": true, "--max-workers": true, "-w": true},
}

// inertFlags mean the runner runs no tests at all.
var inertFlags = map[string]map[string]bool{
	"playwright": {"--help": true, "-h": true, "--list": true},
	"vitest":     {"--help": true, "-h": true, "--version": true, "-v": true},
	"jest": {"--help": true, "-h": true, "--version": true, "-v": true,
		"--listTests": true, "--showConfig": true, "--clearCache": true, "--init": true},
}

// inertVitestCommands are vitest subcommands that run no tests.
var inertVitestCommands = map[string]bool{"list": true, "init": true}

// testRunnerArgv unwraps package launchers (`bunx playwright`, `npm run jest`,
// `pnpm exec vitest`) down to the runner and returns its argv, or nil when the
// chain runs something else. `bun test` is bun's own runner and never matches.
func testRunnerArgv(argv []string) []string {
	for len(argv) > 0 {
		w := argv[0]
		if j := strings.LastIndexByte(w, '/'); j >= 0 {
			w = w[j+1:]
		}
		if testRunners[w] {
			return append([]string{w}, argv[1:]...)
		}
		if !packageLaunchers[w] {
			return nil
		}
		argv = launcherPayload(w, argv[1:])
	}
	return nil
}

// launcherPayload returns the command a package launcher runs, or nil.
func launcherPayload(launcher string, rest []string) []string {
	switch launcher {
	case "bun":
		if len(rest) == 0 || rest[0] == "test" {
			return nil
		}
		if rest[0] == "run" || rest[0] == "x" {
			rest = rest[1:]
		}
	case "npm":
		if len(rest) == 0 || (rest[0] != "run" && rest[0] != "run-script" && rest[0] != "exec" && rest[0] != "x") {
			return nil
		}
		rest = rest[1:]
	case "pnpm", "yarn":
		if len(rest) > 0 && (rest[0] == "run" || rest[0] == "exec" || rest[0] == "dlx") {
			rest = rest[1:]
		}
	}
	for len(rest) > 0 && strings.HasPrefix(rest[0], "-") {
		if launcherValueFlags[rest[0]] && len(rest) > 1 {
			rest = rest[1:]
		}
		rest = rest[1:]
	}
	return rest
}

// workerCount reads the runner's argv. It returns -1 when nothing bounds the
// workers, 0 when the run is serial, else the requested count; inert reports
// an invocation that runs no tests (`--version`, `--list`, `playwright
// install`). A non-numeric value (`--workers 50%`) is no cap.
func workerCount(argv []string) (workers int, inert bool) {
	runner, args := argv[0], argv[1:]
	switch runner {
	case "playwright":
		if len(args) == 0 || args[0] != "test" {
			return -1, true
		}
		args = args[1:]
	case "vitest":
		if len(args) > 0 && inertVitestCommands[args[0]] {
			return -1, true
		}
	}
	workers = -1
	for i := 0; i < len(args); i++ {
		a := args[i]
		if inertFlags[runner][a] {
			return -1, true
		}
		if serialFlags[runner][a] {
			workers = 0
			continue
		}
		name, val, hasVal := a, "", false
		if j := strings.IndexByte(a, '='); j >= 0 {
			name, val, hasVal = a[:j], a[j+1:], true
		}
		switch {
		case capFlags[runner][name] || strings.HasPrefix(name, "--poolOptions.") && (strings.HasSuffix(name, ".maxThreads") || strings.HasSuffix(name, ".maxForks")):
			if !hasVal && i+1 < len(args) {
				i++
				val = args[i]
			}
		case len(name) > 2 && capFlags[runner][name[:2]] && isDigits(name[2:]):
			val = name[2:]
		default:
			continue
		}
		if n, err := strconv.Atoi(val); err == nil && n >= 0 {
			workers = n
		} else {
			workers = -1
		}
	}
	return workers, false
}

// testWorkerMatch is one uncapped or over-capped local runner invocation.
type testWorkerMatch struct {
	argv    string
	runner  string
	workers int
}

// testWorkerCapMatch returns the first local test runner invocation in cmd
// that runs with no worker cap or with more workers than max, or nil.
func testWorkerCapMatch(cmd string, max int) *testWorkerMatch {
	names := make(map[string]bool, len(testRunners)+len(packageLaunchers))
	for n := range testRunners {
		names[n] = true
	}
	for n := range packageLaunchers {
		names[n] = true
	}
	for _, seg := range localSegments(cmd) {
		if textOnly(seg) {
			continue
		}
		argv := testRunnerArgv(chainCommand(seg, names))
		if argv == nil {
			continue
		}
		workers, inert := workerCount(argv)
		if inert || (workers >= 0 && workers <= max) {
			continue
		}
		return &testWorkerMatch{argv: strings.Join(argv, " "), runner: argv[0], workers: workers}
	}
	return nil
}

const testWorkerCapMsg = `%s

runs %s. On this Mac a test runner fans out one
browser or worker per core: 6 headless Chromes at 40-110%% CPU each pinned
the machine on 2026-09-19 while 28 Claude sessions shared it, and every
other session stalled.

Heavy suites belong on the Devbox, where they cost this Mac nothing:
    devbox run -- '<the same command>'            (FixIt: devbox run test,
    scripts/dev/devbox-web-e2e.sh --grep <re>, scripts/dev/devbox-admin-e2e.sh)
Must run here? Cap it:
    playwright test --workers %[3]d        vitest run --maxWorkers %[3]d        jest --maxWorkers %[3]d`

const testWorkerCapEscape = "A deliberate full-parallel run on a quiet Mac: CLAUDE_GUARDS_ALLOW_TEST_WORKERS=1 <command>"

func guardTestWorkerCap(in *HookInput) *Denial {
	cmd := in.ToolInput.Command
	if cmd == "" || escapeHatch(cmd, "CLAUDE_GUARDS_ALLOW_TEST_WORKERS") {
		return nil
	}
	max := in.guards().TestWorkerCap
	m := testWorkerCapMatch(cmd, max)
	if m == nil {
		return nil
	}
	how := m.runner + " with no worker cap"
	if m.workers > max {
		how = fmt.Sprintf("%s with %d workers, above this Mac's cap of %d", m.runner, m.workers, max)
	}
	return deny("machine:test-worker-cap", fmt.Sprintf(testWorkerCapMsg, m.argv, how, max), testWorkerCapEscape)
}
