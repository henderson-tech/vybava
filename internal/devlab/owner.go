package devlab

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Owner attributes a lease: who holds it, from which checkout, under which
// session. It is attribution and liveness, never the capability (the token
// is).
type Owner struct {
	Project  string `json:"project,omitempty"`
	RepoRoot string `json:"repoRoot,omitempty"`
	Worktree string `json:"worktree,omitempty"`
	Branch   string `json:"branch,omitempty"`
	// SessionID is CLAUDE_CODE_SESSION_ID: the SessionEnd hook releases
	// every lease that names the ending session.
	SessionID string `json:"sessionId,omitempty"`
	// ClaudePID and its start time judge liveness: a pid that is gone, or
	// that now belongs to a process started at another time, is dead.
	ClaudePID      int        `json:"claudePid,omitempty"`
	ClaudePIDStart *time.Time `json:"claudePidStart,omitempty"`
	InvokerPID     int        `json:"invokerPid,omitempty"`
	Host           string     `json:"host,omitempty"`
}

// String is the one-line holder description diagnostics print.
func (o *Owner) String() string {
	if o == nil {
		return "an unknown holder"
	}
	var parts []string
	switch {
	case o.Worktree != "" && o.Branch != "":
		parts = append(parts, fmt.Sprintf("worktree %s (%s)", o.Worktree, o.Branch))
	case o.Worktree != "":
		parts = append(parts, "worktree "+o.Worktree)
	case o.Project != "":
		parts = append(parts, o.Project)
	}
	if o.SessionID != "" {
		parts = append(parts, "session "+shortID(o.SessionID))
	}
	if o.ClaudePID != 0 {
		parts = append(parts, "claude pid "+strconv.Itoa(o.ClaudePID))
	} else if o.InvokerPID != 0 {
		parts = append(parts, "pid "+strconv.Itoa(o.InvokerPID))
	}
	if len(parts) == 0 {
		return "an unknown holder"
	}
	return strings.Join(parts, ", ")
}

func shortID(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// currentOwner reads the invoking checkout and session. A field it cannot
// read stays empty: attribution never blocks an acquire.
func (l *Lab) currentOwner(ctx context.Context) *Owner {
	o := &Owner{InvokerPID: l.Pid, SessionID: l.Getenv("CLAUDE_CODE_SESSION_ID")}
	o.Host, _ = os.Hostname()
	if pid, err := strconv.Atoi(strings.TrimSpace(l.Getenv("CLAUDE_PID"))); err == nil && pid > 0 {
		o.ClaudePID = pid
		if start, ok, err := l.ProcStart(pid); err == nil && ok {
			o.ClaudePIDStart = &start
		}
	}
	dir := l.ProjectDir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	git := func(args ...string) string {
		out, err := l.run(ctx, 5*time.Second, append([]string{"git", "-C", dir}, args...)...)
		if err != nil || out.Code != 0 {
			return ""
		}
		return strings.TrimSpace(out.Stdout)
	}
	if root := git("rev-parse", "--show-toplevel"); root != "" {
		o.RepoRoot = root
		o.Worktree = filepath.Base(root)
	}
	o.Branch = git("rev-parse", "--abbrev-ref", "HEAD")
	o.Project = projectFromRemote(git("config", "--get", "remote.origin.url"))
	return o
}

// projectFromRemote turns a git remote into "owner/repo".
func projectFromRemote(url string) string {
	url = strings.TrimSuffix(strings.TrimSpace(url), ".git")
	if url == "" {
		return ""
	}
	if i := strings.Index(url, "://"); i >= 0 {
		url = url[i+3:]
		if at := strings.LastIndex(url, "@"); at >= 0 {
			url = url[at+1:]
		}
		if slash := strings.Index(url, "/"); slash >= 0 {
			return url[slash+1:]
		}
		return url
	}
	if _, path, ok := strings.Cut(url, ":"); ok {
		return path
	}
	return url
}

// Liveness is the holder process's state.
type Liveness string

const (
	Alive   Liveness = "alive"
	Dead    Liveness = "dead"
	Unknown Liveness = "unknown"
)

// startSlack absorbs ps's one-second lstart resolution.
const startSlack = 2 * time.Second

// liveness judges a holder: no recorded claude pid, or a process table it
// cannot read, is Unknown (treated as alive until the TTL ends).
func (l *Lab) liveness(o *Owner) Liveness {
	if o == nil || o.ClaudePID <= 0 {
		return Unknown
	}
	start, ok, err := l.ProcStart(o.ClaudePID)
	if err != nil {
		return Unknown
	}
	if !ok {
		return Dead
	}
	if o.ClaudePIDStart != nil {
		if d := start.Sub(*o.ClaudePIDStart); d > startSlack || d < -startSlack {
			return Dead // the pid was recycled
		}
	}
	return Alive
}
