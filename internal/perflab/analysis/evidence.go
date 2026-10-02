// Package analysis is perflab's analyze, compare and report: pure functions
// of stored evidence (an xctrace .trace, framestats dumps, a poll sidecar, a
// Perfetto trace, marks, the runner log), so `perflab analyze --reread`
// recomputes every number from the files alone and compare reads only the
// metrics re-derived here, never a repo's own result JSON. Two reader bugs
// rewrote published numbers in one week (hitch tables summed: 13.92 for
// 2.77 ms/s; a 60-capped FPS judged on a 120 Hz phone); one reader for both
// verbs keeps a third from hiding.
//
// The run lane writes the evidence contract below (perflab.run.json); this
// package only reads it.
package analysis

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RunFileName is the provenance file at the root of every run dir.
const RunFileName = "perflab.run.json"

// RunFileVersion is the perflab.run.json schema version this reader knows.
const RunFileVersion = 1

// Platform is a device platform.
type Platform string

const (
	PlatformIOS     Platform = "ios"
	PlatformAndroid Platform = "android"
)

// InputSource is how the measured interaction reached the screen; adb
// input skips Samsung's touch boost, so it never compares against W3C or a
// human finger.
type InputSource string

const (
	InputADB   InputSource = "adb"
	InputW3C   InputSource = "w3c"
	InputHuman InputSource = "human"
)

// Provenance is what a run measured on: every field compare treats as a
// confounder lives here.
type Provenance struct {
	// Device is the ledger id (never a marketing name).
	Device      string      `json:"device"`
	Platform    Platform    `json:"platform"`
	Model       string      `json:"model,omitempty"`
	OS          string      `json:"os,omitempty"`
	RefreshHz   float64     `json:"refreshHz,omitempty"`
	InputSource InputSource `json:"inputSource"`
}

// Build is the variant a record ran: its native key, the public env hash
// and whether the binary is the as-shipped one (a bundle-swapped variant
// runs with EXUpdatesEnabled NO and is not).
type Build struct {
	Variant              string `json:"variant"`
	NativeKey            string `json:"nativeKey"`
	PublicEnvHash        string `json:"publicEnvHash"`
	ProductionEquivalent bool   `json:"productionEquivalent"`
}

// DeviceState is a snapshot of the device around a record.
type DeviceState struct {
	// ThermalStatus is Android's PowerManager status (0 none .. 6
	// shutdown) or iOS's ProcessInfo.thermalState (0 nominal .. 3
	// critical).
	ThermalStatus *int     `json:"thermalStatus,omitempty"`
	SwapUsedMb    *float64 `json:"swapUsedMb,omitempty"`
}

// Evidence names a record's stored files, relative to the run dir.
type Evidence struct {
	// iOS: the .trace bundle and xctrace record's output.
	Trace     string `json:"trace,omitempty"`
	RecordLog string `json:"recordLog,omitempty"`
	// Android: the poll sidecar, raw framestats dumps, a Perfetto trace.
	Frames  string   `json:"frames,omitempty"`
	Dumps   []string `json:"dumps,omitempty"`
	Pftrace string   `json:"pftrace,omitempty"`
	// Package is the Android app id the evidence belongs to.
	Package string `json:"package,omitempty"`
	// VsyncPeriodNs is the display period read during the run.
	VsyncPeriodNs int64 `json:"vsyncPeriodNs,omitempty"`
	// Steps: a marks file, or the runner log plus the scenario's step cycle
	// (one name per tap) and its groups.
	Marks      string              `json:"marks,omitempty"`
	Log        string              `json:"log,omitempty"`
	StepCycle  []string            `json:"stepCycle,omitempty"`
	StepGroups map[string][]string `json:"stepGroups,omitempty"`
}

// RunRecord is one measured scenario attempt.
type RunRecord struct {
	Scenario   string       `json:"scenario"`
	Attempt    int          `json:"attempt"`
	RecordedAt time.Time    `json:"recordedAt"`
	Build      Build        `json:"build"`
	Evidence   Evidence     `json:"evidence"`
	Before     *DeviceState `json:"before,omitempty"`
	After      *DeviceState `json:"after,omitempty"`
	// Failed marks an attempt the runner did not finish; it is never
	// measured.
	Failed bool `json:"failed,omitempty"`
}

// RunFile is perflab.run.json.
type RunFile struct {
	Version    int         `json:"version"`
	StartedAt  time.Time   `json:"startedAt"`
	Provenance Provenance  `json:"provenance"`
	Runs       []RunRecord `json:"runs"`
	Runner     struct {
		Exit int `json:"exit"`
	} `json:"runner"`
	// Dir is where the file was read from (not stored).
	Dir string `json:"-"`
}

// IsRunDir reports whether dir holds a perflab.run.json.
func IsRunDir(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, RunFileName))
	return err == nil && !st.IsDir()
}

// LoadRunDir reads a run dir's perflab.run.json.
func LoadRunDir(dir string) (RunFile, error) {
	path := filepath.Join(dir, RunFileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return RunFile{}, diag(DiagFileUnreadable, fmt.Sprintf("cannot read %s: %v", path, err), "pass a run dir perflab run wrote")
	}
	var rf RunFile
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rf); err != nil {
		return RunFile{}, diag(DiagFileUnreadable, fmt.Sprintf("%s: %v", path, err), "pass a run dir perflab run wrote")
	}
	if rf.Version != RunFileVersion {
		return RunFile{}, diag(DiagFileUnreadable, fmt.Sprintf("%s has schema version %d, this perflab reads %d", path, rf.Version, RunFileVersion),
			"vybava install perflab (a newer perflab reads it)")
	}
	rf.Dir = dir
	return rf, nil
}

// path resolves an evidence path against the run dir.
func (rf RunFile) path(p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(rf.Dir, p)
}

// RunsLedgerName is the append-only index of run dirs in the state dir:
// one JSON object per line.
const RunsLedgerName = "runs.jsonl"

// LedgerEntry is one runs.jsonl line.
type LedgerEntry struct {
	RunDir    string    `json:"runDir"`
	Device    string    `json:"device"`
	Variants  []string  `json:"variants"`
	Scenarios []string  `json:"scenarios"`
	At        time.Time `json:"at"`
}

// ReadLedger reads runs.jsonl; a missing file is an empty ledger and a
// malformed line is skipped and counted.
func ReadLedger(path string) ([]LedgerEntry, int, error) {
	fh, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, diag(DiagFileUnreadable, fmt.Sprintf("cannot read %s: %v", path, err), "check PERFLAB_STATE_DIR")
	}
	defer fh.Close()
	var out []LedgerEntry
	bad := 0
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e LedgerEntry
		if json.Unmarshal([]byte(line), &e) != nil || e.RunDir == "" {
			bad++
			continue
		}
		out = append(out, e)
	}
	return out, bad, sc.Err()
}

// LatestRunDirs returns, newest first, the run dirs of the ledger that ran
// variant on device (an empty filter matches all).
func LatestRunDirs(entries []LedgerEntry, device, variant string) []string {
	var out []string
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if device != "" && e.Device != device {
			continue
		}
		if variant != "" && !containsStr(e.Variants, variant) {
			continue
		}
		out = append(out, e.RunDir)
	}
	return out
}

func containsStr(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
