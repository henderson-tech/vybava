package mergeassist

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/henderson-tech/vybava/internal/mergeassist/gitmerge"
)

// A TypeORM migration file: `<ts>-<Name>.ts`, class `<Name><ts>`.
var migrationFile = regexp.MustCompile(`^(\d+)-([A-Za-z_][A-Za-z0-9_]*)\.(ts|js)$`)

type migration struct {
	file string
	ts   int64
	name string
}

func (m migration) ident() string { return m.name + strconv.FormatInt(m.ts, 10) } // class + `name` property
func (m migration) stem() string  { return strconv.FormatInt(m.ts, 10) + "-" + m.name }

// Rename is one planned renumber of a migration the base does not have.
type Rename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// MigrationPlan is one migration directory against the base it merges onto.
type MigrationPlan struct {
	Dir     string   `json:"dir"`
	Onto    string   `json:"onto"`
	BaseMax int64    `json:"baseMax"`
	Renames []Rename `json:"renames"`
	Check   string   `json:"check,omitempty"`
	CheckOK *bool    `json:"checkOk,omitempty"`
	Output  string   `json:"output,omitempty"`
}

// PlanMigrations finds unmerged migrations (in head, absent from onto) that
// sort at or below onto's newest and renumbers them — all of the branch's
// unmerged migrations, in their order, to the first free multiple of Step
// above onto's newest, +1, +2, … A migration onto already has is never
// renamed: its name is recorded in every database that ran it. head "" is
// the working tree.
func (t *Tool) PlanMigrations(onto, head string) ([]MigrationPlan, error) {
	var plans []MigrationPlan
	for _, m := range t.Config.Migrations {
		base, err := t.migrationsAt(onto, m.Dir)
		if err != nil {
			return nil, err
		}
		mine, err := t.migrationsAt(head, m.Dir)
		if err != nil {
			return nil, err
		}
		plan := MigrationPlan{Dir: m.Dir, Onto: onto, Renames: []Rename{}, Check: m.Check}
		onBase := map[string]bool{}
		for _, b := range base {
			onBase[b.file] = true
			plan.BaseMax = max(plan.BaseMax, b.ts)
		}
		var unmerged []migration
		behind := false
		for _, x := range mine {
			if !onBase[x.file] {
				unmerged = append(unmerged, x)
				behind = behind || x.ts <= plan.BaseMax
			}
		}
		if behind {
			step := m.Step
			if step == 0 {
				step = defaultStep
			}
			slot := (plan.BaseMax/step + 1) * step
			for i, x := range unmerged {
				to := migration{ts: slot + int64(i) + 1, name: x.name}
				if r := (Rename{From: x.file, To: to.stem() + path.Ext(x.file)}); r.From != r.To {
					plan.Renames = append(plan.Renames, r)
				}
			}
		}
		plans = append(plans, plan)
	}
	return plans, nil
}

// migrationsAt lists a directory's migrations at a ref ("" = the index, so
// an untracked draft is never taken for a branch migration), oldest first.
func (t *Tool) migrationsAt(ref, dir string) ([]migration, error) {
	var names []string
	if ref == "" {
		out, err := t.git("ls-files", "-z", "--", dir+"/")
		if err != nil {
			return nil, err
		}
		seen := map[string]bool{}
		for _, p := range strings.Split(out, "\x00") {
			name, ok := strings.CutPrefix(p, dir+"/")
			if ok && name != "" && !strings.Contains(name, "/") && !seen[name] { // unmerged paths repeat per stage
				seen[name] = true
				names = append(names, name)
			}
		}
	} else {
		out, err := t.git("ls-tree", "--name-only", ref+":"+dir)
		if err != nil {
			if _, verr := t.git("rev-parse", "--verify", "--quiet", ref+"^{commit}"); verr != nil {
				return nil, err
			}
			out = "" // the directory does not exist on ref yet
		}
		names = strings.Split(strings.TrimSpace(out), "\n")
	}
	var out []migration
	for _, n := range names {
		sub := migrationFile.FindStringSubmatch(n)
		if sub == nil {
			continue
		}
		ts, err := strconv.ParseInt(sub[1], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, migration{file: n, ts: ts, name: sub[2]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ts != out[j].ts {
			return out[i].ts < out[j].ts
		}
		return out[i].file < out[j].file
	})
	return out, nil
}

// ApplyMigrations performs planned renames in the working tree: filename,
// class name and `name` property move together (TypeORM records the class
// name), every other tracked file naming the migration follows, all staged;
// then the repo's own guard runs. Every file is validated before any write.
func (t *Tool) ApplyMigrations(plans []MigrationPlan) ([]MigrationPlan, error) {
	type edit struct {
		from, to migration
		dir      string
	}
	var edits []edit
	for _, p := range plans {
		for _, r := range p.Renames {
			f, tt := migrationFile.FindStringSubmatch(r.From), migrationFile.FindStringSubmatch(r.To)
			from := migration{file: r.From, name: f[2]}
			to := migration{file: r.To, name: tt[2]}
			from.ts, _ = strconv.ParseInt(f[1], 10, 64)
			to.ts, _ = strconv.ParseInt(tt[1], 10, 64)
			body, err := os.ReadFile(filepath.Join(t.Root, filepath.FromSlash(p.Dir), r.From))
			if err != nil {
				return plans, err
			}
			if !regexp.MustCompile(`\bclass\s+` + regexp.QuoteMeta(from.ident()) + `\b`).Match(body) {
				return plans, &Diag{Code: DiagMigrationRefused, Detail: fmt.Sprintf("%s/%s has no `class %s`; TypeORM would keep ordering it by the old timestamp", p.Dir, r.From, from.ident()), Fix: "rename it by hand, keeping filename, class name and `name` in step"}
			}
			edits = append(edits, edit{from: from, to: to, dir: p.Dir})
		}
	}
	unmerged, err := t.unmerged()
	if err != nil {
		return plans, err
	}
	for _, e := range edits {
		oldPath, newPath := path.Join(e.dir, e.from.file), path.Join(e.dir, e.to.file)
		refs, err := t.git("grep", "-l", "-F", "-e", e.from.ident(), "-e", e.from.stem(), "--", ".")
		var exit *exec.ExitError
		if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) { // 1 = no match
			return plans, err
		}
		if _, err := t.git("mv", "--", oldPath, newPath); err != nil {
			return plans, err
		}
		// Read at write time: an earlier rename may already have rewritten this
		// file's reference to another unmerged migration.
		for _, rel := range append([]string{newPath}, strings.Split(strings.TrimSpace(refs), "\n")...) {
			if rel == "" || rel == oldPath {
				continue
			}
			// A conflicted file gets the new name inside its markers but stays
			// unstaged: staging it would declare its conflict resolved.
			if err := t.rewrite(rel, e.from, e.to, unmerged[rel] == nil); err != nil {
				return plans, err
			}
		}
		if err := gitmerge.Record(t.Root, gitmerge.Event{Path: newPath, Class: gitmerge.ClassMigration, Outcome: gitmerge.OutcomeResolved, Detail: "renumbered from " + e.from.file}); err != nil {
			return plans, err
		}
	}
	events, err := gitmerge.Events(t.Root)
	if err != nil {
		return plans, err
	}
	failed := map[string]bool{}
	for _, e := range gitmerge.Latest(events) {
		failed[e.Path] = e.Class == gitmerge.ClassMigration && e.Outcome == gitmerge.OutcomeFailed
	}
	for i, p := range plans {
		// A check that failed earlier reruns even with nothing left to rename:
		// `merge-assist migrations --apply` after the fix is how its row clears.
		if p.Check == "" || (len(p.Renames) == 0 && !failed[checkKey(p.Dir)]) {
			continue
		}
		cmd := exec.Command("sh", "-c", p.Check)
		cmd.Dir = t.Root
		out, err := cmd.CombinedOutput()
		ok := err == nil
		plans[i].CheckOK = &ok
		// Journaled either way: a failed check is an open row, so the merge is
		// never committed over it, and a passing rerun clears it.
		check := gitmerge.Event{Path: checkKey(p.Dir), Class: gitmerge.ClassMigration, Outcome: gitmerge.OutcomeResolved, Detail: p.Check}
		if !ok {
			plans[i].Output = tail(string(out), 8)
			check.Outcome, check.Detail = gitmerge.OutcomeFailed, p.Check+": "+lastLine(plans[i].Output)
		}
		if err := gitmerge.Record(t.Root, check); err != nil {
			return plans, err
		}
	}
	return plans, nil
}

func (t *Tool) rewrite(rel string, from, to migration, stage bool) error {
	abs := filepath.Join(t.Root, filepath.FromSlash(rel))
	data, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	body := strings.ReplaceAll(string(data), from.ident(), to.ident())
	body = strings.ReplaceAll(body, from.stem(), to.stem())
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		return err
	}
	if !stage {
		return nil
	}
	_, err = t.git("add", "--", rel)
	return err
}

// checkKey names a directory's check row by the directory, never the command:
// fixing the failure may mean editing the command itself.
func checkKey(dir string) string { return dir + " (check)" }
