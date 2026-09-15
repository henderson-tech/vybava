package plugingc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// defaultGrace is how long a marker may lag its own process's start time
// before the gap stops being clock jitter and starts meaning a recycled PID.
const defaultGrace = 2 * time.Minute

// Process is one live process, as much of it as a PID refcount needs.
type Process struct {
	PID int
	// Started is when the kernel says the process began. A marker written
	// BEFORE its own process started is impossible, so a later start is proof
	// the PID was recycled.
	Started time.Time
	// Command is the executable path ps reports.
	Command string
}

// ProcessTable is the live process table, keyed by PID.
type ProcessTable map[int]Process

// ProcessLister reads the live process table.
type ProcessLister func(ctx context.Context) (ProcessTable, error)

// sessions counts the processes that could plausibly hold a plugin marker.
func (t ProcessTable) sessions() int {
	count := 0
	for _, process := range t {
		if filepath.Base(process.Command) == claudeBinary {
			count++
		}
	}
	return count
}

const claudeBinary = "claude"

// Liveness is the verdict on one marker's PID.
type Liveness string

const (
	// LivenessHeld is a live Claude Code session. The marker stays, always.
	LivenessHeld Liveness = "held"
	// LivenessGone is a PID with no process behind it.
	LivenessGone Liveness = "gone"
	// LivenessForeign is a PID held by something that plainly cannot be a
	// Claude Code session — the PID was recycled by an unrelated program.
	LivenessForeign Liveness = "foreign"
	// LivenessRecycled is a PID held by a process that started after the
	// marker was written, so it cannot be the process that wrote it.
	LivenessRecycled Liveness = "recycled"
)

// Dead reports whether the marker may be swept. Only the three proven-dead
// verdicts qualify: anything undecidable stays held, because a wrongly swept
// marker un-refcounts a running session.
func (l Liveness) Dead() bool {
	return l == LivenessGone || l == LivenessForeign || l == LivenessRecycled
}

// Marker is one .in_use/<pid> file.
type Marker struct {
	PID      int       `json:"pid"`
	Path     string    `json:"path"`
	Written  time.Time `json:"written"`
	Liveness Liveness  `json:"liveness"`
	// Command is what currently holds the PID, when anything does.
	Command string `json:"command,omitempty"`
	// Recorded is the process start time the marker itself claims, kept for
	// the human report; the verdict uses the file's own mtime, which needs no
	// timezone to be true.
	Recorded string `json:"recorded_start,omitempty"`
}

// markerBody is what Claude Code writes into a marker file.
type markerBody struct {
	PID       int    `json:"pid"`
	ProcStart string `json:"procStart"`
}

// readMarkers reads a version's refcount directory and judges every marker.
func readMarkers(versionPath string, view processView, grace time.Duration) ([]Marker, int, int) {
	dir := filepath.Join(versionPath, MarkerDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, 0
	}
	var markers []Marker
	var live, dead int
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			// Not a PID marker; it is not ours to judge and not ours to
			// delete. Its presence alone keeps the version held.
			live++
			continue
		}
		marker := Marker{PID: pid, Path: filepath.Join(dir, entry.Name())}
		if info, err := entry.Info(); err == nil {
			marker.Written = info.ModTime()
		}
		if body, err := os.ReadFile(marker.Path); err == nil {
			var parsed markerBody
			if json.Unmarshal(body, &parsed) == nil {
				marker.Recorded = parsed.ProcStart
			}
		}
		marker.Liveness, marker.Command = view.classify(marker, grace)
		if marker.Liveness.Dead() {
			dead++
		} else {
			live++
		}
		markers = append(markers, marker)
	}
	sort.Slice(markers, func(i, j int) bool { return markers[i].PID < markers[j].PID })
	return markers, live, dead
}

// processView is the process table plus whether it could be read at all. The
// distinction is load-bearing: an EMPTY table means every PID is gone, an
// UNREADABLE one means nothing is known — and reading the second as the first
// would call every marker on the machine dead.
type processView struct {
	table ProcessTable
	known bool
}

// classify decides whether a marker's PID is still the process that wrote it.
//
// The trap this exists for: kill(pid, 0) succeeds for ANY live process and
// macOS recycles PIDs freely, so "the PID exists" proves nothing. Three
// further checks turn it into proof, and every one fails CLOSED — an
// undecidable marker is held, never swept.
func (v processView) classify(marker Marker, grace time.Duration) (Liveness, string) {
	if !v.known {
		return LivenessHeld, ""
	}
	process, running := v.table[marker.PID]
	if !running {
		return LivenessGone, ""
	}
	if !couldHoldPlugins(process.Command) {
		return LivenessForeign, process.Command
	}
	if !process.Started.IsZero() && !marker.Written.IsZero() && process.Started.After(marker.Written.Add(grace)) {
		return LivenessRecycled, process.Command
	}
	return LivenessHeld, process.Command
}

// couldHoldPlugins reports whether a command could be a Claude Code session.
// It answers generously on purpose: Claude Code runs as the `claude` binary
// here, but a JS-runtime host is a shape it has shipped in, and calling one
// of those foreign would sweep a live session's marker. Everything else —
// a browser, an editor, a daemon — is a recycled PID with certainty.
func couldHoldPlugins(command string) bool {
	base := strings.ToLower(filepath.Base(command))
	if strings.Contains(base, claudeBinary) {
		return true
	}
	switch base {
	case "node", "bun", "deno", "npx", "bunx":
		return true
	}
	return false
}

// processTable reads the live process table through the injected lister,
// defaulting to ps.
func (env Env) processTable(ctx context.Context) (ProcessTable, error) {
	lister := env.Processes
	if lister == nil {
		lister = PSProcesses
	}
	return lister(ctx)
}

// PSProcesses reads every process on the machine in one `ps` call. Output is
// `pid lstart comm`, where lstart is a five-token ctime stamp in local time,
// so the command is whatever follows the fifth token and may contain spaces.
func PSProcesses(ctx context.Context) (ProcessTable, error) {
	out, err := exec.CommandContext(ctx, "ps", "-Ao", "pid=,lstart=,comm=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	return ParsePS(string(out), time.Local), nil
}

// ParsePS turns `ps -Ao pid=,lstart=,comm=` output into a table. A line it
// cannot read is dropped, which leaves its PID absent — so the parser must
// never be fed output from a different ps format, or live sessions read as
// gone. Every caller that cannot produce this exact format returns an error
// instead, which disables the sweep.
func ParsePS(out string, loc *time.Location) ProcessTable {
	table := ProcessTable{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 7 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		started, err := time.ParseInLocation(ctimeLayout, strings.Join(fields[1:6], " "), loc)
		if err != nil {
			// Keep the process: a PID we can see is never "gone".
			started = time.Time{}
		}
		table[pid] = Process{PID: pid, Started: started, Command: strings.Join(fields[6:], " ")}
	}
	return table
}

// ctimeLayout matches ps's lstart once strings.Fields has collapsed the day
// padding ("Sep  8" becomes two tokens either way).
const ctimeLayout = "Mon Jan 2 15:04:05 2006"

// installed is the parsed installed_plugins.json: the ONLY authority on which
// version of a plugin is live.
type installed struct {
	Version int                          `json:"version"`
	Plugins map[string][]installedRecord `json:"plugins"`
}

type installedRecord struct {
	Scope       string `json:"scope"`
	InstallPath string `json:"installPath"`
	Version     string `json:"version"`
}

func loadInstalled(home string) (installed, error) {
	body, err := os.ReadFile(filepath.Join(home, "installed_plugins.json"))
	if err != nil {
		return installed{}, err
	}
	var record installed
	if err := json.Unmarshal(body, &record); err != nil {
		return installed{}, err
	}
	return record, nil
}

// isActive answers by comparing install paths, never by comparing version
// strings: a plugin's versions include git shas next to semver, and sorting
// them lexically puts 3.11.0 above 5.3.0.
func (r installed) isActive(key, versionPath string) bool {
	want, err := filepath.Abs(versionPath)
	if err != nil {
		want = versionPath
	}
	for _, record := range r.Plugins[key] {
		if record.InstallPath == "" {
			continue
		}
		have, err := filepath.Abs(record.InstallPath)
		if err != nil {
			have = record.InstallPath
		}
		if filepath.Clean(have) == filepath.Clean(want) {
			return true
		}
	}
	return false
}

// activeVersions names the versions the install record points at, for the
// report.
func (r installed) activeVersions(key string) []string {
	var names []string
	for _, record := range r.Plugins[key] {
		name := record.Version
		if name == "" && record.InstallPath != "" {
			name = filepath.Base(record.InstallPath)
		}
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

// DefaultHome is ~/.claude/plugins.
func DefaultHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, ".claude", "plugins")
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("no Claude Code plugin home at %s", path)
		}
		return "", err
	}
	return path, nil
}
