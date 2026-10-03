package transcripts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// Live Claude Code sessions. ~/.claude/sessions/<pid>.json is Claude Code's
// own record of a running session; its transcript sits under
// ~/.claude/projects/<slug>/<sessionId>.jsonl. claude-guards (idle sessions),
// readeff (slugs) and gitkit pr-census (who holds a PR) read them through here.

var slugUnsafe = regexp.MustCompile(`[^A-Za-z0-9]`)

// ClaudeSlug is the projects-directory name Claude Code gives a launch
// directory: every character but a letter or digit becomes "-".
func ClaudeSlug(dir string) string { return slugUnsafe.ReplaceAllString(dir, "-") }

// ClaudeSessionFile is the slice of ~/.claude/sessions/<pid>.json readers use.
type ClaudeSessionFile struct {
	SessionID       string `json:"sessionId"`
	Cwd             string `json:"cwd"`
	Name            string `json:"name"`
	Status          string `json:"status"`
	StatusUpdatedAt int64  `json:"statusUpdatedAt"` // unix ms
	ProcStart       string `json:"procStart"`       // ps lstart format, written in UTC
}

// ReadClaudeSession reads <claudeDir>/sessions/<pid>.json; ok is false when
// the file is absent, unreadable or names no session.
func ReadClaudeSession(claudeDir string, pid int) (ClaudeSessionFile, bool) {
	var s ClaudeSessionFile
	raw, err := os.ReadFile(filepath.Join(claudeDir, "sessions", fmt.Sprintf("%d.json", pid)))
	if err != nil || json.Unmarshal(raw, &s) != nil || s.SessionID == "" {
		return ClaudeSessionFile{}, false
	}
	return s, true
}

// ProcStartMatches checks a session file's procStart against the process's
// own start, within two minutes. Claude Code writes it in ps lstart's format
// but in UTC (2026-09-25: every file exactly 2 h behind CEST), so either
// reading is accepted; a recycled pid started hours or days apart, far
// outside both windows.
func ProcStartMatches(procStart string, started time.Time) bool {
	for _, loc := range []*time.Location{time.UTC, time.Local} {
		start, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", procStart, loc)
		if err != nil {
			return false
		}
		if d := started.Sub(start); d > -2*time.Minute && d < 2*time.Minute {
			return true
		}
	}
	return false
}

// FindClaudeTranscript finds <projects>/<slug>/<session>.jsonl. A session that
// has since cd'ed into a subdirectory keeps its original slug, so the cwd's
// ancestors are tried, then every project directory.
func FindClaudeTranscript(projects, cwd, sessionID string) (string, bool) {
	name := sessionID + ".jsonl"
	for dir := filepath.Clean(cwd); ; dir = filepath.Dir(dir) {
		path := filepath.Join(projects, ClaudeSlug(dir), name)
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
			return path, true
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	entries, _ := os.ReadDir(projects)
	for _, e := range entries {
		path := filepath.Join(projects, e.Name(), name)
		if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
			return path, true
		}
	}
	return "", false
}
