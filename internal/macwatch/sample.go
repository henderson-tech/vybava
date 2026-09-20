package macwatch

import (
	"context"
	"fmt"
	"math"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Runner executes one command and returns its stdout. Every system read goes
// through it so tests can replay captured output on any platform.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs commands with exec; stdout is returned even when the
// command exits non-zero (lsof does that for a vanished pid while still
// printing the rest).
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if err != nil && len(out) > 0 {
		return out, nil
	}
	return out, err
}

// Sampler reads one Sample per call. Zero values fall back to the defaults
// the prototype used: top 12 by CPU at or above 3 %, plus top 4 by RSS.
type Sampler struct {
	Run          Runner
	Home         string // shortened out of commands and cwds
	ProjectsRoot string // default <Home>/Work/Projects
	Now          func() time.Time
	TopCPU       int
	CPUFloor     float64
	TopRSS       int
}

const (
	defaultTopCPU   = 12
	defaultCPUFloor = 3.0
	defaultTopRSS   = 4
	ownerHops       = 8
	cmdWidth        = 80
)

type proc struct {
	pid, ppid int
	cpu       float64
	rssKB     int
	etime     string
	tty       string
	command   string
}

func (p proc) exe() string {
	fields := strings.Fields(p.command)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// Sample takes one system row and its process rows.
func (s Sampler) Sample(ctx context.Context) (Sample, error) {
	s = s.withDefaults()
	at := s.Now()
	var sample Sample
	sample.System.At = at

	load, err := s.Run(ctx, "sysctl", "-n", "vm.loadavg")
	if err != nil {
		return sample, fmt.Errorf("sysctl vm.loadavg: %w", err)
	}
	sample.System.Load1, sample.System.Load5, sample.System.Load15 = parseLoadavg(string(load))

	top, err := s.Run(ctx, "top", "-l", "1", "-n", "0")
	if err != nil {
		return sample, fmt.Errorf("top: %w", err)
	}
	parseTop(string(top), &sample.System)

	psOut, err := s.Run(ctx, "ps", "-axo", "pid=,ppid=,%cpu=,rss=,etime=,tty=,command=")
	if err != nil {
		return sample, fmt.Errorf("ps: %w", err)
	}
	table := parsePS(string(psOut))
	countSuspects(table, &sample.System)

	if booted, err := s.Run(ctx, "xcrun", "simctl", "list", "devices", "booted"); err == nil {
		sample.System.Sims = strings.Count(string(booted), "Booted")
	}

	byPID := make(map[int]proc, len(table))
	for _, p := range table {
		byPID[p.pid] = p
	}
	selected := s.selectProcs(table)
	owners := make(map[int]owner, len(selected))
	want := make(map[int]struct{}, len(selected)*2)
	for _, p := range selected {
		o := ownerOf(byPID, p.pid)
		owners[p.pid] = o
		want[p.pid] = struct{}{}
		if o.pid != 0 {
			want[o.pid] = struct{}{}
		}
	}
	cwds := s.cwds(ctx, want)

	for _, p := range selected {
		o := owners[p.pid]
		cwd := cwds[p.pid]
		project := s.projectOf(cwd)
		if project == "-" && o.pid != 0 {
			project = s.projectOf(cwds[o.pid])
		}
		row := ProcessRow{
			At: at, PID: p.pid, PPID: p.ppid, CPU: p.cpu, RSSMB: p.rssKB / 1024,
			Etime: p.etime, TTY: p.tty,
			OwnerKind: o.kind, OwnerPID: o.pid, OwnerTTY: o.tty,
			Project: project, Cwd: cwd, Cmd: s.shortenCmd(p.command),
		}
		if o.kind == "" {
			row.OwnerKind, row.OwnerTTY = "-", "-"
		}
		if row.Cwd == "" {
			row.Cwd = "-"
		}
		sample.Processes = append(sample.Processes, row)
	}
	return sample, nil
}

func (s Sampler) withDefaults() Sampler {
	if s.Run == nil {
		s.Run = ExecRunner
	}
	if s.Now == nil {
		s.Now = time.Now
	}
	if s.ProjectsRoot == "" {
		s.ProjectsRoot = filepath.Join(s.Home, "Work", "Projects")
	}
	if s.TopCPU == 0 {
		s.TopCPU = defaultTopCPU
	}
	if s.CPUFloor == 0 {
		s.CPUFloor = defaultCPUFloor
	}
	if s.TopRSS == 0 {
		s.TopRSS = defaultTopRSS
	}
	return s
}

func parseLoadavg(s string) (float64, float64, float64) {
	fields := strings.Fields(strings.Trim(strings.TrimSpace(s), "{} "))
	var v [3]float64
	for i := 0; i < 3 && i < len(fields); i++ {
		v[i], _ = strconv.ParseFloat(fields[i], 64)
	}
	return v[0], v[1], v[2]
}

var (
	processesLine = regexp.MustCompile(`^Processes:\s+(\d+) total,\s+(\d+) running,.*?(\d+) threads`)
	physMemLine   = regexp.MustCompile(`^PhysMem:\s+(\S+) used \((?:.*?, )?(\S+) compressor\),\s+(\S+) unused`)
)

func parseTop(out string, row *SystemRow) {
	for _, line := range strings.Split(out, "\n") {
		if m := processesLine.FindStringSubmatch(line); m != nil {
			row.Procs, _ = strconv.Atoi(m[1])
			row.Running, _ = strconv.Atoi(m[2])
			row.Threads, _ = strconv.Atoi(m[3])
		}
		if m := physMemLine.FindStringSubmatch(line); m != nil {
			row.UsedGB = gigabytes(m[1])
			row.CompressorGB = gigabytes(m[2])
			row.FreeGB = gigabytes(m[3])
		}
	}
}

// gigabytes converts top's "93G" / "2613M" / "512K" to GB with two decimals.
func gigabytes(s string) float64 {
	if s == "" {
		return 0
	}
	unit := s[len(s)-1]
	v, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil {
		return 0
	}
	switch unit {
	case 'K':
		v /= 1024 * 1024
	case 'M':
		v /= 1024
	case 'T':
		v *= 1024
	}
	return math.Round(v*100) / 100
}

func parsePS(out string) []proc {
	var table []proc
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		cpu, _ := strconv.ParseFloat(fields[2], 64)
		rss, _ := strconv.Atoi(fields[3])
		table = append(table, proc{
			pid: pid, ppid: ppid, cpu: cpu, rssKB: rss, etime: fields[4], tty: fields[5],
			command: strings.Join(fields[6:], " "),
		})
	}
	return table
}

func countSuspects(table []proc, row *SystemRow) {
	for _, p := range table {
		switch filepath.Base(p.exe()) {
		case "claude":
			row.Claude++
		case "codex":
			row.Codex++
		case "java":
			row.Java++
		}
		if strings.Contains(p.command, "xcodebuild") {
			row.Xcodebuild++
		}
		if strings.Contains(p.command, "chrome-headless-shell") {
			row.Chrome++
		}
		if strings.Contains(p.command, "tsc") {
			row.Tsc++
		}
	}
}

// selectProcs is the prototype's cut: top TopCPU by CPU at or above CPUFloor,
// plus top TopRSS by RSS, deduplicated and ordered by pid.
func (s Sampler) selectProcs(table []proc) []proc {
	byCPU := append([]proc(nil), table...)
	sort.SliceStable(byCPU, func(i, j int) bool { return byCPU[i].cpu > byCPU[j].cpu })
	byRSS := append([]proc(nil), table...)
	sort.SliceStable(byRSS, func(i, j int) bool { return byRSS[i].rssKB > byRSS[j].rssKB })

	picked := map[int]proc{}
	for i := 0; i < len(byCPU) && i < s.TopCPU; i++ {
		if byCPU[i].cpu < s.CPUFloor {
			break
		}
		picked[byCPU[i].pid] = byCPU[i]
	}
	for i := 0; i < len(byRSS) && i < s.TopRSS; i++ {
		picked[byRSS[i].pid] = byRSS[i]
	}
	out := make([]proc, 0, len(picked))
	for _, p := range picked {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].pid < out[j].pid })
	return out
}

type owner struct {
	kind string
	pid  int
	tty  string
}

// ownerOf walks up to ownerHops parents looking for the nearest claude or
// codex process; that is the session the work belongs to.
func ownerOf(byPID map[int]proc, pid int) owner {
	p, ok := byPID[pid]
	for hop := 0; ok && hop < ownerHops; hop++ {
		if p.ppid <= 1 {
			break
		}
		p, ok = byPID[p.ppid]
		if !ok {
			break
		}
		switch filepath.Base(p.exe()) {
		case "claude", "codex":
			return owner{kind: filepath.Base(p.exe()), pid: p.pid, tty: p.tty}
		}
	}
	return owner{}
}

// cwds reads the working directory of every wanted pid in ONE lsof call.
func (s Sampler) cwds(ctx context.Context, want map[int]struct{}) map[int]string {
	result := map[int]string{}
	if len(want) == 0 {
		return result
	}
	pids := make([]string, 0, len(want))
	for pid := range want {
		pids = append(pids, strconv.Itoa(pid))
	}
	sort.Strings(pids)
	out, err := s.Run(ctx, "lsof", "-a", "-d", "cwd", "-Fpn", "-p", strings.Join(pids, ","))
	if err != nil && len(out) == 0 {
		return result
	}
	return parseLsof(string(out))
}

func parseLsof(out string) map[int]string {
	result := map[int]string{}
	pid := 0
	for _, line := range strings.Split(out, "\n") {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
		case 'n':
			if pid != 0 {
				if _, seen := result[pid]; !seen {
					result[pid] = line[1:]
				}
			}
		}
	}
	return result
}

// projectOf reduces a directory under ProjectsRoot to `<org>/<repo>` or
// `<org>/<repo>:<worktree-slug>`; anything else is "-".
func (s Sampler) projectOf(cwd string) string {
	return ProjectLabel(s.ProjectsRoot, cwd)
}

// ProjectLabel is projectOf with an explicit root.
func ProjectLabel(root, cwd string) string {
	if root == "" || cwd == "" {
		return "-"
	}
	root = strings.TrimSuffix(root, "/") + "/"
	if !strings.HasPrefix(cwd, root) {
		return "-"
	}
	rel := strings.TrimPrefix(cwd, root)
	if idx := strings.Index(rel, ".worktrees/"); idx >= 0 {
		prefix := strings.TrimSuffix(rel[:idx], "/")
		rest := rel[idx+len(".worktrees/"):]
		slug, _, _ := strings.Cut(rest, "/")
		if prefix == "" || slug == "" {
			return "-"
		}
		return prefix + ":" + slug
	}
	parts := strings.SplitN(rel, "/", 3)
	if len(parts) < 2 || parts[1] == "" {
		return "-"
	}
	return parts[0] + "/" + parts[1]
}

var systemPaths = regexp.MustCompile(`/System/Library/[^ ]*/|/Applications/|/Library/Developer/CoreSimulator/[^ ]*/`)

// shortenCmd drops the long path prefixes the prototype dropped and cuts the
// command to cmdWidth runes.
func (s Sampler) shortenCmd(command string) string {
	if s.Home != "" {
		command = stripHomePaths(command, s.Home)
	}
	command = systemPaths.ReplaceAllString(command, "")
	if r := []rune(command); len(r) > cmdWidth {
		command = string(r[:cmdWidth])
	}
	return command
}

// stripHomePaths removes every `<home>/…/` directory prefix (up to the last
// slash before the next space) so only the file name of each argument stays.
func stripHomePaths(command, home string) string {
	prefix := strings.TrimSuffix(home, "/") + "/"
	var b strings.Builder
	for {
		start := strings.Index(command, prefix)
		if start < 0 {
			b.WriteString(command)
			return b.String()
		}
		end := strings.IndexByte(command[start:], ' ')
		if end < 0 {
			end = len(command)
		} else {
			end += start
		}
		lastSlash := strings.LastIndexByte(command[start:end], '/')
		b.WriteString(command[:start])
		command = command[start+lastSlash+1:]
	}
}
