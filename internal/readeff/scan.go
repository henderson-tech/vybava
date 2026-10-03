package readeff

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/henderson-tech/vybava/internal/transcripts"
)

// Options says what to scan.
type Options struct {
	ClaudeRoot string    // ~/.claude/projects
	CodexDir   string    // ~/.codex
	Since      time.Time // sessions whose transcript was written since
	Repo       string    // keep sessions of this repository root; "" keeps all
	Session    string    // keep only this session (id or id prefix)
}

// scanWorkers bounds the parallel decode: a scan shares the machine.
var scanWorkers = min(4, max(1, runtime.NumCPU()/2))

type scanJob struct {
	path string
	read func(string) (Session, error)
}

// Scan reads every Claude transcript (subagents included) and Codex rollout
// written since opts.Since and hands each kept session to each. Transcripts
// decode on a few workers; each is called from this goroutine, one session
// at a time, in no particular order, and nothing is held after it returns.
// Warnings name transcripts that could not be read; the rest stands.
func Scan(opts Options, each func(Session)) (warnings []string, err error) {
	jobs, err := scanJobs(opts)
	if err != nil {
		return nil, err
	}
	type result struct {
		path string
		sess Session
		err  error
	}
	work, results := make(chan scanJob), make(chan result, scanWorkers)
	var wg sync.WaitGroup
	for range scanWorkers {
		wg.Go(func() {
			for j := range work {
				s, err := j.read(j.path)
				results <- result{j.path, s, err}
			}
		})
	}
	go func() {
		for _, j := range jobs {
			work <- j
		}
		close(work)
		wg.Wait()
		close(results)
	}()
	for r := range results {
		if r.err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", r.path, r.err))
			continue
		}
		if s, ok := keep(opts, r.path, r.sess); ok {
			if s.Oversize > 0 {
				warnings = append(warnings, fmt.Sprintf("%s: %d record(s) over 16 MiB skipped unread; their tool calls are not counted", r.path, s.Oversize))
			}
			each(s)
		}
	}
	return warnings, nil
}

// scanJobs lists the transcripts in scope. A scoped scan skips other
// repositories' transcripts unread: a Claude project directory names its
// launch directory, and a rollout's first record names its cwd.
func scanJobs(opts Options) ([]scanJob, error) {
	var out []scanJob
	files, _, err := transcripts.WalkClaude(opts.ClaudeRoot)
	if err != nil {
		return out, fmt.Errorf("walk %s: %w", opts.ClaudeRoot, err)
	}
	repoSlug := claudeSlug(opts.Repo)
	for _, f := range files {
		if f.Info.ModTime().Before(opts.Since) {
			continue
		}
		slug, name := projectSlug(opts.ClaudeRoot, f.Path)
		if opts.Repo != "" && slug != repoSlug && !strings.HasPrefix(slug, repoSlug+"-") {
			continue // launched outside the repository (and its worktrees)
		}
		if sid, _, _ := strings.Cut(opts.Session, "/"); sid != "" && !strings.HasPrefix(name, sid) {
			continue // <session>.jsonl, or its <session>/subagents/ tree
		}
		sub := f.Kind != transcripts.ClaudeSession
		out = append(out, scanJob{f.Path, func(p string) (Session, error) {
			s, err := readClaude(p)
			if sub { // a subagent shares its parent's session id
				if s.ID == "" {
					_, s.ID = projectSlug(opts.ClaudeRoot, p) // <session>/subagents/…
				}
				s.ID += "/" + trimExt(filepath.Base(p))
			}
			return s, err
		}})
	}
	rollouts, err := transcripts.RolloutPathsIn(opts.CodexDir, opts.Since)
	if err != nil {
		return out, fmt.Errorf("walk %s: %w", opts.CodexDir, err)
	}
	for _, p := range rollouts {
		if opts.Session != "" && !strings.Contains(filepath.Base(p), "-"+opts.Session) {
			continue // rollout-<time>-<thread id>.jsonl
		}
		if opts.Repo != "" && rolloutRepo(p) != opts.Repo {
			continue
		}
		out = append(out, scanJob{p, readCodex})
	}
	return out, nil
}

// keep names a session and applies the repository and session filters.
func keep(opts Options, path string, s Session) (Session, bool) {
	if s.ID == "" {
		s.ID = trimExt(filepath.Base(path))
	}
	if len(s.Calls) == 0 || opts.Repo != "" && s.Repo != opts.Repo {
		return s, false
	}
	if opts.Session != "" && !strings.HasPrefix(s.ID, opts.Session) && !strings.HasPrefix(trimExt(filepath.Base(path)), opts.Session) {
		return s, false
	}
	return s, true
}

func trimExt(name string) string { return name[:len(name)-len(filepath.Ext(name))] }

// claudeSlug is the projects-directory name Claude Code gives a launch directory.
func claudeSlug(dir string) string { return transcripts.ClaudeSlug(dir) }

// projectSlug is the projects-directory a transcript sits in, and the name
// under it: the session's file or its subagents directory.
func projectSlug(root, path string) (slug, name string) {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return "", ""
	}
	parts := strings.SplitN(filepath.ToSlash(rel), "/", 3)
	if len(parts) < 2 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

// rolloutRepo reads only a rollout's opening session_meta to learn its
// repository, so a scoped scan skips other repositories' rollouts unread.
func rolloutRepo(path string) string {
	repo := ""
	stop := errors.New("stop")
	_, _ = transcripts.Scan(path, transcripts.Cursor{}, false, wholeRead, func(line []byte, _ int64) error {
		var rl transcripts.RolloutLine
		var m transcripts.SessionMeta
		if json.Unmarshal(line, &rl) == nil && rl.Type == transcripts.RolloutSessionMeta && json.Unmarshal(rl.Payload, &m) == nil {
			repo, _ = transcripts.GitRoot(m.CWD)
		}
		return stop // the header is the first record
	})
	return repo
}

// RepoRoot resolves a directory to the repository root sessions are keyed by.
func RepoRoot(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", err
	}
	root, _ := transcripts.GitRoot(abs)
	return root, nil
}
