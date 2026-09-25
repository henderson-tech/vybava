package claudeguards

// Machine snapshot — the process/memory read behind `weather`,
// `machine:sim-cap`, `machine:dev-server-cap` and `reap`. The `ps -axo` fork
// dominates its cost (docs/claude-guards.md).
//
// On 2026-09-19/20 the Mac sat at the memory ceiling (95 of 96 GB used,
// 36-49 GB in the compressor) with 4 booted simulators, 3 Metro bundlers and
// 4 API dev servers that the Devbox rule already routed elsewhere, under 55
// Claude sessions. No session knew any of that when it started. This file
// gives them the number: one `ps`, one `sysctl` and one `vm_stat`, no lsof,
// no top.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/transcripts"
)

// machineProc is one row of `ps -axo pid=,ppid=,rss=,etime=,tty=,args=`.
type machineProc struct {
	pid, ppid, rssKB int
	etime, tty, args string
}

// base is the command's basename (first args field).
// exe is the executable path as ps printed it (argv[0]); base its file name.
func (p machineProc) exe() string {
	f := strings.Fields(p.args)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

func (p machineProc) base() string {
	return filepath.Base(p.exe())
}

// machineProcTable reads the live process table; tests inject their own.
var machineProcTable = func() []machineProc {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,rss=,etime=,tty=,args=").Output()
	if err != nil {
		return nil
	}
	return parseProcTable(string(out))
}

func parseProcTable(out string) []machineProc {
	var table []machineProc
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 6 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		rss, err3 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		table = append(table, machineProc{pid: pid, ppid: ppid, rssKB: rss, etime: f[3], tty: f[4], args: strings.Join(f[5:], " ")})
	}
	return table
}

// etimeSeconds parses ps's elapsed time: `[[dd-]hh:]mm:ss`.
func etimeSeconds(s string) (int, bool) {
	days := 0
	if i := strings.IndexByte(s, '-'); i >= 0 {
		d, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, false
		}
		days, s = d, s[i+1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, false
		}
		total = total*60 + n
	}
	return days*86400 + total, true
}

// Process kinds the machine rules care about.
const (
	kindSim      = "sim"      // one launchd_sim per booted iOS simulator
	kindEmulator = "emulator" // Android emulator (qemu)
	kindMetro    = "metro"    // Expo / React Native bundler
	kindNext     = "next"     // next dev / next-server
	kindAPI      = "api"      // nest start, tsx watch, bun --watch/--hot, turbo dev
	kindClaude   = "claude"
	kindCodex    = "codex"
)

var (
	metroArgs = regexp.MustCompile(`(^|[/ ])(expo(/bin/cli(\.js)?)?|react-native(/cli\.js)?|metro) start( |$)`)
	nextArgs  = regexp.MustCompile(`(^|[/ ])(next(/dist/bin/next)? dev( |$)|next-server)`)
	apiArgs   = regexp.MustCompile(`(^|[/ ])(nest start|tsx watch|turbo(/bin/turbo)? dev|bun (run )?--(watch|hot))( |$)`)
)

// procKind classifies one process, or "" when it is none of ours.
func procKind(p machineProc) string {
	b := p.base()
	switch {
	case b == "launchd_sim":
		return kindSim
	case strings.HasPrefix(b, "qemu-system") || (b == "emulator" && strings.Contains(p.args, "-avd")):
		return kindEmulator
	case b == "claude":
		return kindClaude
	case b == "codex":
		return kindCodex
	case nextArgs.MatchString(p.args):
		return kindNext
	case metroArgs.MatchString(p.args):
		return kindMetro
	case apiArgs.MatchString(p.args):
		return kindAPI
	}
	return ""
}

// machineCounts is what the table holds right now. Which sessions are idle
// is a separate, costlier read (idle.go), made only under pressure.
type machineCounts struct {
	Sims, Emulators, Metro, Next, API, Claude, Codex int
	Servers                                          []machineProc // every counted dev server (metro/next/api)
}

func (c machineCounts) sims() int       { return c.Sims + c.Emulators }
func (c machineCounts) devServers() int { return c.Metro + c.Next + c.API }

var devKinds = map[string]bool{kindMetro: true, kindNext: true, kindAPI: true}

// countMachine classifies the table. A dev server whose ancestor is itself a
// counted dev server (turbo dev → next dev, bun run dev → nest start) counts
// once, at the leaf.
func countMachine(table []machineProc) machineCounts {
	kinds := make(map[int]string, len(table))
	byPID := make(map[int]machineProc, len(table))
	for _, p := range table {
		byPID[p.pid] = p
		if k := procKind(p); k != "" {
			kinds[p.pid] = k
		}
	}
	var c machineCounts
	for _, p := range table {
		k := kinds[p.pid]
		switch k {
		case kindSim:
			c.Sims++
		case kindEmulator:
			c.Emulators++
		case kindClaude, kindCodex:
			if k == kindClaude {
				c.Claude++
			} else {
				c.Codex++
			}
		case kindMetro, kindNext, kindAPI:
			if hasDevDescendant(p.pid, byPID, kinds) {
				continue // the leaf counts, not the orchestrator
			}
			switch k {
			case kindMetro:
				c.Metro++
			case kindNext:
				c.Next++
			default:
				c.API++
			}
			c.Servers = append(c.Servers, p)
		}
	}
	sort.Slice(c.Servers, func(i, j int) bool { return c.Servers[i].pid < c.Servers[j].pid })
	return c
}

// hasDevDescendant reports whether any process under pid is a dev server.
func hasDevDescendant(pid int, byPID map[int]machineProc, kinds map[int]string) bool {
	for cpid, k := range kinds {
		if !devKinds[k] || cpid == pid {
			continue
		}
		for cur, hops := byPID[cpid].ppid, 0; cur > 1 && hops < 12; hops++ {
			if cur == pid {
				return true
			}
			cur = byPID[cur].ppid
		}
	}
	return false
}

// ownedBySession reports whether a live claude/codex process is an ancestor
// of pid within the table.
func ownedBySession(pid int, byPID map[int]machineProc) bool {
	for cur, hops := byPID[pid].ppid, 0; cur > 1 && hops < 12; hops++ {
		p, ok := byPID[cur]
		if !ok {
			return false
		}
		if b := p.base(); b == kindClaude || b == kindCodex {
			return true
		}
		cur = p.ppid
	}
	return false
}

// machineStats is the sysctl/vm_stat side of the snapshot.
type machineStats struct {
	Cores                         int
	Load1                         float64
	TotalGB, FreeGB, CompressorGB float64
}

// readMachineStats reads the live numbers; tests inject their own.
var readMachineStats = func() (machineStats, error) {
	var s machineStats
	out, err := exec.Command("sysctl", "-n", "hw.memsize", "hw.ncpu", "vm.loadavg").Output()
	if err != nil {
		return s, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 3 {
		return s, fmt.Errorf("sysctl: %d lines", len(lines))
	}
	mem, err := strconv.ParseFloat(strings.TrimSpace(lines[0]), 64)
	if err != nil {
		return s, err
	}
	s.TotalGB = mem / (1 << 30)
	if s.Cores, err = strconv.Atoi(strings.TrimSpace(lines[1])); err != nil {
		return s, err
	}
	if f := strings.Fields(strings.Trim(lines[2], "{} ")); len(f) > 0 {
		if s.Load1, err = strconv.ParseFloat(f[0], 64); err != nil {
			return s, err
		}
	}
	vm, err := exec.Command("vm_stat").Output()
	if err != nil {
		return s, err
	}
	s.FreeGB, s.CompressorGB, err = parseVMStat(string(vm))
	return s, err
}

var vmPageSize = regexp.MustCompile(`page size of (\d+) bytes`)

// parseVMStat returns free (free + speculative) and compressor GB.
func parseVMStat(out string) (freeGB, compressorGB float64, err error) {
	m := vmPageSize.FindStringSubmatch(out)
	if m == nil {
		return 0, 0, fmt.Errorf("vm_stat: no page size")
	}
	page, _ := strconv.ParseFloat(m[1], 64)
	pages := func(label string) float64 {
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, label) {
				v := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, label), "."))
				n, _ := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(v, ":")), 64)
				return n
			}
		}
		return 0
	}
	free := pages("Pages free") + pages("Pages speculative")
	comp := pages("Pages occupied by compressor")
	return free * page / (1 << 30), comp * page / (1 << 30), nil
}

// Weather prints the machine-pressure line. The hook form (text=false) is
// what SessionStart injects into the model's context: one line always, a
// second only under pressure, naming the idle sessions (idle.go). --text adds
// them as a table for a human. Any sampling failure prints nothing: a weather
// report must never brick a session start. With reap (the SessionStart hook form) the same
// table then feeds the orphan sweep: one `ps` per session start, not two.
func Weather(text, reap bool, w, stderr io.Writer) error {
	table := machineProcTable()
	if table == nil {
		return nil
	}
	if reap {
		defer reapTable(table, stderr)
	}
	stats, err := readMachineStats()
	if err != nil {
		return nil
	}
	c := countMachine(table)
	cwd, _ := os.Getwd()
	cfg := guardConfig(cwd)
	fmt.Fprint(w, weatherLine(stats, c))
	if !pressured(stats, c, cfg) && !text {
		return nil
	}
	// The hook reads at most 4 MiB of transcripts per session start and
	// finishes a big one over later starts; a human asking gets all of it.
	budget := int64(transcripts.DefaultBudget)
	if text {
		budget = 1 << 30
	}
	var idle idleReport
	if home, err := os.UserHomeDir(); err == nil {
		facts, flush := liveSessionFacts(home, budget, cronCachePath())
		idle = idleSessions(table, time.Now(), facts)
		flush()
	}
	if pressure := weatherPressure(stats, c, cfg, idle); pressure != "" {
		fmt.Fprint(w, pressure)
	}
	if text {
		weatherIdleTable(w, idle)
	}
	return nil
}

// weatherIdleTable is --text's list. It names; it never kills or parks.
func weatherIdleTable(w io.Writer, idle idleReport) {
	if len(idle.Idle) > 0 {
		fmt.Fprintf(w, "\nClaude sessions idle for 10 h+, no background task, teammate or cron (pid · idle · RSS with children · session · project):\n")
		for _, s := range idle.Idle {
			fmt.Fprintf(w, "  %d · %s · %s · %.8s · %s\n", s.pid, fmtIdle(s.idleFor), fmtKB(s.rssKB), s.sessionID, s.project)
		}
	}
	var held []string
	for _, h := range []sessionHold{holdTask, holdTeammates, holdCron, holdUndecided} {
		if n := idle.Held[h]; n > 0 {
			held = append(held, fmt.Sprintf("%d %s", n, h))
		}
	}
	if len(held) > 0 {
		fmt.Fprintf(w, "Kept off the list: %s.\n", strings.Join(held, ", "))
	}
}

func weatherLine(s machineStats, c machineCounts) string {
	return fmt.Sprintf("🌡️ Mac: load %.1f on %d cores · free %.1f GB, compressor %.0f GB of %.0f · %d claude + %d codex sessions · %d sims · %d metro · %d next · %d api\n",
		s.Load1, s.Cores, s.FreeGB, s.CompressorGB, s.TotalGB, c.Claude, c.Codex, c.sims(), c.Metro, c.Next, c.API)
}

func pressured(s machineStats, c machineCounts, cfg Config) bool {
	return s.FreeGB < 2 || s.Load1 > float64(s.Cores) || c.sims() >= cfg.SimCap || c.devServers() >= cfg.DevServerCap
}

func weatherPressure(s machineStats, c machineCounts, cfg Config, idle idleReport) string {
	if !pressured(s, c, cfg) {
		return ""
	}
	return "⚠️ At the memory ceiling: start nothing heavy here. Dev servers and suites go to the Devbox (devbox run -- '<cmd>'), idle worktrees get paused (/wk:pause)" + idle.idleClause() + ".\n"
}

// shortArgs trims a command line for a block message: home stripped, at most
// n runes.
func shortArgs(args string, n int) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		args = strings.ReplaceAll(args, home, "~")
	}
	if r := []rune(args); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return args
}
