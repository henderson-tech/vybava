package claudeguards

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ---------------------------------------------------------------------------
// machine:sim-cap / machine:dev-server-cap - a simulator boot or a dev-server
// start on a Mac that already holds its share. On 2026-09-19 four booted
// simulators (about 250 daemons each plus a 1.4-2 GB dev client), three Metro
// bundlers and four API dev servers sat on this machine - roughly 25 GB and a
// thousand processes - while the memory compressor held 36-49 GB of 96. The
// rule reads the process table only when the command IS a boot or a start, so
// every other Bash call costs nothing. A `dev:*` package script is a start only
// when its package.json body is one (`dev:export-structure` → `tree` is not).
// A deliberate extra instance on a quiet Mac sets
// CLAUDE_GUARDS_ALLOW_MACHINE_CAP=1.
// ---------------------------------------------------------------------------

const (
	defaultSimCap       = 2
	defaultDevServerCap = 3
)

// machineStarters are the command words a boot or start chain begins with.
var machineStarters = map[string]bool{
	"xcrun": true, "expo": true, "emulator": true, "next": true, "turbo": true,
	"nest": true, "vite": true, "tsx": true,
	"bunx": true, "npx": true, "bun": true, "npm": true, "pnpm": true, "yarn": true,
}

var devScripts = map[string]bool{"dev": true, "run:api": true, "run:web": true, "run:admin": true}

// kindDevScript is a `dev:*` package script, which its body classifies
// (devScriptKind).
const kindDevScript = "dev-script"

// startKind classifies one argv as a simulator boot ("sim"), a dev-server
// start ("dev") or neither. Package launchers are unwrapped the way the
// worker-cap rule does it, then the script name or the tool decides.
func startKind(argv []string) string {
	for len(argv) > 0 {
		w := argv[0]
		if j := strings.LastIndexByte(w, '/'); j >= 0 {
			w = w[j+1:]
		}
		args := argv[1:]
		switch w {
		case "xcrun":
			if len(args) >= 2 && args[0] == "simctl" && args[1] == "boot" {
				return kindSim
			}
			return ""
		case "expo":
			if len(args) == 0 {
				return ""
			}
			switch args[0] {
			case "run:ios", "run:android":
				return kindSim
			case "start":
				return "dev"
			}
			return ""
		case "emulator":
			for _, a := range args {
				if a == "-avd" || a == "@" || strings.HasPrefix(a, "@") {
					return kindSim
				}
			}
			return ""
		case "next", "turbo":
			if len(args) > 0 && args[0] == "dev" {
				return "dev"
			}
			return ""
		case "nest":
			if len(args) > 0 && args[0] == "start" {
				return "dev"
			}
			return ""
		case "vite":
			if len(args) == 0 || args[0] == "dev" || args[0] == "serve" || strings.HasPrefix(args[0], "-") {
				return "dev"
			}
			return ""
		case "tsx", "node":
			if len(args) == 0 {
				return ""
			}
			return scriptKind(args)
		}
		if !packageLaunchers[w] {
			return ""
		}
		payload := launcherPayload(w, skipFilter(args))
		if len(payload) == 0 {
			return ""
		}
		if k := scriptKind(payload); k != "" {
			return k
		}
		argv = payload
	}
	return ""
}

// skipFilter drops `--filter <pkg>` / `--filter=<pkg>` pairs so
// `bun run --filter @fixit/web dev` reads as `dev`.
func skipFilter(argv []string) []string {
	var out []string
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "--filter" || a == "-F" {
			i++
			continue
		}
		if strings.HasPrefix(a, "--filter=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// scriptKind reads a package script name or a scripts/dev/run.ts verb.
func scriptKind(payload []string) string {
	name := payload[0]
	switch {
	case strings.HasPrefix(name, "run:sim:"):
		return kindSim
	case devScripts[name]:
		return "dev"
	case strings.HasPrefix(name, "dev:"):
		return kindDevScript
	}
	if filepath.Base(name) == "run.ts" && len(payload) > 1 {
		switch payload[1] {
		case "sim":
			return kindSim
		case "api", "web", "admin":
			return "dev"
		}
	}
	return ""
}

// machineStartMatch returns the first local segment that boots a simulator or
// starts a dev server, with its kind; cwd, followed through `cd`, is where a
// `dev:*` script's package.json is read.
func machineStartMatch(cmd, cwd string) (segment, kind string) {
	home, _ := os.UserHomeDir()
	for _, seg := range localSegments(cmd) {
		if f := shellFields(seg); len(f) > 1 && f[0] == "cd" {
			cwd = resolveDir(f[1], cwd, home)
			continue
		}
		if textOnly(seg) {
			continue
		}
		argv := chainCommand(trimSubshell(seg), machineStarters)
		if argv == nil {
			continue
		}
		k := startKind(argv)
		if k == kindDevScript {
			k = devScriptKind(argv, cwd)
		}
		if k != "" {
			return strings.TrimSpace(trimAssignments(trimSubshell(seg))), k
		}
	}
	return "", ""
}

// devScriptKind classifies the `dev:*` script argv runs by its body in cwd's
// package.json. It errs toward counting: a script it cannot read stays a
// dev-server start, and so does a body that runs another `dev:*` script or
// has a server word (`bun --watch`, `nx serve`, `turbo run dev`); only a body
// with none (`tree …`, `bunx ccusage`) is not a start.
func devScriptKind(argv []string, cwd string) string {
	name := ""
	for _, a := range argv {
		if strings.HasPrefix(a, "dev:") {
			name = a
			break
		}
	}
	body, ok := packageScript(cwd, name)
	if !ok {
		return "dev"
	}
	for _, seg := range localSegments(body) {
		if a := chainCommand(trimSubshell(seg), machineStarters); a != nil {
			switch k := startKind(a); k {
			case "":
			case kindDevScript:
				return "dev"
			default:
				return k
			}
		}
	}
	for _, w := range strings.Fields(body) {
		if w = strings.Trim(w, `"'`); serverWords[w] || strings.HasPrefix(w, "dev:") {
			return "dev"
		}
	}
	return ""
}

// serverWords mark a script body as a long-running server whatever tool runs it.
var serverWords = map[string]bool{"dev": true, "serve": true, "start": true, "watch": true, "--watch": true, "--hot": true}

func packageScript(dir, name string) (string, bool) {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return "", false
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(b, &pkg) != nil {
		return "", false
	}
	body, ok := pkg.Scripts[name]
	return body, ok
}

const machineCapEscape = "A deliberate extra instance on a quiet Mac: CLAUDE_GUARDS_ALLOW_MACHINE_CAP=1 <command>"

func guardMachineCap(in *HookInput) *Denial {
	cmd := in.ToolInput.Command
	if cmd == "" || escapeHatch(cmd, "CLAUDE_GUARDS_ALLOW_MACHINE_CAP") {
		return nil
	}
	seg, kind := machineStartMatch(cmd, in.CWD)
	if kind == "" {
		return nil
	}
	table := machineProcTable()
	if table == nil {
		return nil // no process table: fail open
	}
	c := countMachine(table)
	cfg := in.guards()
	if kind == kindSim {
		if c.sims() < cfg.SimCap {
			return nil
		}
		return deny("machine:sim-cap", fmt.Sprintf(`%s

boots another simulator while this Mac already runs %d (cap %d: guards.simCap).
Each booted simulator is ~250 iOS daemons plus a 1.4-2 GB dev client; on
2026-09-19 four of them, with 3 Metro and 4 API servers, held ~25 GB and a
thousand processes at the memory ceiling.

Pause what you are not using: /wk:pause (FixIt) or
    xcrun simctl shutdown <udid>       xcrun simctl list devices booted`, seg, c.sims(), cfg.SimCap), machineCapEscape)
	}
	if c.devServers() < cfg.DevServerCap {
		return nil
	}
	var running strings.Builder
	for _, p := range c.Servers {
		fmt.Fprintf(&running, "    %d · %s · %s\n", p.pid, shortArgs(p.args, 60), p.etime)
	}
	return deny("machine:dev-server-cap", fmt.Sprintf(`%s

starts another dev server while this Mac already runs %d (cap %d: guards.devServerCap):
%s
Pause what you are not using: /wk:pause (FixIt) or kill <pid>. Web/API dev
servers and suites belong on the Devbox: devbox run -- '<the same command>'.`, seg, c.devServers(), cfg.DevServerCap, running.String()), machineCapEscape)
}
