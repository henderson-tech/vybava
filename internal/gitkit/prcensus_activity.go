package gitkit

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/transcripts"
)

// pr-census's activity: who is working on a PR right now. A PR is active
// when a live Claude Code session, or a Codex rollout written in the last
// codexWindow, made at least minTouches tool calls on it — or runs inside
// its worktree — or when its worktree holds uncommitted edits younger than
// writeWindow. Only tool-call INPUTS count, never their output: a
// `git worktree list` result names every worktree on the machine. The
// session running the census is never a holder, or every PR it inspected
// would read as held.

const (
	claudeWindow = 24 * time.Hour
	codexWindow  = 2 * time.Hour
	writeWindow  = time.Hour
	minTouches   = 3
	leadTail     = 4 << 20 // bytes read from the end of a lead transcript or rollout
	agentTail    = 1 << 20 // bytes read from the end of a subagent transcript
)

// Holder is one session (or uncommitted edit) holding a PR, keys in wire order.
type Holder struct {
	Kind        string `json:"kind"` // claude | codex | writes
	Name        string `json:"name"`
	PID         int    `json:"pid,omitempty"`
	Status      string `json:"status,omitempty"` // Claude's own: busy, idle, waiting, shell
	LastEventAt string `json:"lastEventAt"`
	Evidence    string `json:"evidence"`
}

// Activity is a PR's holders; State is "active" when there is one.
type Activity struct {
	State   string   `json:"state"`
	Holders []Holder `json:"holders"`
}

// touch is one tool call's footprint: when, the directory it ran in, and
// the text naming paths or PRs (a command line, a file path, a patch).
type touch struct {
	at   time.Time
	cwd  string
	text string
}

// agentLog is one session's recent tool calls.
type agentLog struct {
	holder  Holder
	cwd     string // the session's own working directory
	touches []touch
}

// prTarget is what identifies a PR in a tool call.
type prTarget struct {
	number   int
	slug     string // owner/name
	worktree string // "" when its head branch has no local worktree
	repoDirs []string
	home     string
}

func within(path, dir string) bool {
	return dir != "" && (path == dir || strings.HasPrefix(path, dir+"/"))
}

// mentionsDir reports text naming dir as a whole path: the next byte, if
// any, must not continue a file name, so .worktrees/fix never matches
// .worktrees/fix-2.
func mentionsDir(text, dir string) bool {
	for i := 0; dir != ""; {
		j := strings.Index(text[i:], dir)
		if j < 0 {
			return false
		}
		end := i + j + len(dir)
		if end == len(text) || !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-", rune(text[end])) {
			return true
		}
		i = end
	}
	return false
}

// namesWorktree reports a tool call naming the worktree: absolute, relative
// to the call's directory, or ~-relative.
func namesWorktree(tc touch, worktree, home string) bool {
	if mentionsDir(tc.text, worktree) {
		return true
	}
	if home != "" && within(worktree, home) && mentionsDir(tc.text, "~"+strings.TrimPrefix(worktree, home)) {
		return true
	}
	if rel, err := filepath.Rel(tc.cwd, worktree); err == nil && tc.cwd != "" && !strings.HasPrefix(rel, "..") && rel != "." {
		return mentionsDir(tc.text, rel)
	}
	return false
}

var (
	ghPRVerb  = regexp.MustCompile(`\bgh pr (?:checkout|checks|comment|edit|merge|ready|review|close|reopen)\b[^|;&\n]*?\s#?(\d+)\b`)
	otherRepo = regexp.MustCompile(`(?:\s-R\s*|--repo[=\s])([\w.-]+/[\w.-]+)`)
)

// names reports whether one tool call works on the PR: it ran inside the
// worktree, names the worktree, links the PR, or drives it with a gh verb
// from this repository. Reading it (gh pr view, diff, list) does not count.
func (t prTarget) names(tc touch) bool {
	if t.worktree != "" && (within(tc.cwd, t.worktree) || namesWorktree(tc, t.worktree, t.home)) {
		return true
	}
	n := strconv.Itoa(t.number)
	if strings.Contains(tc.text, t.slug+"/pull/"+n) || strings.Contains(tc.text, "repos/"+t.slug+"/pulls/"+n) {
		return true
	}
	inRepo := false
	for _, dir := range t.repoDirs {
		inRepo = inRepo || within(tc.cwd, dir)
	}
	if !inRepo {
		return false
	}
	if m := otherRepo.FindStringSubmatch(tc.text); m != nil && !strings.EqualFold(m[1], t.slug) {
		return false
	}
	for _, m := range ghPRVerb.FindAllStringSubmatch(tc.text, -1) {
		if m[1] == n {
			return true
		}
	}
	return false
}

// holdersOf matches a PR against every session's log.
func holdersOf(t prTarget, logs []agentLog) []Holder {
	holders := []Holder{}
	for _, l := range logs {
		h := l.holder
		if t.worktree != "" && within(l.cwd, t.worktree) {
			h.Evidence = "runs in the worktree"
			holders = append(holders, h)
			continue
		}
		count := 0
		var last time.Time
		for _, tc := range l.touches {
			if t.names(tc) {
				count++
				if tc.at.After(last) {
					last = tc.at
				}
			}
		}
		if count >= minTouches {
			h.LastEventAt = last.UTC().Format(time.RFC3339)
			h.Evidence = strconv.Itoa(count) + " tool calls"
			holders = append(holders, h)
		}
	}
	return holders
}

// readTail returns the complete lines in the last n bytes of a file.
func readTail(path string, n int64) [][]byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	offset := max(info.Size()-n, 0)
	buf := make([]byte, info.Size()-offset)
	if _, err := f.ReadAt(buf, offset); err != nil && err != io.EOF {
		return nil
	}
	if offset > 0 {
		if i := bytes.IndexByte(buf, '\n'); i >= 0 {
			buf = buf[i+1:]
		}
	}
	return bytes.Split(buf, []byte("\n"))
}

// claudeTouches lists the tool calls in a transcript's tail since since.
func claudeTouches(path string, tail int64, since time.Time) []touch {
	var out []touch
	for _, line := range readTail(path, tail) {
		if !transcripts.ClaudeToolLine(line) {
			continue
		}
		rec, err := transcripts.DecodeClaudeTool(line)
		if err != nil || rec.Type != "assistant" || rec.Timestamp.Before(since) {
			continue
		}
		for _, use := range rec.Message.ToolUses() {
			var in struct {
				Command      string `json:"command"`
				FilePath     string `json:"file_path"`
				Path         string `json:"path"`
				NotebookPath string `json:"notebook_path"`
			}
			if json.Unmarshal(use.Input, &in) != nil {
				continue
			}
			text := strings.Join([]string{in.Command, in.FilePath, in.Path, in.NotebookPath}, "\n")
			out = append(out, touch{at: rec.Timestamp, cwd: rec.Cwd, text: text})
		}
	}
	return out
}

// claudeProc is one live claude process from `ps -axo pid=,lstart=,args=`.
type claudeProc struct {
	pid     int
	started time.Time
}

func parseClaudeProcs(ps string) []claudeProc {
	var out []claudeProc
	for line := range strings.SplitSeq(ps, "\n") {
		f := strings.Fields(line)
		if len(f) < 7 || filepath.Base(f[6]) != "claude" {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		started, perr := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(f[1:6], " "), time.Local)
		if err != nil || perr != nil {
			continue
		}
		out = append(out, claudeProc{pid: pid, started: started})
	}
	return out
}

// claudeLogs reads every live Claude session but self: its session file
// (pid reuse refused by procStart), then the tails of its transcript and of
// the subagent transcripts written since since.
func claudeLogs(claudeDir string, procs []claudeProc, self string, since time.Time) []agentLog {
	var logs []agentLog
	for _, p := range procs {
		s, ok := transcripts.ReadClaudeSession(claudeDir, p.pid)
		if !ok || s.SessionID == self || !transcripts.ProcStartMatches(s.ProcStart, p.started) {
			continue
		}
		log := agentLog{cwd: s.Cwd, holder: Holder{Kind: "claude", Name: s.Name, PID: p.pid, Status: s.Status}}
		if s.StatusUpdatedAt > 0 {
			log.holder.LastEventAt = time.UnixMilli(s.StatusUpdatedAt).UTC().Format(time.RFC3339)
		}
		if path, found := transcripts.FindClaudeTranscript(filepath.Join(claudeDir, "projects"), s.Cwd, s.SessionID); found {
			log.touches = claudeTouches(path, leadTail, since)
			_ = filepath.WalkDir(strings.TrimSuffix(path, ".jsonl"), func(sub string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || !strings.HasSuffix(sub, ".jsonl") {
					return nil
				}
				if info, err := d.Info(); err == nil && info.ModTime().After(since) {
					log.touches = append(log.touches, claudeTouches(sub, agentTail, since)...)
				}
				return nil
			})
		}
		logs = append(logs, log)
	}
	return logs
}

// codexLogs reads the rollouts under $CODEX_HOME (else ~/.codex) written
// since since: the session_meta cwd from the first line, the commands and
// patches from the tail.
func codexLogs(home string, since time.Time) []agentLog {
	codexDir := os.Getenv("CODEX_HOME")
	if codexDir == "" {
		codexDir = filepath.Join(home, ".codex")
	}
	paths, _ := transcripts.RolloutPathsIn(codexDir, since)
	var logs []agentLog
	for _, path := range paths {
		log := agentLog{holder: Holder{Kind: "codex", Name: "codex " + rolloutShortID(path)}}
		if first := readHead(path); first != nil {
			var line transcripts.RolloutLine
			var meta transcripts.SessionMeta
			if json.Unmarshal(first, &line) == nil && json.Unmarshal(line.Payload, &meta) == nil {
				log.cwd = meta.CWD
			}
		}
		for _, raw := range readTail(path, leadTail) {
			if !transcripts.RolloutToolLine(raw) {
				continue
			}
			var line transcripts.RolloutLine
			var item transcripts.ResponseItem
			if json.Unmarshal(raw, &line) != nil || line.Type != transcripts.RolloutResponseItem || json.Unmarshal(line.Payload, &item) != nil {
				continue
			}
			at, err := time.Parse(time.RFC3339Nano, line.Timestamp)
			if err != nil || at.Before(since) {
				continue
			}
			cmds, patches := item.Commands()
			for _, c := range cmds {
				cwd := c.Workdir
				if cwd == "" {
					cwd = log.cwd
				}
				log.touches = append(log.touches, touch{at: at, cwd: cwd, text: c.Cmd})
			}
			for _, p := range patches {
				log.touches = append(log.touches, touch{at: at, cwd: log.cwd, text: p})
			}
		}
		if len(log.touches) > 0 {
			sort.Slice(log.touches, func(i, j int) bool { return log.touches[i].at.Before(log.touches[j].at) })
			log.holder.LastEventAt = log.touches[len(log.touches)-1].at.UTC().Format(time.RFC3339)
		}
		logs = append(logs, log)
	}
	return logs
}

// readHead returns a file's first line, nil when unreadable or longer than 1 MiB.
func readHead(path string) []byte {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	n, _ := io.ReadFull(f, buf)
	if i := bytes.IndexByte(buf[:n], '\n'); i >= 0 {
		return buf[:i]
	}
	return nil
}

var rolloutID = regexp.MustCompile(`([0-9a-f]{8})-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.jsonl$`)

func rolloutShortID(path string) string {
	if m := rolloutID.FindStringSubmatch(path); m != nil {
		return m[1]
	}
	return filepath.Base(path)
}
