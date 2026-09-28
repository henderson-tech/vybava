package reconcile

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// StatusReport is the read-only view `status --json`, the per-box page and
// the hub all share.
type StatusReport struct {
	Repo            string         `json:"repo"`
	HostLabel       string         `json:"host_label"`
	Clone           string         `json:"clone"`
	GeneratedAt     time.Time      `json:"generated_at"`
	Version         string         `json:"vybava_version,omitempty"`
	VersionMismatch string         `json:"version_mismatch,omitempty"`
	Mode            string         `json:"mode"`
	Commit          string         `json:"commit"`
	CommitSubject   string         `json:"commit_subject,omitempty"`
	LastGood        string         `json:"last_good,omitempty"`
	LastGoodSubject string         `json:"last_good_subject,omitempty"`
	Pin             string         `json:"pin,omitempty"`
	Sync            string         `json:"sync"` // in-sync | pending | held | errors
	Pending         []string       `json:"pending"`
	Held            []string       `json:"held"`
	CertHeld        []string       `json:"cert_held"` // TLS vhosts whose certificate is not on the box yet
	SkippedApps     []string       `json:"skipped_apps"`
	Errors          []Issue        `json:"errors"`
	LastTick        *HistoryEntry  `json:"last_tick,omitempty"`
	TickAge         string         `json:"tick_age,omitempty"`
	History         []HistoryEntry `json:"history"`
}

// StatusReport computes the read-only report: a report-mode sweep of the
// checkout as it stands plus what the state directory already knows.
func (e *Engine) StatusReport(historyLimit int) (StatusReport, error) {
	res, err := e.Status()
	if err != nil {
		return StatusReport{}, err
	}
	st := e.state()
	rep := StatusReport{
		Repo: e.M.Repo, HostLabel: e.M.HostLabel, Clone: e.M.Clone, GeneratedAt: e.now(),
		Version: e.Version, VersionMismatch: e.versionMismatch(),
		Mode: res.Mode, Commit: res.Commit, CommitSubject: res.CommitSubject,
		LastGood: st.LastGood(), Pin: st.Pin(),
		Pending: res.Pending, Held: res.Held, CertHeld: res.CertHeld, SkippedApps: res.SkippedApps, Errors: res.Errors,
		History: []HistoryEntry{},
	}
	if rep.LastGood != "" {
		rep.LastGoodSubject = e.git().subject(rep.LastGood)
	}
	switch {
	case len(rep.Errors) > 0:
		rep.Sync = "errors"
	case len(rep.Held) > 0 || len(rep.CertHeld) > 0:
		rep.Sync = "held"
	case len(rep.Pending) > 0:
		rep.Sync = "pending"
	default:
		rep.Sync = "in-sync"
	}
	history, err := st.History(historyLimit)
	if err != nil {
		return rep, err
	}
	if len(history) > 0 {
		rep.History = history
		for i := range history {
			if history[i].Action != "force" {
				rep.LastTick = &history[i]
				rep.TickAge = e.now().Sub(history[i].Time).Truncate(time.Second).String()
				break
			}
		}
	}
	return rep, nil
}

// Diff returns the unified diff live → repo for one mapped, tracked file.
// Only git-tracked, mapped paths are accepted; the live path comes from the
// mapping, never from the caller.
func (e *Engine) Diff(rp string) (string, error) {
	if filepath.IsAbs(rp) || strings.Contains("/"+rp+"/", "/../") {
		return "", errors.New("invalid path")
	}
	g := e.git()
	if !g.tracks(rp) {
		return "", fmt.Errorf("not a git-tracked repo file: %s", rp)
	}
	t, ok := e.M.MapPath(rp)
	if !ok {
		return "", fmt.Errorf("not a mapped path: %s", rp)
	}
	live := t.Dest
	if !isRegular(live) {
		live = "/dev/null"
	}
	cmd := exec.Command("git", "diff", "--no-index", "--no-color", "--", live, filepath.Join(e.M.Clone, rp))
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
		return "", fmt.Errorf("git diff: %w: %s", err, strings.TrimSpace(errb.String()))
	}
	if out.Len() == 0 {
		return "(live matches repo)\n", nil
	}
	return out.String(), nil
}
