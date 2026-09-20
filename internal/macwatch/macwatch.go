// Package macwatch samples what is loading a Mac and attributes every heavy
// process to a project, a directory and the Claude/Codex session that owns
// it. macOS keeps no per-process CPU history, so the sampler appends one
// system row and a handful of process rows to a TSV file every interval, and
// the report integrates that file into offenders, projects, sessions and a
// spike ledger.
//
// The TSV shape is shared with the bash prototype that preceded this applet
// (docs/macwatch.md), so the report reads either file.
package macwatch

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// TimeLayout is the timestamp format of every row (local time, no zone).
const TimeLayout = "2006-01-02T15:04:05"

// SystemRow is one `S` line: load, process counts, memory and the counts of
// the usual suspects at that instant.
type SystemRow struct {
	At           time.Time `json:"at"`
	Load1        float64   `json:"load1"`
	Load5        float64   `json:"load5"`
	Load15       float64   `json:"load15"`
	Procs        int       `json:"procs"`
	Running      int       `json:"running"`
	Threads      int       `json:"threads"`
	UsedGB       float64   `json:"usedGb"`
	FreeGB       float64   `json:"freeGb"`
	CompressorGB float64   `json:"compressorGb"`
	Claude       int       `json:"claude"`
	Codex        int       `json:"codex"`
	Sims         int       `json:"sims"`
	Xcodebuild   int       `json:"xcodebuild"`
	Chrome       int       `json:"chrome"`
	Tsc          int       `json:"tsc"`
	Java         int       `json:"java"`
}

// ProcessRow is one `P` line: a process that made the top-by-CPU or
// top-by-RSS cut at that instant, with its owner session and project.
type ProcessRow struct {
	At        time.Time `json:"at"`
	PID       int       `json:"pid"`
	PPID      int       `json:"ppid"`
	CPU       float64   `json:"cpu"`
	RSSMB     int       `json:"rssMb"`
	Etime     string    `json:"etime"`
	TTY       string    `json:"tty"`
	OwnerKind string    `json:"ownerKind"`
	OwnerPID  int       `json:"ownerPid"`
	OwnerTTY  string    `json:"ownerTty"`
	Project   string    `json:"project"`
	Cwd       string    `json:"cwd"`
	Cmd       string    `json:"cmd"`
}

// Sample is one system row with the process rows taken alongside it.
type Sample struct {
	System    SystemRow    `json:"system"`
	Processes []ProcessRow `json:"processes"`
}

// File is a parsed TSV: samples in file order plus whether the DONE trailer
// was seen.
type File struct {
	Samples []Sample
	Done    bool
}

// Owner names the session a process belongs to: "claude 123 (ttys004)",
// or "-" when no claude/codex ancestor exists.
func (p ProcessRow) Owner() string {
	if p.OwnerKind == "" || p.OwnerKind == "-" {
		return "-"
	}
	return fmt.Sprintf("%s %d (%s)", p.OwnerKind, p.OwnerPID, p.OwnerTTY)
}

// TSV renders the row exactly as the prototype did.
func (r SystemRow) TSV() string {
	return strings.Join([]string{
		"S", r.At.Format(TimeLayout),
		ftoa(r.Load1), ftoa(r.Load5), ftoa(r.Load15),
		strconv.Itoa(r.Procs), strconv.Itoa(r.Running), strconv.Itoa(r.Threads),
		ftoa(r.UsedGB), ftoa(r.FreeGB), ftoa(r.CompressorGB),
		strconv.Itoa(r.Claude), strconv.Itoa(r.Codex), strconv.Itoa(r.Sims),
		strconv.Itoa(r.Xcodebuild), strconv.Itoa(r.Chrome), strconv.Itoa(r.Tsc), strconv.Itoa(r.Java),
	}, "\t")
}

// TSV renders the row exactly as the prototype did.
func (p ProcessRow) TSV() string {
	ownerPID := "-"
	if p.OwnerKind != "-" && p.OwnerKind != "" {
		ownerPID = strconv.Itoa(p.OwnerPID)
	}
	return strings.Join([]string{
		"P", p.At.Format(TimeLayout),
		strconv.Itoa(p.PID), strconv.Itoa(p.PPID), ftoa(p.CPU), strconv.Itoa(p.RSSMB),
		p.Etime, p.TTY, dash(p.OwnerKind), ownerPID, dash(p.OwnerTTY),
		dash(p.Project), dash(p.Cwd), cell(p.Cmd),
	}, "\t")
}

// DoneLine is the trailer the sampler appends when a run completes.
func DoneLine(at time.Time, samples int) string {
	return fmt.Sprintf("DONE %s %d samples", at.Format(TimeLayout), samples)
}

// Read parses a TSV written by this applet or by the bash prototype. Process
// rows attach to the system row that precedes them; unknown lines and blank
// lines are skipped, a malformed known row is an error.
func Read(r io.Reader) (File, error) {
	var file File
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		switch {
		case strings.HasPrefix(text, "S\t"):
			row, err := parseSystem(strings.Split(text, "\t"))
			if err != nil {
				return file, fmt.Errorf("line %d: %w", line, err)
			}
			file.Samples = append(file.Samples, Sample{System: row})
		case strings.HasPrefix(text, "P\t"):
			row, err := parseProcess(strings.Split(text, "\t"))
			if err != nil {
				return file, fmt.Errorf("line %d: %w", line, err)
			}
			if len(file.Samples) == 0 {
				file.Samples = append(file.Samples, Sample{System: SystemRow{At: row.At}})
			}
			last := &file.Samples[len(file.Samples)-1]
			last.Processes = append(last.Processes, row)
		case strings.HasPrefix(text, "DONE"):
			file.Done = true
		}
	}
	if err := scanner.Err(); err != nil {
		return file, err
	}
	return file, nil
}

func parseSystem(fields []string) (SystemRow, error) {
	if len(fields) < 18 {
		return SystemRow{}, fmt.Errorf("system row has %d fields, want 18", len(fields))
	}
	at, err := time.ParseInLocation(TimeLayout, fields[1], time.Local)
	if err != nil {
		return SystemRow{}, err
	}
	p := &numbers{}
	row := SystemRow{
		At:    at,
		Load1: p.float(fields[2]), Load5: p.float(fields[3]), Load15: p.float(fields[4]),
		Procs: p.int(fields[5]), Running: p.int(fields[6]), Threads: p.int(fields[7]),
		UsedGB: p.float(fields[8]), FreeGB: p.float(fields[9]), CompressorGB: p.float(fields[10]),
		Claude: p.int(fields[11]), Codex: p.int(fields[12]), Sims: p.int(fields[13]),
		Xcodebuild: p.int(fields[14]), Chrome: p.int(fields[15]), Tsc: p.int(fields[16]), Java: p.int(fields[17]),
	}
	return row, p.err
}

func parseProcess(fields []string) (ProcessRow, error) {
	if len(fields) < 14 {
		return ProcessRow{}, fmt.Errorf("process row has %d fields, want 14", len(fields))
	}
	at, err := time.ParseInLocation(TimeLayout, fields[1], time.Local)
	if err != nil {
		return ProcessRow{}, err
	}
	p := &numbers{}
	row := ProcessRow{
		At:  at,
		PID: p.int(fields[2]), PPID: p.int(fields[3]), CPU: p.float(fields[4]), RSSMB: p.int(fields[5]),
		Etime: fields[6], TTY: fields[7],
		OwnerKind: fields[8], OwnerPID: p.int(fields[9]), OwnerTTY: fields[10],
		// The prototype's sed left "repo:/slug"; this applet writes "repo:slug".
		Project: strings.Replace(fields[11], ":/", ":", 1),
		Cwd:     fields[12], Cmd: strings.Join(fields[13:], " "),
	}
	return row, p.err
}

// numbers parses the prototype's numeric cells, which may be empty, "-", or
// an unevaluated "2613/1024" (megabytes the bash script left as a division).
type numbers struct{ err error }

func (n *numbers) float(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return 0
	}
	if num, den, ok := strings.Cut(s, "/"); ok {
		return n.float(num) / n.float(den)
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil && n.err == nil {
		n.err = fmt.Errorf("number %q: %w", s, err)
	}
	return v
}

func (n *numbers) int(s string) int {
	s = strings.TrimSpace(s)
	if s == "" || s == "-" {
		return 0
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return int(n.float(s))
	}
	return v
}

func ftoa(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return cell(s)
}

// cell keeps a value on one line and inside its column.
func cell(s string) string {
	s = strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s)
	if s == "" {
		return "-"
	}
	return s
}
