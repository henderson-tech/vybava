package mergeassist

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/henderson-tech/vybava/internal/mergeassist/gitmerge"
)

// Row classes beyond the journal's: what no driver owns.
const (
	ClassCode         = "code"
	ClassModifyDelete = "modify-delete"
)

// Row states.
const (
	StateAuto    = "auto"
	StateOpen    = "open"
	StatePending = "pending"
	StateFailed  = "failed"
)

// Row is one line of the triage table.
type Row struct {
	Path   string `json:"path"`
	Class  string `json:"class"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// Report is the product: one row per path the merge had to settle.
type Report struct {
	Onto       string          `json:"onto"`
	Branch     string          `json:"branch"`
	DryRun     bool            `json:"dryRun"`
	UpToDate   bool            `json:"upToDate,omitempty"`
	Rows       []Row           `json:"rows"`
	Auto       int             `json:"auto"`
	Open       int             `json:"open"`
	Committed  string          `json:"committed,omitempty"`
	Regen      []RegenRun      `json:"regen,omitempty"`
	Migrations []MigrationPlan `json:"migrations,omitempty"`
	Warnings   []string        `json:"warnings,omitempty"`
}

// MergeOptions drive one `merge-assist merge`.
type MergeOptions struct {
	Onto     string // "" = origin's default branch
	DryRun   bool
	NoFetch  bool
	NoCommit bool
}

// Merge merges onto into the current branch with the drivers active, runs
// the regens they asked for, renumbers migrations the base overtook, and
// commits when nothing is left open. DryRun previews with `git merge-tree`
// and touches nothing.
func (t *Tool) Merge(opts MergeOptions) (Report, error) {
	onto := opts.Onto
	if onto == "" {
		onto = t.DefaultRef()
	}
	rep := Report{Onto: onto, DryRun: opts.DryRun, Rows: []Row{}}
	rep.Branch, _ = t.git("branch", "--show-current")
	rep.Branch = strings.TrimSpace(rep.Branch)
	if t.merging() {
		return rep, &Diag{Code: DiagMergeInProgress, Detail: "a merge is already in progress", Fix: "merge-assist status"}
	}
	if !opts.NoFetch {
		if remote, _, ok := strings.Cut(onto, "/"); ok && t.isRemote(remote) {
			if _, err := t.git("fetch", "--quiet", remote); err != nil {
				return rep, err
			}
		}
	}
	// Local and idempotent, so every merge (and preview) runs with the drivers
	// this worktree's config declares.
	if _, err := t.Setup(false); err != nil {
		return rep, err
	}
	if opts.DryRun {
		return t.preview(rep)
	}
	if out, err := t.git("status", "--porcelain", "--untracked-files=no"); err != nil {
		return rep, err
	} else if strings.TrimSpace(out) != "" {
		return rep, &Diag{Code: DiagDirty, Detail: "tracked changes are not committed", Fix: "commit them first; merge-assist merges from a clean tree"}
	}
	if err := gitmerge.Clear(t.Root); err != nil {
		return rep, err
	}
	if _, err := t.git("merge", "--no-ff", "--no-commit", "--no-edit", onto); err != nil && !t.merging() {
		return rep, err
	}
	if !t.merging() {
		rep.UpToDate = true
		return rep, nil
	}
	plans, err := t.PlanMigrations(onto, "")
	if err != nil {
		return rep, err
	}
	if rep.Migrations, err = t.ApplyMigrations(plans); err != nil {
		return rep, err
	}
	if rep.Regen, err = t.Regen(); err != nil {
		return rep, err
	}
	status, err := t.Status()
	if err != nil {
		return rep, err
	}
	rep.Rows, rep.Auto, rep.Open = status.Rows, status.Auto, status.Open
	if rep.Open == 0 && !opts.NoCommit {
		if _, err := t.git("commit", "--no-edit", "--quiet"); err != nil {
			return rep, err
		}
		head, err := t.git("rev-parse", "--short", "HEAD")
		if err != nil {
			return rep, err
		}
		rep.Committed = strings.TrimSpace(head)
	}
	return rep, nil
}

// Status is the table for the merge in progress (or the last one journaled).
func (t *Tool) Status() (Report, error) {
	rep := Report{Rows: []Row{}}
	rep.Branch, _ = t.git("branch", "--show-current")
	rep.Branch = strings.TrimSpace(rep.Branch)
	if head, err := t.git("rev-parse", "-q", "--verify", "MERGE_HEAD"); err == nil {
		rep.Onto = strings.TrimSpace(head)
	}
	events, err := gitmerge.Events(t.Root)
	if err != nil {
		return rep, err
	}
	if rep.Onto == "" && len(events) == 0 {
		return rep, &Diag{Code: DiagNoMerge, Detail: "no merge in progress and nothing journaled", Fix: "merge-assist merge --dry-run"}
	}
	unmerged, err := t.unmerged()
	if err != nil {
		return rep, err
	}
	rep.Rows = t.rows(events, unmerged, func(p string) ([]byte, error) {
		return os.ReadFile(filepath.Join(t.Root, filepath.FromSlash(p)))
	})
	rep.count()
	return rep, nil
}

// preview runs `git merge-tree` with the drivers journaling into a scratch
// file, so the real merge state is untouched.
func (t *Tool) preview(rep Report) (Report, error) {
	scratch, err := os.CreateTemp("", "vybava-merge-preview-*.jsonl")
	if err != nil {
		return rep, err
	}
	scratch.Close()
	defer os.Remove(scratch.Name())
	cmd := exec.Command("git", "merge-tree", "--write-tree", "--no-messages", "HEAD", rep.Onto)
	cmd.Dir = t.Root
	cmd.Env = append(os.Environ(), gitmerge.JournalEnv+"="+scratch.Name())
	out, err := cmd.Output()
	var exit *exec.ExitError
	if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) { // 1 = conflicts
		return rep, fmt.Errorf("git merge-tree: %w", err)
	}
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	tree := ""
	unmerged := map[string][]int{}
	for sc.Scan() {
		line := sc.Text()
		if tree == "" {
			tree = line
			continue
		}
		meta, path, ok := strings.Cut(line, "\t")
		if f := strings.Fields(meta); ok && len(f) == 3 && len(f[2]) == 1 {
			unmerged[path] = append(unmerged[path], int(f[2][0]-'0'))
		}
	}
	events, err := gitmerge.ReadJournal(scratch.Name())
	if err != nil {
		return rep, err
	}
	rep.Rows = t.rows(events, unmerged, func(p string) ([]byte, error) {
		o, err := t.git("show", tree+":"+p)
		return []byte(o), err
	})
	if rep.Migrations, err = t.PlanMigrations(rep.Onto, "HEAD"); err != nil {
		return rep, err
	}
	for _, p := range rep.Migrations {
		for _, r := range p.Renames {
			rep.Rows = append(rep.Rows, Row{Path: p.Dir + "/" + r.To, Class: gitmerge.ClassMigration, State: StateAuto, Detail: "renumbered from " + r.From})
		}
	}
	sortRows(rep.Rows)
	rep.count()
	return rep, nil
}

// rows joins what the drivers journaled with what git left unmerged.
func (t *Tool) rows(events []gitmerge.Event, unmerged map[string][]int, read func(string) ([]byte, error)) []Row {
	rows := []Row{}
	seen := map[string]bool{}
	for _, cmd := range PendingRegen(events) {
		seen[cmd] = true
		rows = append(rows, Row{Path: cmd, Class: gitmerge.ClassRegen, State: StatePending})
	}
	for _, e := range gitmerge.Latest(events) {
		if e.Class == gitmerge.ClassRegen {
			switch {
			case e.Outcome != gitmerge.OutcomeFailed:
				rows = append(rows, Row{Path: e.Path, Class: gitmerge.ClassRegen, State: StateAuto, Detail: e.Detail})
			case len(unmerged) > 0:
				// A generator compiling the code trips over conflict markers;
				// that is expected until the open rows are resolved.
				rows[indexOf(rows, e.Path)].Detail = "rerun after the open rows: merge-assist regen"
			default:
				rows[indexOf(rows, e.Path)].State, rows[indexOf(rows, e.Path)].Detail = StateFailed, lastLine(e.Detail)
			}
			continue
		}
		seen[e.Path] = true
		state := StateAuto
		if unmerged[e.Path] != nil || e.Outcome == gitmerge.OutcomeConflict {
			state = StateOpen
		}
		rows = append(rows, Row{Path: e.Path, Class: e.Class, State: state, Detail: e.Detail})
	}
	for path, stages := range unmerged {
		if seen[path] {
			continue
		}
		row := Row{Path: path, Class: ClassCode, State: StateOpen}
		switch {
		case !hasStage(stages, 2) && hasStage(stages, 1):
			row.Class, row.Detail = ClassModifyDelete, "deleted by us, modified by them"
		case !hasStage(stages, 3) && hasStage(stages, 1):
			row.Class, row.Detail = ClassModifyDelete, "modified by us, deleted by them"
		default:
			if data, err := read(path); err != nil {
				row.Detail = "unreadable: " + err.Error()
			} else {
				row.Detail = hunks(data)
			}
		}
		rows = append(rows, row)
	}
	sortRows(rows)
	return rows
}

func hasStage(stages []int, s int) bool {
	for _, x := range stages {
		if x == s {
			return true
		}
	}
	return false
}

// hunks lists conflict-marker ranges as "L12-30 L88-95" — where to look,
// never what is there.
func hunks(data []byte) string {
	var out []string
	start := 0
	for i, line := range strings.Split(string(data), "\n") {
		switch {
		case strings.HasPrefix(line, "<<<<<<< ") || line == "<<<<<<<":
			start = i + 1
		case (strings.HasPrefix(line, ">>>>>>> ") || line == ">>>>>>>") && start > 0:
			out = append(out, fmt.Sprintf("L%d-%d", start, i+1))
			start = 0
		}
	}
	if len(out) == 0 {
		return "no text markers (binary, or edited)"
	}
	return strings.Join(out, " ")
}

// sortRows: settled first, then what needs attention, open last so the eye
// ends on the work; grouped by class within a state.
func sortRows(rows []Row) {
	rank := map[string]int{StateAuto: 0, StatePending: 1, StateFailed: 2, StateOpen: 3}
	sort.SliceStable(rows, func(i, j int) bool {
		if rank[rows[i].State] != rank[rows[j].State] {
			return rank[rows[i].State] < rank[rows[j].State]
		}
		if rows[i].Class != rows[j].Class {
			return rows[i].Class < rows[j].Class
		}
		return rows[i].Path < rows[j].Path
	})
}

func (r *Report) count() {
	r.Auto, r.Open = 0, 0
	for _, row := range r.Rows {
		switch row.State {
		case StateAuto:
			r.Auto++
		case StateOpen, StateFailed:
			r.Open++
		}
	}
}

func (t *Tool) merging() bool {
	_, err := t.git("rev-parse", "-q", "--verify", "MERGE_HEAD")
	return err == nil
}

func (t *Tool) isRemote(name string) bool {
	out, err := t.git("remote")
	if err != nil {
		return false
	}
	for _, r := range strings.Fields(out) {
		if r == name {
			return true
		}
	}
	return false
}

// DefaultRef is origin's default branch as a remote-tracking ref.
func (t *Tool) DefaultRef() string {
	if out, err := t.git("symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		return strings.TrimSpace(out)
	}
	return "origin/main"
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

func indexOf(rows []Row, path string) int {
	for i, r := range rows {
		if r.Path == path {
			return i
		}
	}
	return -1
}

// WriteTable renders the report for a human or an agent: one header line,
// one row per path, nothing from inside the files.
func (r Report) WriteTable(w io.Writer) error {
	mode := "merge"
	if r.DryRun {
		mode = "preview"
	}
	if r.UpToDate {
		_, err := fmt.Fprintf(w, "%s %s into %s: already up to date\n", mode, r.Onto, r.Branch)
		return err
	}
	head := fmt.Sprintf("%s %s into %s: %d auto, %d open", mode, r.Onto, r.Branch, r.Auto, r.Open)
	if r.Committed != "" {
		head += ", committed " + r.Committed
	}
	if _, err := fmt.Fprintln(w, head); err != nil {
		return err
	}
	width := 4
	for _, row := range r.Rows {
		width = max(width, len(row.Path))
	}
	width = min(width, 72)
	for _, row := range r.Rows {
		path := row.Path
		if len(path) > width {
			path = "…" + path[len(path)-width+1:]
		}
		if _, err := fmt.Fprintf(w, "  %-*s  %-13s  %-7s  %s\n", width, path, row.Class, row.State, row.Detail); err != nil {
			return err
		}
	}
	for _, p := range r.Migrations {
		if p.CheckOK != nil && !*p.CheckOK {
			if _, err := fmt.Fprintf(w, "! migration check failed: %s\n%s\n", p.Check, p.Output); err != nil {
				return err
			}
		}
	}
	for _, warn := range r.Warnings {
		if _, err := fmt.Fprintf(w, "! %s\n", warn); err != nil {
			return err
		}
	}
	return nil
}
