package memo

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// snapshotRepo is where a personal home's history lives: the enclosing git
// work tree when there is one (`~/.claude` is itself a repo, so the home is
// already tracked there and every git call is scoped to the home's own
// paths), else the home itself, initialised on first use and never given a
// remote.
type snapshotRepo struct {
	Root string // git toplevel
	Home string // the home, absolute
	Rel  string // home relative to Root ("." when the home is the repo)
}

func repoFor(h Home, create bool) (snapshotRepo, error) {
	home, err := filepath.Abs(h.Path)
	if err != nil {
		return snapshotRepo{}, err
	}
	if home, err = filepath.EvalSymlinks(home); err != nil {
		return snapshotRepo{}, err
	}
	out, err := exec.Command("git", "-C", home, "rev-parse", "--show-toplevel").Output()
	if err == nil {
		root := strings.TrimSpace(string(out))
		rel, err := filepath.Rel(root, home)
		if err != nil {
			return snapshotRepo{}, err
		}
		return snapshotRepo{Root: root, Home: home, Rel: rel}, nil
	}
	if !create {
		return snapshotRepo{}, os.ErrNotExist
	}
	if out, err := git(home, "init", "-q"); err != nil {
		return snapshotRepo{}, fmt.Errorf("git init: %s", out)
	}
	return snapshotRepo{Root: home, Home: home, Rel: "."}, nil
}

// Snapshot commits the personal home's files. Inside an existing work tree
// only the home's own paths are staged and committed (`git add -- <home>`,
// `git commit -- <home>`), so unrelated dirty files stay untouched. Returns
// the commit hash, or "" with a SNAPSHOT_CLEAN info diagnostic.
func Snapshot(h Home, message string) (string, *Diag, error) {
	if d := teamOwned(h); d != nil {
		return "", d, nil
	}
	r, err := repoFor(h, true)
	if err != nil {
		return "", nil, err
	}
	if out, err := git(r.Root, "add", "--", r.Rel); err != nil {
		return "", nil, fmt.Errorf("git add: %s", out)
	}
	if out, _ := git(r.Root, "status", "--porcelain", "--", r.Rel); strings.TrimSpace(out) == "" {
		return "", &Diag{Code: DiagSnapshotClean, Severity: "info", Detail: "nothing changed since the last snapshot"}, nil
	}
	if message == "" {
		message = "memo: snapshot " + h.Alias
	}
	if out, err := git(r.Root, "-c", "user.name=memo", "-c", "user.email=memo@vybava.local", "commit", "-q", "-m", message, "--", r.Rel); err != nil {
		return "", nil, fmt.Errorf("git commit: %s", out)
	}
	hash, err := git(r.Root, "rev-parse", "--short", "HEAD")
	return strings.TrimSpace(hash), nil, err
}

// LogEntry is one snapshot in `memo log`.
type LogEntry struct {
	Rev     string `json:"rev"`
	At      string `json:"at"`
	Message string `json:"message"`
}

// Log lists the newest n commits touching the home.
func Log(h Home, n int) ([]LogEntry, *Diag, error) {
	if d := teamOwned(h); d != nil {
		return nil, d, nil
	}
	r, err := repoFor(h, false)
	if os.IsNotExist(err) {
		return []LogEntry{}, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	out, err := git(r.Root, "log", fmt.Sprintf("-n%d", n), "--format=%h%x1f%cI%x1f%s", "--", r.Rel)
	if err != nil {
		if strings.Contains(out, "does not have any commits") {
			return []LogEntry{}, nil, nil
		}
		return nil, nil, fmt.Errorf("git log: %s", out)
	}
	entries := []LogEntry{}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.SplitN(line, "\x1f", 3)
		if len(parts) == 3 {
			entries = append(entries, LogEntry{Rev: parts[0], At: parts[1], Message: parts[2]})
		}
	}
	return entries, nil, nil
}

// Restore brings one file of the home back from a revision.
func Restore(h Home, rev, file string) (*Diag, error) {
	if d := teamOwned(h); d != nil {
		return d, nil
	}
	if strings.Contains(file, "..") || filepath.IsAbs(file) {
		return errorDiag(DiagUsage, "restore takes a file relative to the home", "memo restore "+rev+" "+LedgerFile), nil
	}
	r, err := repoFor(h, false)
	if os.IsNotExist(err) {
		return errorDiag(DiagUsage, h.Path+" has no snapshots yet", "memo snapshot --home "+h.Path), nil
	}
	if err != nil {
		return nil, err
	}
	if out, err := git(r.Root, "checkout", rev, "--", filepath.Join(r.Rel, file)); err != nil {
		return errorDiag(DiagUsage, strings.TrimSpace(out), "memo log --json  # pick a rev"), nil
	}
	return nil, nil
}

func teamOwned(h Home) *Diag {
	if h.Kind != KindTeam {
		return nil
	}
	return &Diag{Code: DiagSnapshotTeamOwned, Severity: "info", Detail: h.Path + " is a team home; the repository's git owns its history", Fix: "git -C " + RepoRoot(h.Path) + " log -- .claude/memory"}
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	return string(out), err
}
