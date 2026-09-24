package memo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GitignoreFile is the per-home ignore list a TEAM home carries (decision
// 2026-09-21): only LEDGER.md and notes/ are shared through the repository;
// MEMORY.md and usage.jsonl are per-machine projections rendered on demand.
// A personal home never gets one - everything there is snapshotted as before.
const GitignoreFile = ".gitignore"

// LocalFiles are the team-home files that stay out of git.
var LocalFiles = []string{IndexFile, UsageFile}

// EnsureGitignore makes sure a team home inside a git work tree lists the
// local files in <home>/.gitignore: the file is created when missing, the
// two lines are added when absent, every other line is kept. Idempotent;
// reports whether the file changed. A personal home, or a team home outside
// any work tree, is left alone. A local file git still tracks gets no line:
// git ignores nothing it tracks, so the line would only dirty the checkout
// (TrackedIndex names the untracking instead).
func EnsureGitignore(home string, kind Kind) (bool, error) {
	if kind != KindTeam {
		return false, nil
	}
	path := filepath.Join(home, GitignoreFile)
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(data) == 0 {
		lines = nil
	}
	have := map[string]bool{}
	for _, line := range lines {
		have[strings.TrimSpace(line)] = true
	}
	var missing []string
	for _, f := range LocalFiles {
		if have[f] || have["/"+f] {
			continue
		}
		if tracked, _ := GitTracked(filepath.Join(home, f)); tracked {
			continue
		}
		missing = append(missing, f)
	}
	if len(missing) == 0 {
		return false, nil
	}
	if _, ok := gitToplevel(home); !ok {
		return false, nil
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return false, err
	}
	lines = append(lines, missing...)
	return true, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// EnsureResult is what EnsureIndex decided for one home.
type EnsureResult struct {
	Home     string `json:"home"`
	Reason   string `json:"reason"` // missing | stale | current
	Rendered bool   `json:"rendered"`
	Tracked  *Diag  `json:"tracked,omitempty"` // SURFACE_TRACKED: left as committed
}

// EnsureIndex renders MEMORY.md when it is missing or older than LEDGER.md
// or usage.jsonl, and does nothing when it is current. A stale file whose
// render is unchanged gets its mtime bumped so the next call is the fast
// path; a tracked one is not touched at all. Meant for SessionStart: cheap
// when nothing moved.
func EnsureIndex(l *Ledger, events []Event, now time.Time) (EnsureResult, error) {
	home := l.Home()
	res := EnsureResult{Home: home, Reason: "current"}
	index := filepath.Join(home, IndexFile)
	info, err := os.Stat(index)
	switch {
	case os.IsNotExist(err):
		res.Reason = "missing"
	case err != nil:
		return res, err
	default:
		for _, f := range []string{LedgerFile, UsageFile} {
			if src, err := os.Stat(filepath.Join(home, f)); err == nil && src.ModTime().After(info.ModTime()) {
				res.Reason = "stale"
				break
			}
		}
	}
	if res.Reason == "current" {
		return res, nil
	}
	changed, tracked, err := WriteIndex(l, events, now)
	if err != nil {
		return res, err
	}
	res.Rendered, res.Tracked = changed, tracked
	if !changed && tracked == nil {
		if err := os.Chtimes(index, now, now); err != nil {
			return res, err
		}
	}
	return res, nil
}

// EnsureHome opens a home and runs EnsureIndex on it.
func (e Env) EnsureHome(h Home, now time.Time) (EnsureResult, *Diag, error) {
	l, d, err := e.Open(h, false)
	if d != nil || err != nil {
		return EnsureResult{Home: h.Path}, d, err
	}
	events, d, err := LoadEvents(h.Path)
	if d != nil || err != nil {
		return EnsureResult{Home: h.Path}, d, err
	}
	res, err := EnsureIndex(l, events, now)
	return res, nil, err
}

// GitIgnored asks git whether a path is ignored. known is false outside a
// work tree (or when git is unavailable), where the answer means nothing. A
// tracked file is never ignored: `git check-ignore` reports it as not
// ignored even when a pattern matches, which is exactly the "committed"
// case memorylint warns about.
func GitIgnored(path string) (ignored, known bool) {
	dir := filepath.Dir(path)
	if _, ok := gitToplevel(dir); !ok {
		return false, false
	}
	cmd := exec.Command("git", "-C", dir, "check-ignore", "-q", "--", path)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	err := cmd.Run()
	if err == nil {
		return true, true
	}
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
		return false, true
	}
	return false, false
}

// GitTracked asks git whether a path is in its work tree's index. known is
// false outside a work tree (or when git is unavailable).
func GitTracked(path string) (tracked, known bool) {
	dir := filepath.Dir(path)
	if !isDir(dir) {
		return false, false
	}
	cmd := exec.Command("git", "-C", dir, "ls-files", "--error-unmatch", "--", filepath.Base(path))
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	err := cmd.Run()
	if err == nil {
		return true, true
	}
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
		return false, true
	}
	return false, false
}

// TrackedIndex returns the SURFACE_TRACKED warning when git tracks a team
// home's MEMORY.md, else nil. Such a file is never written by memo: a tracked
// render (a home committed before 2026-09-21) is untracked with `git rm
// --cached`; a tracked hand-written v2 index is folded into rows first, so
// the fix never drops an index nobody has migrated.
func TrackedIndex(home string, kind Kind) *Diag {
	if kind != KindTeam {
		return nil
	}
	if tracked, _ := GitTracked(filepath.Join(home, IndexFile)); !tracked {
		return nil
	}
	path := filepath.Join(home, IndexFile)
	untrack := "git -C " + home + " rm --cached -q -- " + IndexFile + " && memo render --home " + home
	committed, err := exec.Command("git", "-C", home, "show", ":./"+IndexFile).Output()
	if err != nil || !isRenderedIndex(string(committed)) {
		return &Diag{Code: DiagSurfaceTracked, Severity: "warning", Detail: path + " is a hand-written index tracked by git, so memo left it as committed; in a worktree fold its lines into rows (`memo migrate` prints the template, `memo import` appends it), then untrack it: " + untrack, Fix: "memo migrate " + home}
	}
	return &Diag{Code: DiagSurfaceTracked, Severity: "warning", Detail: path + " is tracked by git, so memo left it as committed: the team hot surface is a local render and rewriting a tracked copy dirties the checkout; untrack it in a worktree and commit the removal with " + GitignoreFile, Fix: untrack}
}

// LegacyHome returns the LEGACY_HOME refusal for a home holding a
// hand-written v2 MEMORY.md and no LEDGER.md, else nil. A first `memo add`
// there would render over the index (it happened to devulinka-infra and
// ReservineBack, and nothing snapshots a personal home before its ledger
// exists); `memo import` is the conversion and may replace it.
func LegacyHome(home string) *Diag {
	if hasLedger(home) {
		return nil
	}
	have, err := os.ReadFile(filepath.Join(home, IndexFile))
	if err != nil || strings.TrimSpace(string(have)) == "" || isRenderedIndex(string(have)) {
		return nil
	}
	return errorDiag(DiagLegacyHome, home+" is a v2 home (a hand-written MEMORY.md, no LEDGER.md), so the row was not written: a first add would render over the index; convert it: `memo migrate "+home+"` prints the template, `memo import <file> --home "+home+"` creates the ledger and replaces the index, then re-run this add", "memo migrate "+home)
}

// isRenderedIndex tells a memo render from a hand-written v2 index.
func isRenderedIndex(s string) bool {
	return strings.HasPrefix(s, indexHeading+"\n") && strings.Contains(s, "\n"+citeLead)
}
