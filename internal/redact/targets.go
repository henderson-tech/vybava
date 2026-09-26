package redact

import (
	"encoding/json"
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

// Roots are where each agent keeps history. Tests point them at fixtures.
type Roots struct {
	// ClaudeProjects holds <slug>/<session>.jsonl and <slug>/<session>/…
	// (subagents, workflow journals and state, persisted tool results).
	ClaudeProjects string
	// ClaudeTmp holds <slug>/<session>/tasks/*.output — background-task
	// output as plain files, and links to subagent transcripts.
	ClaudeTmp string
	// ClaudeHistory is the prompt history (every typed prompt, pastes too).
	ClaudeHistory string
	// Codex is $CODEX_HOME: rollouts under transcripts.RolloutRoots and
	// history.jsonl.
	Codex string
}

// DefaultRoots resolves the live locations for the current user.
func DefaultRoots() (Roots, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Roots{}, fmt.Errorf("resolve home: %w", err)
	}
	claude := os.Getenv("CLAUDE_CONFIG_DIR")
	if claude == "" {
		claude = filepath.Join(home, ".claude")
	}
	codex := os.Getenv("CODEX_HOME")
	if codex == "" {
		codex = filepath.Join(home, ".codex")
	}
	return Roots{
		ClaudeProjects: filepath.Join(claude, "projects"),
		ClaudeTmp:      filepath.Join("/tmp", fmt.Sprintf("claude-%d", os.Getuid())),
		ClaudeHistory:  filepath.Join(claude, "history.jsonl"),
		Codex:          codex,
	}, nil
}

// Target names what to scan; the union of every field is scanned.
type Target struct {
	// Sessions are Claude session ids (with subagents, workflows, tool
	// results and task output) or Codex thread ids (their rollout).
	Sessions []string
	// Projects are directories: every Claude session recorded for it or one
	// of its worktrees, every Codex rollout whose cwd is inside it.
	Projects []string
	// Claude and Codex take an agent's whole history, prompt history included.
	Claude, Codex bool
	// Paths are files or directories taken as given.
	Paths []string
	// Since drops files not modified since then; zero keeps everything.
	Since time.Time
}

// Empty reports a target that names nothing.
func (t Target) Empty() bool {
	return len(t.Sessions) == 0 && len(t.Projects) == 0 && !t.Claude && !t.Codex && len(t.Paths) == 0
}

var reSessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{6,}$`)

// Files lists every file the target names, sorted and unique, and counts the
// entries it could not read. A named session or project that matches nothing
// is an error: a typo must not read as "clean".
func (r Roots) Files(t Target) (files []string, unreadable int, err error) {
	set := map[string]bool{}
	add := func(p string) {
		if info, err := os.Stat(p); err == nil && !info.IsDir() && !info.ModTime().Before(t.Since) {
			set[p] = true
		}
	}
	walk := func(dir string) {
		_ = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				unreadable++ // the rest of the walk still counts
				return nil
			}
			if !e.IsDir() {
				add(p)
			}
			return nil
		})
	}
	for _, id := range t.Sessions {
		if !reSessionID.MatchString(id) {
			return nil, 0, fmt.Errorf("not a session id: %q", id)
		}
		found := false
		for _, root := range []string{r.ClaudeProjects, r.ClaudeTmp} {
			dirs, _ := filepath.Glob(filepath.Join(root, "*", id)) // the pattern is fixed: no ErrBadPattern
			logs, _ := filepath.Glob(filepath.Join(root, "*", id+".jsonl"))
			for _, d := range dirs {
				walk(d)
			}
			for _, l := range logs {
				add(l)
			}
			found = found || len(dirs)+len(logs) > 0
		}
		rollouts, err := transcripts.RolloutPathsIn(r.Codex, time.Time{})
		if err != nil {
			return nil, 0, err
		}
		for _, p := range rollouts {
			if strings.Contains(filepath.Base(p), id) {
				add(p)
				found = true
			}
		}
		if !found {
			return nil, 0, fmt.Errorf("no Claude or Codex session %s", id)
		}
	}
	for _, dir := range t.Projects {
		abs, err := filepath.Abs(dir)
		if err != nil {
			return nil, 0, err
		}
		slug := ProjectSlug(abs)
		found := false
		for _, root := range []string{r.ClaudeProjects, r.ClaudeTmp} {
			entries, _ := os.ReadDir(root)
			for _, e := range entries {
				// A worktree's slug extends its repo's: <slug>--worktrees-x.
				if e.IsDir() && (e.Name() == slug || strings.HasPrefix(e.Name(), slug+"--")) {
					walk(filepath.Join(root, e.Name()))
					found = true
				}
			}
		}
		rollouts, err := transcripts.RolloutPathsIn(r.Codex, t.Since)
		if err != nil {
			return nil, 0, err
		}
		for _, p := range rollouts {
			if cwd := rolloutCWD(p); cwd == abs || strings.HasPrefix(cwd, abs+string(filepath.Separator)) {
				add(p)
				found = true
			}
		}
		if !found {
			return nil, 0, fmt.Errorf("no Claude or Codex history for %s", abs)
		}
	}
	if t.Claude {
		walk(r.ClaudeProjects)
		walk(r.ClaudeTmp)
		add(r.ClaudeHistory)
	}
	if t.Codex {
		rollouts, err := transcripts.RolloutPathsIn(r.Codex, t.Since)
		if err != nil {
			return nil, 0, err
		}
		for _, p := range rollouts {
			add(p)
		}
		add(filepath.Join(r.Codex, "history.jsonl"))
	}
	for _, p := range t.Paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, 0, err
		}
		if info.IsDir() {
			walk(p)
		} else {
			add(p)
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, unreadable, nil
}

var reSlugUnsafe = regexp.MustCompile(`[^A-Za-z0-9-]`)

// ProjectSlug is Claude Code's directory name for a project path: every
// character outside [A-Za-z0-9-] becomes '-'.
func ProjectSlug(abs string) string { return reSlugUnsafe.ReplaceAllString(abs, "-") }

// rolloutCWD reads the cwd from a rollout's first record (its session_meta);
// "" when it has none. The decoder stops after that one record, however
// large its base instructions make it.
func rolloutCWD(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	var line transcripts.RolloutLine
	var meta transcripts.SessionMeta
	if json.NewDecoder(f).Decode(&line) != nil || line.Type != "session_meta" || json.Unmarshal(line.Payload, &meta) != nil || meta.CWD == "" {
		return ""
	}
	return filepath.Clean(meta.CWD)
}
