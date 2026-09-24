package mergeassist

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/henderson-tech/vybava/internal/mergeassist/gitmerge"
)

// RegenRun is one regen command's outcome.
type RegenRun struct {
	Cmd    string   `json:"cmd"`
	OK     bool     `json:"ok"`
	Staged []string `json:"staged"`
	Output string   `json:"output,omitempty"`
}

// PendingRegen lists commands the drivers asked for since their last
// successful run, each once, in first-request order.
func PendingRegen(events []gitmerge.Event) []string {
	var order []string
	pending := map[string]bool{}
	for _, e := range events {
		switch {
		case e.Class == gitmerge.ClassRegen && e.Outcome == gitmerge.OutcomeResolved:
			pending[e.Path] = false
		case e.Regen != "":
			if _, seen := pending[e.Regen]; !seen {
				order = append(order, e.Regen)
			}
			pending[e.Regen] = true
		}
	}
	var out []string
	for _, cmd := range order {
		if pending[cmd] {
			out = append(out, cmd)
		}
	}
	return out
}

// Regen runs every pending command once from the repo root and stages what
// it rewrote. A failure is journaled and reported, never fatal: the command
// can be rerun once the code conflicts it trips over are resolved.
func (t *Tool) Regen() ([]RegenRun, error) {
	events, err := gitmerge.Events(t.Root)
	if err != nil {
		return nil, err
	}
	runs := []RegenRun{}
	for _, cmdline := range PendingRegen(events) {
		before, err := t.dirty()
		if err != nil {
			return runs, err
		}
		run := RegenRun{Cmd: cmdline, Staged: []string{}}
		cmd := exec.Command("sh", "-c", cmdline)
		cmd.Dir = t.Root
		out, runErr := cmd.CombinedOutput()
		run.OK = runErr == nil
		if !run.OK {
			run.Output = tail(string(out), 8)
		}
		after, err := t.dirty()
		if err != nil {
			return runs, err
		}
		var changed []string
		for p, sum := range after {
			if before[p] != sum {
				changed = append(changed, p)
			}
		}
		sort.Strings(changed)
		if len(changed) > 0 && run.OK { // a failed run's partial output stays unstaged
			if _, err := t.git(append([]string{"add", "--"}, changed...)...); err != nil {
				return runs, err
			}
			run.Staged = changed
		}
		e := gitmerge.Event{Path: cmdline, Class: gitmerge.ClassRegen, Outcome: gitmerge.OutcomeResolved, Detail: strings.Join(changed, " ")}
		if !run.OK {
			e.Outcome, e.Detail = gitmerge.OutcomeFailed, run.Output
		}
		if err := gitmerge.Record(t.Root, e); err != nil {
			return runs, err
		}
		runs = append(runs, run)
	}
	return runs, nil
}

// dirty fingerprints every file differing from the index (unmerged paths
// excluded — those are the session's) and every untracked file, so a regen
// stages exactly what it rewrote and never a session's unstaged edit.
func (t *Tool) dirty() (map[string]string, error) {
	modified, err := t.git("diff", "--name-only", "-z", "--diff-filter=ACMRT")
	if err != nil {
		return nil, err
	}
	untracked, err := t.git("ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	unmerged, err := t.unmerged()
	if err != nil {
		return nil, err
	}
	sums := map[string]string{}
	for _, p := range strings.Split(modified+untracked, "\x00") {
		if p == "" || unmerged[p] != nil {
			continue
		}
		sum, err := fingerprint(filepath.Join(t.Root, p))
		if err != nil {
			return nil, err
		}
		sums[p] = sum
	}
	return sums, nil
}

// unmerged maps each conflicted path to the stages it has (1 base, 2 ours, 3 theirs).
func (t *Tool) unmerged() (map[string][]int, error) {
	out, err := t.git("ls-files", "-u", "-z")
	if err != nil {
		return nil, err
	}
	stages := map[string][]int{}
	for _, rec := range strings.Split(out, "\x00") {
		meta, path, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		f := strings.Fields(meta) // <mode> <object> <stage>
		if len(f) == 3 && len(f[2]) == 1 {
			stages[path] = append(stages[path], int(f[2][0]-'0'))
		}
	}
	return stages, nil
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// fingerprint hashes a file's content, a symlink's target (an untracked
// link to a directory, like a shared node_modules, is never followed) and
// a missing file as empty.
func fingerprint(abs string) (string, error) {
	info, err := os.Lstat(abs)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Sprintf("%x", sha256.Sum256(nil)), nil
	case err != nil:
		return "", err
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(abs)
		if err != nil {
			return "", err
		}
		return "link:" + target, nil
	case info.IsDir():
		return "dir", nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}
