package claudeguards

// Idle sessions: the candidates the weather's pressure line names. Warn only;
// nothing here ever kills, parks or signals a session.
//
// Process age is not idleness. The sessions that are old on purpose are the
// ones that must never be named: release:monitored's 10 h ScheduleWakeup
// babysit, persistent e2e writers, vitrinka listen loops. A session is idle
// only when every signal agrees:
//
//   - ~/.claude/sessions/<pid>.json (Claude Code's own file) says status
//     "idle" and its statusUpdatedAt is older than idleSessionAge, and its
//     procStart matches the process, so a recycled pid never reads another
//     session's file. Transcript mtime is NOT a signal: Claude Code rewrites
//     idle transcripts in place (on 2026-09-25 one idle for 4 days carried
//     that day's mtime, its last record four days old).
//   - no live child but an MCP server or caffeinate: a Bash run_in_background
//     task or a Monitor is a `zsh -c source …/shell-snapshots/…` child for as
//     long as it runs;
//   - no live teammate (a `claude --parent-session-id <id>` process);
//   - no durable cron in <cwd>/.claude/scheduled_tasks.json;
//   - nothing written under its session directory (subagents, workflows,
//     tool results) since it went idle: an in-process background agent or
//     workflow has no process of its own, and the lead waiting on one reads
//     "idle". As a hold, a spurious write only keeps a session off the list;
//   - no CronCreate in its transcript. Session-only crons live in memory and
//     are visible nowhere else, so the transcript is read incrementally
//     under a byte budget (a cold 55 MB read took 8.5 s on a loaded Mac),
//     with the cursor cached between runs; until it has been read to the
//     end the session is held.
//
// A pending ScheduleWakeup needs no check: its delay is clamped to [60,
// 3600] s and every wakeup runs a turn, which refreshes statusUpdatedAt, so a
// session idle for 10 h has none pending. Whatever cannot be decided is held,
// never named: no session file, a stale one or one without statusUpdatedAt,
// an unread transcript. Codex
// sessions have no status file and are never named.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/transcripts"
)

const idleSessionAge = 10 * time.Hour

// sessionHold says why an otherwise idle session is kept off the list.
type sessionHold string

const (
	holdNone      sessionHold = ""                // idle: a candidate
	holdActive    sessionHold = "active"          // busy, waiting on input, or idle < 10 h
	holdTask      sessionHold = "background task" // a live Bash/Monitor child, or an agent writing since idle
	holdTeammates sessionHold = "teammates"
	holdCron      sessionHold = "cron"
	holdUndecided sessionHold = "undecided" // no or stale session file, transcript not read to the end
)

// claudeSessionFile is the slice of ~/.claude/sessions/<pid>.json read here.
type claudeSessionFile struct {
	SessionID       string `json:"sessionId"`
	Cwd             string `json:"cwd"`
	Status          string `json:"status"`
	StatusUpdatedAt int64  `json:"statusUpdatedAt"` // unix ms
	ProcStart       string `json:"procStart"`       // ps lstart format, written in UTC
}

// sessionFacts is what the classification needs beyond the process table;
// liveSessionFacts reads the machine, tests inject their own.
type sessionFacts struct {
	session       func(pid int) (claudeSessionFile, bool)
	durableCron   func(cwd string) bool
	agentActivity func(s claudeSessionFile) bool
	sessionCron   func(s claudeSessionFile) (cron, decided bool)
}

type idleSession struct {
	pid       int
	sessionID string
	project   string
	idleFor   time.Duration
	rssKB     int // the session plus its descendants: its MCP servers exit with it
}

type idleReport struct {
	Idle []idleSession // largest RSS first
	Held map[sessionHold]int
}

func (r idleReport) rssKB() int {
	total := 0
	for _, s := range r.Idle {
		total += s.rssKB
	}
	return total
}

var (
	// mcpArgs recognizes an MCP server child: mcp-lazy, appium-mcp,
	// @playwright/mcp, `artisan boost:mcp`, `@angular/cli mcp`.
	mcpArgs = regexp.MustCompile(`(^|[\s/:@_-])mcp([\s/:@_.-]|$)`)
	// shellSnapshot marks the Bash tool's shell: a background task or Monitor.
	shellSnapshot = "/.claude/shell-snapshots/"
)

// sessionHelper reports a child every session has that is not a task. A
// shell never is, whatever its command line mentions.
func sessionHelper(p machineProc) bool {
	if strings.Contains(p.args, shellSnapshot) {
		return false
	}
	switch p.base() {
	case "zsh", "bash", "sh", "-zsh", "-bash":
		return false
	case "caffeinate":
		return true
	}
	return mcpArgs.MatchString(p.args)
}

// parentSessionID is a teammate's lead session, "" for a top-level session.
func parentSessionID(args string) string {
	f := strings.Fields(args)
	for i := 0; i+1 < len(f); i++ {
		if f[i] == "--parent-session-id" {
			return f[i+1]
		}
	}
	return ""
}

// procStartMatches checks the session file's procStart against the
// process's own start (now - etime), within two minutes. Claude Code writes
// it in ps lstart's format but in UTC (2026-09-25: every file exactly 2 h
// behind CEST), so either reading is accepted; a recycled pid started hours
// or days apart, far outside both windows.
func procStartMatches(procStart, etime string, now time.Time) bool {
	sec, ok := etimeSeconds(etime)
	if !ok {
		return false
	}
	actual := now.Add(-time.Duration(sec) * time.Second)
	for _, loc := range []*time.Location{time.UTC, time.Local} {
		start, err := time.ParseInLocation("Mon Jan _2 15:04:05 2006", procStart, loc)
		if err != nil {
			return false
		}
		if d := actual.Sub(start); d > -2*time.Minute && d < 2*time.Minute {
			return true
		}
	}
	return false
}

// idleSessions classifies every top-level claude process in the table.
func idleSessions(table []machineProc, now time.Time, facts sessionFacts) idleReport {
	children := make(map[int][]machineProc, len(table))
	teammates := map[string]int{}
	for _, p := range table {
		children[p.ppid] = append(children[p.ppid], p)
		if p.base() == kindClaude {
			if lead := parentSessionID(p.args); lead != "" {
				teammates[lead]++
			}
		}
	}
	report := idleReport{Held: map[sessionHold]int{}}
	for _, p := range table {
		if p.base() != kindClaude || parentSessionID(p.args) != "" {
			continue // a teammate belongs to its lead
		}
		hold, s, idleFor := classifySession(p, children[p.pid], teammates, now, facts)
		switch hold {
		case holdActive:
		case holdNone:
			report.Idle = append(report.Idle, idleSession{pid: p.pid, sessionID: s.SessionID, project: projectLabel(s.Cwd),
				idleFor: idleFor, rssKB: subtreeRSS(p, children)})
		default:
			report.Held[hold]++
		}
	}
	sort.Slice(report.Idle, func(i, j int) bool { return report.Idle[i].rssKB > report.Idle[j].rssKB })
	return report
}

// classifySession applies the rules in the order of their cost; the
// transcript read runs only for a session every cheaper rule let through.
func classifySession(p machineProc, kids []machineProc, teammates map[string]int, now time.Time, facts sessionFacts) (sessionHold, claudeSessionFile, time.Duration) {
	s, ok := facts.session(p.pid)
	if !ok || !procStartMatches(s.ProcStart, p.etime, now) || s.StatusUpdatedAt <= 0 {
		return holdUndecided, s, 0 // no timestamp would read as idle since 1970
	}
	idleFor := now.Sub(time.UnixMilli(s.StatusUpdatedAt))
	if s.Status != "idle" || idleFor < idleSessionAge {
		return holdActive, s, idleFor
	}
	for _, k := range kids {
		if !sessionHelper(k) {
			return holdTask, s, idleFor
		}
	}
	if teammates[s.SessionID] > 0 {
		return holdTeammates, s, idleFor
	}
	if facts.durableCron(s.Cwd) {
		return holdCron, s, idleFor
	}
	if facts.agentActivity(s) {
		return holdTask, s, idleFor
	}
	cron, decided := facts.sessionCron(s)
	switch {
	case cron:
		return holdCron, s, idleFor
	case !decided:
		return holdUndecided, s, idleFor
	}
	return holdNone, s, idleFor
}

func subtreeRSS(root machineProc, children map[int][]machineProc) int {
	total := root.rssKB
	seen := map[int]bool{root.pid: true}
	stack := []int{root.pid}
	for len(stack) > 0 {
		pid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, c := range children[pid] {
			if !seen[c.pid] {
				seen[c.pid] = true
				total += c.rssKB
				stack = append(stack, c.pid)
			}
		}
	}
	return total
}

// projectLabel names a session's project: the repo, plus the worktree slug
// for a checkout under .worktrees/.
func projectLabel(cwd string) string {
	const marker = "/.worktrees/"
	if i := strings.Index(cwd, marker); i >= 0 {
		slug, _, _ := strings.Cut(cwd[i+len(marker):], "/")
		return filepath.Base(cwd[:i]) + "/" + slug
	}
	return filepath.Base(cwd)
}

// idleClause is the pressure line's tail: how much the idle sessions hold
// and the three largest.
func (r idleReport) idleClause() string {
	if len(r.Idle) == 0 {
		return ""
	}
	var top []string
	for _, s := range r.Idle[:min(3, len(r.Idle))] {
		top = append(top, fmt.Sprintf("%s %s idle %s", s.project, fmtKB(s.rssKB), fmtIdle(s.idleFor)))
	}
	return fmt.Sprintf(", and %d Claude sessions idle for 10 h+ with no background task, teammate or cron hold %s; close by hand if done: %s (claude-guards weather --text lists them)",
		len(r.Idle), fmtKB(r.rssKB()), strings.Join(top, ", "))
}

func fmtKB(kb int) string {
	if kb >= 1<<20 {
		return fmt.Sprintf("%.1f GB", float64(kb)/(1<<20))
	}
	return fmt.Sprintf("%d MB", kb>>10)
}

func fmtIdle(d time.Duration) string {
	if d >= 48*time.Hour {
		return fmt.Sprintf("%dd", int(d.Hours())/24)
	}
	return fmt.Sprintf("%dh", int(d.Hours()))
}

// liveSessionFacts reads Claude Code's state under home. budget bounds the
// transcript bytes this run reads; flush persists the cron-scan cursors.
func liveSessionFacts(home string, budget int64, cachePath string) (sessionFacts, func()) {
	claude := filepath.Join(home, ".claude")
	cache := loadCronCache(cachePath)
	type found struct {
		path string
		ok   bool
	}
	transcriptOf := map[string]found{}
	transcript := func(s claudeSessionFile) (string, bool) {
		f, known := transcriptOf[s.SessionID]
		if !known {
			f.path, f.ok = findTranscript(filepath.Join(claude, "projects"), s.Cwd, s.SessionID)
			transcriptOf[s.SessionID] = f
		}
		return f.path, f.ok
	}
	facts := sessionFacts{
		session: func(pid int) (claudeSessionFile, bool) {
			var s claudeSessionFile
			raw, err := os.ReadFile(filepath.Join(claude, "sessions", fmt.Sprintf("%d.json", pid)))
			if err != nil || json.Unmarshal(raw, &s) != nil || s.SessionID == "" {
				return claudeSessionFile{}, false
			}
			return s, true
		},
		durableCron: func(cwd string) bool {
			dirs := []string{cwd}
			if root, _ := transcripts.GitRoot(cwd); root != "" && root != cwd {
				dirs = append(dirs, root)
			}
			for _, dir := range dirs {
				if hasScheduledTasks(filepath.Join(dir, ".claude", "scheduled_tasks.json")) {
					return true
				}
			}
			return false
		},
		agentActivity: func(s claudeSessionFile) bool {
			path, found := transcript(s)
			if !found {
				return false // never prompted: nothing was ever spawned
			}
			// A minute of slack: the turn that went idle writes its own last
			// tool results in the same moment.
			return writtenAfter(strings.TrimSuffix(path, ".jsonl"), time.UnixMilli(s.StatusUpdatedAt).Add(time.Minute))
		},
		sessionCron: func(s claudeSessionFile) (bool, bool) {
			path, found := transcript(s)
			if !found {
				return false, true // never prompted: no transcript, no cron
			}
			return cache.scan(s.SessionID, path, &budget)
		},
	}
	return facts, func() { cache.save(cachePath) }
}

// writtenAfter reports anything under a session's directory (<session>/
// subagents, workflows, tool-results) written after since. An in-process
// background agent or workflow has no process of its own and leaves the lead
// "idle"; its transcript there is the only trace that it still runs. What
// cannot be read counts as written: the safe side.
func writtenAfter(dir string, since time.Time) bool {
	written := false
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		var info fs.FileInfo
		if err == nil {
			info, err = entry.Info()
		}
		if errors.Is(err, fs.ErrNotExist) && path != dir {
			return nil // removed since the listing
		}
		if err != nil {
			return err
		}
		if info.ModTime().After(since) {
			written = true
			return filepath.SkipAll
		}
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return written // no directory: the session never spawned an agent
	}
	return written || err != nil
}

// hasScheduledTasks reads a durable cron file. Unreadable or unparseable
// counts as holding tasks: the safe side.
func hasScheduledTasks(path string) bool {
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	// A list of tasks, or an object whose "tasks" is one; any other object
	// with keys counts as holding them.
	var list []json.RawMessage
	if json.Unmarshal(raw, &list) == nil {
		return len(list) > 0
	}
	var doc map[string]json.RawMessage
	if json.Unmarshal(raw, &doc) != nil {
		return len(bytes.TrimSpace(raw)) > 0
	}
	if json.Unmarshal(doc["tasks"], &list) == nil {
		return len(list) > 0
	}
	return len(doc) > 0
}

var slugUnsafe = regexp.MustCompile(`[^A-Za-z0-9]`)

// findTranscript finds <projects>/<slug>/<session>.jsonl. The slug is the
// project directory with every other character a dash; a session that has
// since cd'ed into a subdirectory keeps its original slug, so the cwd's
// ancestors are tried, then every project directory.
func findTranscript(projects, cwd, sessionID string) (string, bool) {
	name := sessionID + ".jsonl"
	for dir := filepath.Clean(cwd); ; dir = filepath.Dir(dir) {
		path := filepath.Join(projects, slugUnsafe.ReplaceAllString(dir, "-"), name)
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

// cronCreateTag is the prefilter: a tool_use block's name, unescaped, which a
// tool result or a quoted command can only ever carry escaped.
var cronCreateTag = []byte(`"name":"CronCreate"`)

// cronCreateRecord reports an assistant record that calls CronCreate.
func cronCreateRecord(line []byte) bool {
	if !bytes.Contains(line, cronCreateTag) {
		return false
	}
	rec, err := transcripts.DecodeClaude(line)
	if err != nil {
		return true // matched the tag and cannot be read: the safe side
	}
	var blocks []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if rec.Type != "assistant" || json.Unmarshal(rec.Message.Content, &blocks) != nil {
		return false
	}
	for _, b := range blocks {
		if b.Type == "tool_use" && b.Name == "CronCreate" {
			return true
		}
	}
	return false
}

// cronScan is one transcript's persisted scan state.
type cronScan struct {
	Path   string             `json:"path"`
	Cursor transcripts.Cursor `json:"cursor"`
	Cron   bool               `json:"cron"`
}

type cronCache struct {
	entries map[string]cronScan
	used    map[string]bool
	dirty   bool
}

func loadCronCache(path string) *cronCache {
	c := &cronCache{entries: map[string]cronScan{}, used: map[string]bool{}}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &c.entries) // a corrupt cache only costs a rescan
	}
	return c
}

// scan reads the transcript from its cached cursor while the run's budget
// lasts. decided is true once the file has been read to its end, or as soon
// as a CronCreate is seen.
func (c *cronCache) scan(sessionID, path string, budget *int64) (cron, decided bool) {
	c.used[sessionID] = true
	e, known := c.entries[sessionID]
	if known && e.Path != path {
		e, known = cronScan{}, false
	}
	if e.Cron {
		return true, true
	}
	info, err := os.Lstat(path)
	if err != nil {
		return false, false
	}
	if known && e.Cursor.Unchanged(info) {
		return false, true
	}
	if *budget <= 0 {
		return false, false
	}
	saw := false
	res, err := transcripts.Scan(path, e.Cursor, known, transcripts.ScanOptions{Budget: *budget, SkipOversize: true}, func(line []byte, _ int64) error {
		saw = saw || cronCreateRecord(line)
		return nil
	})
	if err != nil || res.Skipped {
		return false, false // vanished since the stat: nothing to decide on
	}
	*budget -= res.Read
	// A reset re-read from byte 0, so saw covers the whole file either way.
	e.Path, e.Cursor, e.Cron = path, res.Cursor, saw
	c.entries[sessionID], c.dirty = e, true
	return e.Cron, e.Cron || res.Pending == 0
}

// save writes the cache atomically, keeping entries this run used or read
// within a week.
func (c *cronCache) save(path string) {
	if !c.dirty || path == "" {
		return
	}
	cutoff := time.Now().Add(-7 * 24 * time.Hour)
	for id, e := range c.entries {
		if !c.used[id] && e.Cursor.VerifiedAt.Before(cutoff) {
			delete(c.entries, id)
		}
	}
	raw, err := json.Marshal(c.entries)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}

// cronCachePath is where the scan cursors live between runs.
func cronCachePath() string {
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "vybava", "claude-guards", "session-cron-scan.json")
}
