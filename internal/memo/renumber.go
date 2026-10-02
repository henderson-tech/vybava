package memo

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/shellword"
)

// A team ledger is append-only per branch, so two branches that both ran
// `memo add` after their fork give the same #tNN to two different facts.
// The base's ids win: settling a merge keeps the base's LEDGER.md verbatim
// and appends the branch's own rows after its newest, folding a row the base
// already carries into the base's id. Every reference the branch wrote
// moves with its row; a line the base already has keeps the base's meaning.
// Docs: docs/memo.md "Merging a branch".

// Move is one branch row whose id changes when it is settled onto the base.
type Move struct {
	From   int    `json:"from"`
	To     int    `json:"to"`
	Topic  string `json:"topic"`
	Folded bool   `json:"folded,omitempty"` // an identical row already holds To; the branch copy is dropped
}

// Rebased is a branch's ledger settled onto its base's.
type Rebased struct {
	Text  []byte // the base's LEDGER.md verbatim, then the branch's own rows
	Moves []Move // every branch row whose id changed, in branch order
	Added []Row  // the branch's own rows as appended
	Alias string // the base ledger's alias: only alias-qualified refs to it move
	// Doubly lists rows the settled ledger closes twice (both sides
	// superseded or retired the same row): a judgment memo cannot make.
	Doubly []string
}

// MoveMap is the old-to-new id table of the moves.
func (r Rebased) MoveMap() map[int]int {
	m := make(map[int]int, len(r.Moves))
	for _, mv := range r.Moves {
		m[mv.From] = mv.To
	}
	return m
}

// key is a row's identity without its id: two rows with the same key are
// the same fact, whatever number each branch gave it.
func (r Row) key() string {
	r.ID, r.Line = 0, 0
	return r.Format()
}

// RebaseRows settles branch (the home's LEDGER.md: it may hold git conflict
// markers or a "keep both" resolution with duplicate ids) onto base (the
// base ref's LEDGER.md). Rows identical to the base's row of the same id are
// shared; every other row is the branch's own and is appended after the
// base's newest in branch order, its references rewritten first so a
// supersede or link follows the row it names.
func RebaseRows(baseText, branchText []byte) (Rebased, *Diag) {
	base, d, err := parse("base LEDGER.md", bytes.NewReader(baseText))
	if err != nil {
		return Rebased{}, errorDiag(DiagLedgerInvalid, "base ledger: "+err.Error(), "")
	}
	if d != nil {
		d.Detail = "base ledger: " + d.Detail
		return Rebased{}, d
	}
	rows, d := looseRows(branchText, base.Kind)
	if d != nil {
		return Rebased{}, d
	}
	byID := make(map[int]Row, len(base.Rows))
	byKey := map[string]int{}
	for _, r := range base.Rows {
		byID[r.ID] = r
		if _, ok := byKey[r.key()]; !ok {
			byKey[r.key()] = r.ID
		}
	}
	out := Rebased{Alias: base.Alias, Moves: []Move{}}
	moved := map[int]int{}
	handled := map[int]string{} // branch id -> raw key, so a duplicate line is read once
	next := base.NextID()
	for _, r := range rows {
		raw := r.key()
		if b, ok := byID[r.ID]; ok && b.key() == raw {
			continue
		}
		if k, ok := handled[r.ID]; ok && k == raw {
			continue
		}
		from, first := r.ID, true
		if _, ok := handled[r.ID]; ok {
			first = false // two different branch rows share an id: refs keep the first
		}
		handled[r.ID] = raw
		r = r.rewritten(moved, base.Alias)
		to, folded := byKey[r.key()]
		if !folded {
			to, next = next, next+1
			r.ID, r.Line = to, 0
			out.Added = append(out.Added, r)
			byKey[r.key()] = to
		}
		if to != from {
			if first {
				moved[from] = to
			}
			out.Moves = append(out.Moves, Move{From: from, To: to, Topic: r.Type + "/" + r.Topic, Folded: folded})
		}
	}
	var b bytes.Buffer
	b.Write(baseText)
	if len(baseText) > 0 && baseText[len(baseText)-1] != '\n' {
		b.WriteByte('\n')
	}
	for _, r := range out.Added {
		b.WriteString(r.Format())
		b.WriteByte('\n')
	}
	out.Text = b.Bytes()
	settled, d, err := parse("settled LEDGER.md", bytes.NewReader(out.Text))
	if err != nil || d != nil {
		return Rebased{}, diagOrErrDiag(d, err, "the settled ledger does not parse")
	}
	out.Doubly = doublyClosed(settled)
	return out, nil
}

func diagOrErrDiag(d *Diag, err error, what string) *Diag {
	if d != nil {
		d.Detail = what + ": " + d.Detail
		return d
	}
	return errorDiag(DiagLedgerInvalid, what+": "+err.Error(), "")
}

// doublyClosed names every row two rows of l supersede or retire.
func doublyClosed(l *Ledger) []string {
	closers := map[int][]int{}
	for _, r := range l.Rows {
		for _, t := range []int{r.Supersedes(), r.Retires()} {
			if t > 0 {
				closers[t] = append(closers[t], r.ID)
			}
		}
	}
	var out []string
	for t, by := range closers {
		if len(by) > 1 {
			out = append(out, fmt.Sprintf("#t%d is closed by %s", t, citeList(by)))
		}
	}
	sort.Strings(out)
	return out
}

func citeList(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = "#t" + strconv.Itoa(id)
	}
	return strings.Join(parts, " and ")
}

// conflictMarkerRE matches the lines git writes around a conflict, in the
// merge and diff3/zdiff3 styles.
var conflictMarkerRE = regexp.MustCompile(`^(?:<{7}|>{7}|\|{7})(?: |$)|^={7}$`)

// looseRows reads every row of a ledger that may sit mid-merge: frontmatter,
// comments, blank lines and conflict markers are skipped, ids need not
// increase. Anything else must be a row of the home's kind.
func looseRows(data []byte, kind Kind) ([]Row, *Diag) {
	var rows []Row
	inFront := false
	for i, text := range strings.Split(string(data), "\n") {
		line := i + 1
		text = strings.TrimSuffix(text, "\r")
		switch {
		case line == 1 && text == "---":
			inFront = true
			continue
		case inFront:
			inFront = text != "---"
			continue
		case strings.TrimSpace(text) == "", strings.HasPrefix(text, "<!--"), conflictMarkerRE.MatchString(text):
			continue
		}
		r, d := ParseRow(text)
		if d != nil {
			d.Line = line
			d.Detail = fmt.Sprintf("LEDGER.md:%d: %s", line, d.Detail)
			d.Fix = "fix that line by hand (it is neither a row nor a conflict marker), then re-run memo renumber"
			return nil, d
		}
		if r.Team != (kind == KindTeam) {
			return nil, &Diag{Code: DiagLedgerInvalid, Severity: "error", Line: line, Detail: fmt.Sprintf("LEDGER.md:%d: row #%s%d does not use the base ledger's id prefix", line, r.IDPrefix(), r.ID)}
		}
		r.Line = line
		rows = append(rows, r)
	}
	return rows, nil
}

// rewritten returns r with its references moved (sentence and links).
func (r Row) rewritten(moves map[int]int, alias string) Row {
	r.Sentence, _ = RewriteRefs(r.Sentence, moves, alias)
	if len(r.Links) > 0 {
		links := make([]string, len(r.Links))
		for i, l := range r.Links {
			links[i], _ = RewriteRefs(l, moves, alias)
		}
		r.Links = links
	}
	return r
}

// teamRefRE finds the team citation forms, leftmost first:
// [[<alias>/LEDGER#^tN]], <alias>#tN, ^tN (incl. [[LEDGER#^tN]]) and #tN.
var teamRefRE = regexp.MustCompile(`\[\[([a-z0-9]+(?:-[a-z0-9]+)*)/LEDGER#\^t(\d+)\]\]|([a-z0-9]+(?:-[a-z0-9]+)*)#t(\d+)\b|\^t(\d+)\b|#t(\d+)\b`)

// RewriteRefs moves every team citation of s whose id is in moves, in one
// pass (an id is replaced once, so a chain 330->336, 336->342 never
// compounds). A citation qualified by another home's alias, and a bare #tN
// glued to a word, `#`, `&` or `/` (the hook's own citation boundary), are
// left alone. It returns the rewritten text and how many citations moved.
func RewriteRefs(s string, moves map[int]int, alias string) (string, int) {
	if len(moves) == 0 || !strings.Contains(s, "t") {
		return s, 0
	}
	var b strings.Builder
	last, n := 0, 0
	for _, m := range teamRefRE.FindAllStringSubmatchIndex(s, -1) {
		var idAt []int
		switch {
		case m[2] >= 0:
			if s[m[2]:m[3]] == alias {
				idAt = m[4:6]
			}
		case m[6] >= 0:
			if s[m[6]:m[7]] == alias {
				idAt = m[8:10]
			}
		case m[10] >= 0:
			idAt = m[10:12]
		case m[12] >= 0:
			if m[0] == 0 || !citeGlued(s[m[0]-1]) {
				idAt = m[12:14]
			}
		}
		if idAt == nil {
			continue
		}
		id, _ := strconv.Atoi(s[idAt[0]:idAt[1]])
		to, ok := moves[id]
		if !ok {
			continue
		}
		b.WriteString(s[last:idAt[0]])
		b.WriteString(strconv.Itoa(to))
		last = idAt[1]
		n++
	}
	if n == 0 {
		return s, 0
	}
	b.WriteString(s[last:])
	return b.String(), n
}

func citeGlued(c byte) bool {
	return c == '_' || c == '#' || c == '&' || c == '/' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// RefEdit is one file whose branch-written lines had citations moved.
type RefEdit struct {
	Path  string `json:"path"` // repo-relative
	Lines []int  `json:"lines"`
	Refs  int    `json:"refs"`
}

// RenumberResult is what `memo renumber` settled (or, dry, would settle).
type RenumberResult struct {
	Home    string    `json:"home"`
	Base    string    `json:"base"`
	Ledger  string    `json:"ledger"`
	Moves   []Move    `json:"moves"`
	Added   int       `json:"added"`
	Files   []RefEdit `json:"files"`
	Usage   int       `json:"usageEvents"`
	Changed bool      `json:"changed"` // the settled LEDGER.md differs from the one on disk
	Written bool      `json:"written"`
	Merging bool      `json:"merging"`
	// NoBaseLedger: the base carries no ledger at this path, so nothing
	// can collide.
	NoBaseLedger bool `json:"noBaseLedger,omitempty"`
	// Paths are the repo-relative files written, for an explicit `git add`.
	Paths []string `json:"paths"`
	Root  string   `json:"root"`
}

// maxRefFile bounds the files the reference rewrite reads; a bigger one is
// an artifact, not prose that cites a row.
const maxRefFile = 4 << 20

// Renumber settles the home's LEDGER.md onto base (any ref) for a merge:
// the ledger becomes the base's plus the branch's own rows, every citation
// the branch wrote (lines its files hold and the base's copy does not)
// follows its row, the home's local usage.jsonl credits the moved ids, and
// MEMORY.md is rendered. dry computes the same report and writes nothing.
// It refuses a personal home (one writer, never merged across branches)
// and, unless dry, a branch that neither merges the base now nor has
// merged it.
func Renumber(home, base string, dry bool, now time.Time) (RenumberResult, []*Diag, error) {
	res := RenumberResult{Home: home, Ledger: filepath.Join(home, LedgerFile), Moves: []Move{}, Files: []RefEdit{}, Paths: []string{}}
	l, d, err := Load(res.Ledger)
	switch {
	case os.IsNotExist(err):
		return res, nil, errorDiag(DiagHomeNotFound, res.Ledger+" does not exist", "memo homes --json")
	case err == nil && d == nil && l.Kind != KindTeam:
		return res, nil, errorDiag(DiagUsage, home+" is a personal home: one writer, never merged across branches, so its ids cannot collide", "memo renumber --home <repo>/.claude/memory")
	}
	// A ledger mid-merge does not Load (markers, duplicate ids); its kind is
	// the base's, checked row by row when it is read loosely.
	root, ok := gitToplevel(home)
	if !ok {
		return res, nil, errorDiag(DiagBaseUnresolved, home+" is not inside a git work tree, so it has no base to settle onto", "")
	}
	res.Root = root
	res.Merging = refResolves(home, "MERGE_HEAD")
	if base == "" {
		if res.Merging {
			base = strings.TrimSpace(gitStdoutOr(home, "", "rev-parse", "--short", "MERGE_HEAD"))
		} else {
			base = DefaultBase(home)
		}
	}
	retry := "memo renumber --base " + shellword.Quote(base) + " --home " + shellword.Quote(home)
	if base == "" {
		return res, nil, errorDiag(DiagBaseUnresolved, "no base given and none found (no merge in progress, no origin/HEAD, origin/main or GITHUB_BASE_REF)", "memo renumber --base <ref> --home "+shellword.Quote(home))
	}
	res.Base = base
	if !refResolves(home, base) {
		return res, nil, errorDiag(DiagBaseUnresolved, "base "+base+" does not resolve to a commit in "+root, "git -C "+shellword.Quote(root)+" fetch origin && "+retry)
	}
	if !dry && !res.Merging && !isAncestor(home, base, "HEAD") {
		return res, nil, errorDiag(DiagBaseNotMerged, "this branch has not merged "+base+": settling now would copy its rows in and the merge would still conflict; settle the conflict that merge leaves in LEDGER.md", "git -C "+shellword.Quote(root)+" merge "+shellword.Quote(base)+"; "+retry)
	}
	prefix := strings.TrimSpace(gitStdoutOr(home, "", "rev-parse", "--show-prefix"))
	baseText, err := gitStdout(home, "show", base+":./"+LedgerFile)
	if err != nil {
		if gitStdoutOr(home, "missing", "cat-file", "-t", base+":./"+LedgerFile) == "missing" {
			res.NoBaseLedger = true
			return res, nil, nil
		}
		return res, nil, err
	}
	branchText, err := os.ReadFile(res.Ledger)
	if err != nil {
		return res, nil, err
	}
	reb, d := RebaseRows(baseText, branchText)
	if d != nil {
		if d.Fix == "" {
			d.Fix = "memorylint check " + shellword.Quote(home)
		}
		return res, nil, d
	}
	res.Moves, res.Added = reb.Moves, len(reb.Added)
	res.Changed = !bytes.Equal(reb.Text, branchText)
	var warnings []*Diag
	for _, problem := range reb.Doubly {
		warnings = append(warnings, &Diag{Code: DiagTargetDoublyClosed, Severity: "warning", Detail: problem + ": both sides closed it; keep one closer and let the other supersede it", Fix: "memorylint check " + shellword.Quote(home)})
	}
	moves := reb.MoveMap()
	ledgerRel := filepath.ToSlash(filepath.Join(prefix, LedgerFile))
	if len(moves) > 0 {
		skip := map[string]bool{ledgerRel: true, filepath.ToSlash(filepath.Join(prefix, IndexFile)): true, filepath.ToSlash(filepath.Join(prefix, UsageFile)): true}
		if res.Files, err = rewriteBranchRefs(root, base, moves, reb.Alias, skip, dry); err != nil {
			return res, warnings, err
		}
		if res.Usage, err = remapUsage(home, moves, dry); err != nil {
			return res, warnings, err
		}
	}
	if dry {
		return res, warnings, nil
	}
	if res.Changed {
		if err := writeKeepingMode(res.Ledger, reb.Text); err != nil {
			return res, warnings, err
		}
		res.Paths = append(res.Paths, ledgerRel)
	}
	for _, f := range res.Files {
		res.Paths = append(res.Paths, f.Path)
	}
	res.Written = res.Changed || len(res.Files) > 0
	settled, d, err := Load(res.Ledger)
	if err != nil || d != nil {
		return res, warnings, diagOrErrDiag(d, err, "the settled ledger does not load")
	}
	events, d, err := LoadEvents(home)
	if err != nil || d != nil {
		return res, warnings, diagOrErrDiag(d, err, "usage.jsonl")
	}
	if _, tracked, err := WriteIndex(settled, events, now); err != nil {
		return res, warnings, err
	} else if tracked != nil {
		warnings = append(warnings, tracked)
	}
	return res, warnings, nil
}

// rewriteBranchRefs moves the citations on every line the branch wrote: a
// line of a file that differs from base (or is untracked) and that base's
// copy of the file does not hold verbatim. A line the base holds keeps the
// base's meaning, whichever side it reached the work tree from.
func rewriteBranchRefs(root, base string, moves map[int]int, alias string, skip map[string]bool, dry bool) ([]RefEdit, error) {
	changed, err := gitStdout(root, "diff", "--name-only", "--no-relative", "--no-renames", "-z", base, "--")
	if err != nil {
		return nil, err
	}
	untracked, err := gitStdout(root, "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var paths []string
	for _, p := range strings.Split(string(changed)+string(untracked), "\x00") {
		if p != "" && !seen[p] && !skip[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	edits := []RefEdit{}
	for _, p := range paths {
		full := filepath.Join(root, filepath.FromSlash(p))
		info, err := os.Lstat(full)
		if err != nil || !info.Mode().IsRegular() || info.Size() > maxRefFile {
			continue // deleted on the branch, a symlink, or an artifact
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, err
		}
		if bytes.IndexByte(data[:min(len(data), 8000)], 0) >= 0 {
			continue
		}
		if _, n := RewriteRefs(string(data), moves, alias); n == 0 {
			continue
		}
		baseLines := map[string]bool{}
		if old, err := gitStdout(root, "show", base+":"+p); err == nil {
			for _, line := range strings.Split(string(old), "\n") {
				baseLines[strings.TrimSuffix(line, "\r")] = true
			}
		}
		lines := strings.SplitAfter(string(data), "\n")
		edit := RefEdit{Path: p}
		for i, line := range lines {
			body := strings.TrimRight(line, "\r\n")
			if baseLines[body] {
				continue
			}
			moved, n := RewriteRefs(body, moves, alias)
			if n == 0 {
				continue
			}
			lines[i] = moved + line[len(body):]
			edit.Lines = append(edit.Lines, i+1)
			edit.Refs += n
		}
		if edit.Refs == 0 {
			continue
		}
		if !dry {
			if err := os.WriteFile(full, []byte(strings.Join(lines, "")), info.Mode().Perm()); err != nil {
				return nil, err
			}
		}
		edits = append(edits, edit)
	}
	return edits, nil
}

// remapUsage moves the home's local usage events onto the new ids: before
// the settle every one of them credited the branch's row. Only the lines
// that change are re-encoded; the file is replaced through a rename.
func remapUsage(home string, moves map[int]int, dry bool) (int, error) {
	path := filepath.Join(home, UsageFile)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	lines := strings.SplitAfter(string(data), "\n")
	n := 0
	for i, line := range lines {
		body := strings.TrimRight(line, "\n")
		if body == "" {
			continue
		}
		var e Event
		if err := json.Unmarshal([]byte(body), &e); err != nil {
			return 0, &Diag{Code: DiagLedgerInvalid, Severity: "error", Line: i + 1, Detail: fmt.Sprintf("%s:%d is not a usage event", path, i+1), Fix: fmt.Sprintf("remove %s:%d by hand", path, i+1)}
		}
		to, ok := moves[e.Row]
		if !ok {
			continue
		}
		e.Row = to
		enc, err := json.Marshal(e)
		if err != nil {
			return 0, err
		}
		lines[i] = string(enc) + line[len(body):]
		n++
	}
	if n == 0 || dry {
		return n, nil
	}
	tmp := path + ".renumber"
	if err := os.WriteFile(tmp, []byte(strings.Join(lines, "")), 0o644); err != nil {
		return 0, err
	}
	return n, os.Rename(tmp, path)
}

func writeKeepingMode(path string, data []byte) error {
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return os.WriteFile(path, data, mode)
}

// DefaultBase is the ref a home's branch merges into: the pull request's base
// on CI (GITHUB_BASE_REF), else origin's default branch. Empty when none
// resolves (no remote, or a shallow CI checkout that never fetched it).
func DefaultBase(home string) string {
	var candidates []string
	if ref := os.Getenv("GITHUB_BASE_REF"); ref != "" {
		candidates = append(candidates, "origin/"+ref)
	}
	if head := strings.TrimSpace(gitStdoutOr(home, "", "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")); head != "" {
		candidates = append(candidates, head)
	}
	candidates = append(candidates, "origin/main")
	for _, c := range candidates {
		if refResolves(home, c) {
			return c
		}
	}
	return ""
}

func refResolves(dir, ref string) bool {
	_, err := gitStdout(dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	return err == nil
}

func isAncestor(dir, ancestor, of string) bool {
	_, err := gitStdout(dir, "merge-base", "--is-ancestor", ancestor, of)
	return err == nil
}

// gitStdout runs git in dir and returns stdout alone: -z listings and file
// contents must not carry stderr.
func gitStdout(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func gitStdoutOr(dir, fallback string, args ...string) string {
	out, err := gitStdout(dir, args...)
	if err != nil {
		return fallback
	}
	return string(out)
}
