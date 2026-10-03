package uiloop

// Pass evidence is read from files, not reconstructed from agent summaries.
import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

type captureEvidence struct {
	HeadSHA string `json:"headSha"`
}

func (t *Tool) git(args ...string) (CmdOut, error) {
	return RealExec(context.Background(), Cmd{Dir: t.Root, Args: append([]string{"git"}, args...)})
}

// isRevision accepts a full SHA-1 or SHA-256 object name, never a ref.
func isRevision(s string) bool {
	_, err := hex.DecodeString(s)
	return (len(s) == 40 || len(s) == 64) && err == nil
}

// changedSince lists the paths (relative to Root, sorted, never nil) inside
// pathspec whose tracked content differs between head and the working tree,
// both sides of a rename, plus the untracked files git does not ignore. git
// does the pathspec matching, relative to Root unless a pathspec says
// `:(top)`; a path outside Root (a config root below the repo's top) is
// listed with its `../`. ok is false when head is not a revision this clone
// has.
func (t *Tool) changedSince(head string, pathspec ...string) (files []string, ok bool, err error) {
	if !isRevision(head) {
		return nil, false, nil
	}
	prefix, err := t.git("rev-parse", "--show-prefix")
	if err != nil || prefix.Code != 0 {
		return nil, false, err
	}
	files = []string{}
	for _, args := range [][]string{{"diff", "--name-only", "-z", "--no-renames", "--no-relative", head}, {"ls-files", "-z", "--full-name", "--others", "--exclude-standard"}} {
		out, err := t.git(append(append(args, "--"), pathspec...)...)
		if err != nil {
			return nil, false, err
		}
		if out.Code != 0 {
			return nil, false, nil
		}
		for _, file := range strings.Split(out.Stdout, "\x00") {
			if file == "" {
				continue
			}
			rel, err := filepath.Rel(filepath.FromSlash(strings.TrimSpace(prefix.Stdout)), filepath.FromSlash(file))
			if err != nil {
				return nil, false, err
			}
			files = append(files, filepath.ToSlash(rel))
		}
	}
	sort.Strings(files)
	return slices.Compact(files), true, nil
}

// sourceUnchanged reports whether the repo (tracked edits and untracked
// files, outside Root too) still matches head, Config.Out, .vitrinka and the
// ignore dirs aside. Capture counts the rig (Config.Dir) as source; a skip or
// block judges the app and passes it in ignore. State judges a pass by drift
// instead.
func (t *Tool) sourceUnchanged(head string, ignore ...string) (bool, error) {
	files, ok, err := t.changedSince(head, t.sourceSpec(ignore...)...)
	return ok && len(files) == 0, err
}

// sourceSpec is sourceUnchanged's pathspec.
func (t *Tool) sourceSpec(ignore ...string) []string {
	spec := []string{":(top)"}
	for _, dir := range append([]string{t.Config.Out, ".vitrinka"}, ignore...) {
		spec = append(spec, ":(exclude,literal)"+strings.TrimRight(dir, "/"))
	}
	return spec
}

// commitFacts is what validCheckpoint asks git of the commits a pass's
// checkpoints name: the ancestors of HEAD, and of those the open (not done)
// checkpoints' commits the app is unchanged since (sourceUnchanged, the rig
// ignored). A pass holds a checkpoint per item but a handful of fix commits,
// and a git process per checkpoint was most of a state call (fixit/5334), so
// checkpointFacts asks for all of them in a fixed number of calls.
type commitFacts struct {
	ancestor, unchanged map[string]bool
}

func (t *Tool) checkpointFacts(basis string, cps []Checkpoint) (commitFacts, error) {
	facts := commitFacts{ancestor: map[string]bool{}, unchanged: map[string]bool{}}
	var commits []string
	for _, c := range cps {
		if c.Basis == basis && isRevision(c.Commit) {
			commits = append(commits, c.Commit)
		}
	}
	slices.Sort(commits)
	if len(commits) == 0 {
		return facts, nil
	}
	head, objects, err := t.ancestorsOfHead(slices.Compact(commits))
	if err != nil {
		return facts, err
	}
	var open []string
	for c := range objects {
		facts.ancestor[c] = true
	}
	for _, c := range cps {
		if facts.ancestor[c.Commit] && c.Basis == basis && c.Status != "done" {
			open = append(open, c.Commit)
		}
	}
	slices.Sort(open)
	facts.unchanged, err = t.unchangedSince(head, objects, slices.Compact(open), t.Config.Dir)
	return facts, err
}

// ancestorsOfHead is `git merge-base --is-ancestor <c> HEAD` for each of
// commits in two git calls: HEAD's object name, and each ancestor's commit
// object by its given name. cat-file drops the names this clone lacks (never
// ancestors) so one gc'd commit cannot fail rev-list for the rest, which then
// lists everything they reach that HEAD does not: exactly the non-ancestors
// among them. A git that answers no (not a repository, HEAD unborn) makes
// none an ancestor, as merge-base's exit code did.
func (t *Tool) ancestorsOfHead(commits []string) (head string, objects map[string]string, err error) {
	objects = map[string]string{}
	names := append([]string{"HEAD"}, commits...)
	out, err := RealExec(context.Background(), Cmd{Dir: t.Root, Args: []string{"git", "cat-file", "--batch-check=%(objectname) %(objecttype)"},
		Stdin: strings.NewReader(strings.Join(names, "^{commit}\n") + "^{commit}\n")})
	if err != nil || out.Code != 0 {
		return "", objects, err
	}
	lines := strings.Split(strings.TrimSuffix(out.Stdout, "\n"), "\n")
	if len(lines) != len(names) {
		return "", nil, fmt.Errorf("git cat-file --batch-check: %d answers for %d commits", len(lines), len(names))
	}
	object := func(line string) string {
		if f := strings.Fields(line); len(f) == 2 && f[1] == "commit" {
			return f[0]
		}
		return ""
	}
	if head = object(lines[0]); head == "" {
		return "", objects, nil
	}
	revs := []string{"^" + head}
	for i, c := range commits {
		if o := object(lines[i+1]); o != "" {
			objects[c] = o
			revs = append(revs, o)
		}
	}
	walk, err := RealExec(context.Background(), Cmd{Dir: t.Root, Args: []string{"git", "rev-list", "--stdin"}, Stdin: strings.NewReader(strings.Join(revs, "\n") + "\n")})
	if err != nil || walk.Code != 0 {
		return "", map[string]string{}, err
	}
	outside := map[string]bool{}
	for _, line := range strings.Fields(walk.Stdout) {
		outside[line] = true
	}
	maps.DeleteFunc(objects, func(_, o string) bool { return outside[o] })
	return head, objects, nil
}

// unchangedSince is sourceUnchanged(c, ignore...) for each of commits, all
// ancestors of HEAD (head; objects maps each to its commit object), in three
// git calls. The untracked files and the working tree's changes against HEAD
// (W) are the same for every commit; `diff-tree --stdin` lists each commit's
// changes against HEAD (T). A path in only one of W and T differs between
// the commit and the working tree, so unequal sets answer false and two empty
// ones true; equal non-empty sets (a working tree that undoes commits) and
// any git failure ask sourceUnchanged itself.
func (t *Tool) unchangedSince(head string, objects map[string]string, commits []string, ignore ...string) (map[string]bool, error) {
	unchanged := map[string]bool{}
	if len(commits) == 0 {
		return unchanged, nil
	}
	spec := t.sourceSpec(ignore...)
	untracked, err := t.git(append([]string{"ls-files", "-z", "--full-name", "--others", "--exclude-standard", "--"}, spec...)...)
	if err != nil {
		return nil, err
	}
	if untracked.Code == 0 && untracked.Stdout != "" {
		return unchanged, nil
	}
	worktree, err := t.git(append([]string{"diff", "--name-only", "-z", "--no-renames", "--no-relative", head, "--"}, spec...)...)
	if err != nil {
		return nil, err
	}
	var pairs, order []string
	for _, c := range commits {
		if !slices.Contains(order, objects[c]) {
			order = append(order, objects[c])
			pairs = append(pairs, objects[c]+" "+head+"\n")
		}
	}
	trees, err := RealExec(context.Background(), Cmd{Dir: t.Root, Args: append([]string{"git", "diff-tree", "--stdin", "-z", "--always", "-r", "--name-status", "--no-renames", "--"}, spec...),
		Stdin: strings.NewReader(strings.Join(pairs, ""))})
	if err != nil {
		return nil, err
	}
	changed, parsed := diffTreeNames(trees.Stdout, order)
	w := strings.Split(strings.TrimSuffix(worktree.Stdout, "\x00"), "\x00")
	if worktree.Stdout == "" {
		w = []string{}
	}
	slices.Sort(w)
	w = slices.Compact(w)
	for _, c := range commits {
		known := untracked.Code == 0 && worktree.Code == 0 && trees.Code == 0 && parsed
		switch {
		case known && !slices.Equal(changed[objects[c]], w):
		case known && len(w) == 0:
			unchanged[c] = true
		default:
			if unchanged[c], err = t.sourceUnchanged(c, ignore...); err != nil {
				return nil, err
			}
		}
	}
	return unchanged, nil
}

// diffTreeNames splits `diff-tree --stdin -z --always --name-status` output
// into each commit's sorted paths. A header is a commit's object name and a
// status one letter before its path (no renames, so no scores), so the two
// never mix; ok is false unless the headers are exactly commits, in order.
func diffTreeNames(out string, commits []string) (names map[string][]string, ok bool) {
	names = map[string][]string{}
	tokens := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	if out == "" {
		tokens = nil
	}
	seen := 0
	for i := 0; i < len(tokens); i++ {
		switch {
		case len(tokens[i]) == 1 && seen > 0 && i+1 < len(tokens):
			names[commits[seen-1]] = append(names[commits[seen-1]], tokens[i+1])
			i++
		case seen < len(commits) && tokens[i] == commits[seen]:
			names[commits[seen]] = []string{}
			seen++
		default:
			return nil, false
		}
	}
	for c, paths := range names {
		slices.Sort(paths)
		names[c] = slices.Compact(paths)
	}
	return names, seen == len(commits)
}

// Drift is what changed between a pass's captured revision and the working
// tree, by what it stales. Only App stales the pass: the rig is repaired
// between review and verify, and a spec edit is reported by rule, never
// recaptured.
type Drift struct {
	// App is the changed paths inside Config.SourceOrDefault.
	App []string `json:"app"`
	// Rig is the changed paths under Config.Dir, vendor/ included.
	Rig []string `json:"rig"`
	// Spec is the rule ids whose text changed in Config.Spec (specDrift).
	Spec []string `json:"spec"`
	// AppTotal is len(App) before state caps it at driftCap.
	AppTotal int `json:"appTotal"`
}

// driftCap is the paths state lists per drift class.
const driftCap = 20

// capped is d as state reports it: App and Rig cut to driftCap.
func (d Drift) capped() Drift {
	d.App, d.Rig = d.App[:min(len(d.App), driftCap)], d.Rig[:min(len(d.Rig), driftCap)]
	return d
}

// drift classifies the change since head, every list complete and never
// nil; ok is false when head is not a revision this clone has.
func (t *Tool) drift(head string) (d Drift, ok bool, err error) {
	if d.App, ok, err = t.changedSince(head, t.Config.SourceOrDefault()...); err != nil || !ok {
		return Drift{}, ok, err
	}
	if d.Rig, ok, err = t.changedSince(head, ":(literal)"+strings.TrimRight(t.Config.Dir, "/")); err != nil || !ok {
		return Drift{}, ok, err
	}
	if d.Spec, err = t.specDrift(head); err != nil {
		return Drift{}, false, err
	}
	d.AppTotal = len(d.App)
	return d, true, nil
}

var (
	ruleID  = regexp.MustCompile(`\b[A-Z][A-Z0-9]*-[A-Z]*[0-9]+\b`)
	heading = regexp.MustCompile(`^ {0,3}(#{1,6})\s+(.*?)[\s#]*$`)
	hunk    = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)
)

// specDrift is the rules whose text changed in the spec between head and
// the working tree, sorted. A changed (added or removed) line names the ids
// it carries, else the first id of the nearest line above it in its section,
// else of the nearest enclosing heading that carries one, else its section's
// heading text, else the spec's path.
func (t *Tool) specDrift(head string) ([]string, error) {
	rules := []string{}
	if t.Config.Spec == "" {
		return rules, nil
	}
	out, err := t.git("diff", "-U0", "--inter-hunk-context=0", "--no-color", "--no-ext-diff", head, "--", t.Config.Spec)
	if err != nil {
		return nil, err
	}
	if out.Code != 0 {
		return nil, fmt.Errorf("git diff %s -- %s: exit %d: %s", head, t.Config.Spec, out.Code, strings.TrimSpace(out.Stderr))
	}
	if out.Stdout == "" {
		return rules, nil
	}
	before, _, err := t.committedFile(head, t.Config.Spec)
	if err != nil {
		return nil, err
	}
	after, err := os.ReadFile(t.abs(t.Config.Spec))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	sides := [2][]string{strings.Split(before, "\n"), strings.Split(string(after), "\n")}
	// ruleAt names what line n (1-based) of lines belongs to. Walking up, a
	// heading closes the section: past it only an enclosing (shallower)
	// heading can still name the rule, so an earlier section's last rule
	// never claims an edit to the next section's prose.
	ruleAt := func(lines []string, n int) []string {
		if n < 1 || n > len(lines) {
			return []string{t.Config.Spec}
		}
		if ids := ruleID.FindAllString(lines[n-1], -1); len(ids) > 0 {
			return ids
		}
		level, section := 7, ""
		for i := n - 1; i >= 0; i-- {
			m := heading.FindStringSubmatch(lines[i])
			if (m == nil && level < 7) || (m != nil && len(m[1]) >= level) {
				continue
			}
			if ids := ruleID.FindAllString(lines[i], -1); len(ids) > 0 {
				return ids[:1]
			}
			if m != nil {
				level = len(m[1])
				if section == "" {
					section = m[2]
				}
			}
		}
		if section != "" {
			return []string{section}
		}
		return []string{t.Config.Spec}
	}
	// next is each side's 1-based line number of the hunk line being read.
	var next [2]int
	inHunk := false
	for _, line := range strings.Split(out.Stdout, "\n") {
		if m := hunk.FindStringSubmatch(line); m != nil {
			for side := range next {
				if next[side], err = strconv.Atoi(m[side+1]); err != nil {
					return nil, fmt.Errorf("git diff %s -- %s: hunk %q: %w", head, t.Config.Spec, line, err)
				}
			}
			inHunk = true
			continue
		}
		if !inHunk || line == "" {
			continue
		}
		switch line[0] {
		case '-':
			rules = append(rules, ruleAt(sides[0], next[0])...)
			next[0]++
		case '+':
			rules = append(rules, ruleAt(sides[1], next[1])...)
			next[1]++
		case ' ':
			next[0]++
			next[1]++
		}
	}
	sort.Strings(rules)
	return slices.Compact(rules), nil
}

// committedFile reads rel (relative to Root) at head, a revision this clone
// has; found is false when head holds no such path.
func (t *Tool) committedFile(head, rel string) (body string, found bool, err error) {
	object := head + ":./" + filepath.ToSlash(rel)
	out, err := t.git("cat-file", "-e", object)
	if err != nil || out.Code != 0 {
		return "", false, err
	}
	if out, err = t.git("cat-file", "blob", object); err != nil {
		return "", false, err
	}
	if out.Code != 0 {
		return "", false, fmt.Errorf("git cat-file blob %s: exit %d: %s", object, out.Code, strings.TrimSpace(out.Stderr))
	}
	return out.Stdout, true, nil
}

func (t *Tool) backlogEvidenceCurrent(pass int, file string, knownBasis ...string) (bool, error) {
	var marker captureEvidence
	found, err := readJSON(filepath.Join(t.passAbs(pass), "capture.json"), &marker)
	if err != nil {
		return false, err
	}
	if !found {
		return true, nil
	}
	basis, err := t.cachedReviewBasis(pass, knownBasis)
	if err != nil {
		return false, err
	}
	var receipt struct {
		Basis string `json:"basis"`
	}
	if _, err := readJSON(filepath.Join(filepath.Dir(file), "basis.json"), &receipt); err != nil {
		return false, err
	}
	return receipt.Basis == basis, nil
}

// Persist BEFORE the interruptible runner. A resume never replaces the original revision.
func (t *Tool) captureProvenance(pass int, resume bool) error {
	file := filepath.Join(t.passAbs(pass), "capture.json")
	var marker captureEvidence
	found, err := readJSON(file, &marker)
	if err != nil {
		return err
	}
	if resume && found {
		clean, err := t.sourceUnchanged(marker.HeadSHA)
		if err != nil {
			return err
		}
		if !clean {
			return diag(DiagSelectionInvalid, "application source differs from the captured revision", "capture a new pass without --resume")
		}
		return nil
	}
	if resume {
		return nil
	} // Legacy CLI passes remain usable; workflows require provenance.
	head, err := t.git("rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	if head.Code != 0 {
		return nil
	} // Captures outside git have no verified source revision.
	marker.HeadSHA = strings.TrimSpace(head.Stdout)
	clean, err := t.sourceUnchanged(marker.HeadSHA)
	if err != nil {
		return err
	}
	if !clean {
		return diag(DiagSelectionInvalid, "application source has uncommitted changes", "commit the application changes before capture")
	}
	return writeJSON(file, marker)
}

// Content basis: compact JSON of sorted [relative filename, SHA256(bytes)]
// pairs over the pass's shots, the manifest (*.ts under Config.Dir, vendor/
// skipped) and the spec. A pass's evidence is immutable: with provenance its
// manifest and its spec are read at the captured revision, so neither a rig
// repair nor a spec edit committed after capture stales the reviews, backlog
// and checkpoints of that pass; state reports the spec edit as drift.spec.
func (t *Tool) reviewBasis(pass int) (string, error) {
	basis, _, _, err := t.reviewEvidence(pass)
	return basis, err
}

func (t *Tool) cachedReviewBasis(pass int, known []string) (string, error) {
	if len(known) > 0 {
		return known[0], nil
	}
	return t.reviewBasis(pass)
}

// passSnapshot is one request's read of a pass's evidence (reviewEvidence):
// the basis and the SHA256 of every file it covers, keyed relative to Root,
// and its shot records, so a request hashes and decodes the shots once.
type passSnapshot struct {
	basis   string
	hashes  map[string]string
	records []Record
}

// snapshot is known[0], else the pass's evidence read now.
func (t *Tool) snapshot(pass int, known []passSnapshot) (passSnapshot, error) {
	if len(known) > 0 {
		return known[0], nil
	}
	basis, hashes, _, err := t.reviewEvidence(pass)
	if err != nil {
		return passSnapshot{}, err
	}
	records, err := LoadRecords(t.passAbs(pass))
	return passSnapshot{basis: basis, hashes: hashes, records: records}, err
}

// screenEntry is a screen's manifest entry as its shot records recorded it at
// capture (harness/capture.spec.ts copies it into every record).
type screenEntry struct {
	ID          string   `json:"id"`
	App         string   `json:"app"`
	Area        string   `json:"area"`
	Kind        string   `json:"kind"`
	State       string   `json:"state"`
	Title       string   `json:"title"`
	Route       string   `json:"route"`
	ParentID    string   `json:"parentId"`
	VariantOf   string   `json:"variantOf"`
	As          string   `json:"as"`
	SourceFiles []string `json:"sourceFiles"`
	KnownIssues []string `json:"knownIssues"`
	Destructive bool     `json:"destructive"`
}

// screenEvidence is what a screen's review judged: shots are the sorted
// [file, SHA256] pairs of its ok shots' PNGs (file relative to the pass
// directory), screen is its manifest entry as its first record recorded it (a
// resume refuses a changed rig, so every record of a pass agrees) and spec is
// the spec's SHA256 as the basis read it ("" without one).
type screenEvidence struct {
	Shots  [][2]string `json:"shots"`
	Screen screenEntry `json:"screen"`
	Spec   string      `json:"spec"`
}

// screenDigests is each screen's review evidence digest: the SHA256 of the
// compact JSON of its screenEvidence. A --resume retake that changes a PNG
// moves its screen's digest and no other; a rig or spec edit after capture
// moves none.
func (t *Tool) screenDigests(pass int, records []Record, hashes map[string]string) (map[string]string, error) {
	byID, err := t.screenEvidence(pass, records, hashes)
	if err != nil {
		return nil, err
	}
	digests := make(map[string]string, len(byID))
	for id, e := range byID {
		var b bytes.Buffer
		enc := json.NewEncoder(&b)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(e); err != nil {
			return nil, err
		}
		digests[id] = digest(strings.TrimSuffix(b.String(), "\n"), 64)
	}
	return digests, nil
}

// screenEvidence is each screen's screenEvidence in pass, shots sorted.
func (t *Tool) screenEvidence(pass int, records []Record, hashes map[string]string) (map[string]*screenEvidence, error) {
	prefix, err := filepath.Rel(t.Root, t.passAbs(pass))
	if err != nil {
		return nil, err
	}
	spec := ""
	if t.Config.Spec != "" {
		rel, err := filepath.Rel(t.Root, t.abs(t.Config.Spec))
		if err != nil {
			return nil, err
		}
		spec = hashes[filepath.ToSlash(rel)]
	}
	byID := map[string]*screenEvidence{}
	for _, r := range records {
		e := byID[r.ID]
		if e == nil {
			e = &screenEvidence{Shots: [][2]string{}, Spec: spec, Screen: screenEntry{
				ID: r.ID, App: r.App, Area: r.Area, Kind: r.Kind, State: r.State, Title: r.Title, Route: r.Route,
				ParentID: r.ParentID, VariantOf: r.VariantOf, As: r.As, SourceFiles: r.SourceFiles, KnownIssues: r.KnownIssues, Destructive: r.Destructive,
			}}
			byID[r.ID] = e
		}
		if r.Status != "ok" {
			continue
		}
		for _, name := range []string{r.Files.Viewport, r.Files.Full} {
			if name != "" {
				file := path.Join(r.Dir, name)
				e.Shots = append(e.Shots, [2]string{file, hashes[path.Join(filepath.ToSlash(prefix), file)]})
			}
		}
	}
	for _, e := range byID {
		sort.Slice(e.Shots, func(i, j int) bool { return e.Shots[i][0] < e.Shots[j][0] })
	}
	return byID, nil
}

// reviewEvidence is the basis, the per-file hashes it covers (keyed relative to
// Root) and a warning when a strict pass's manifest and spec had to be read
// from the working tree because its captured revision is not in this clone.
func (t *Tool) reviewEvidence(pass int) (string, map[string]string, []runxDiagnostic, error) {
	entries := [][2]string{}
	hashes := map[string]string{}
	cache := t.openDigestCache(pass)
	add := func(file string) error {
		rel, err := filepath.Rel(t.Root, file)
		if err != nil {
			return err
		}
		sum, found, err := cache.sum(file, filepath.ToSlash(rel))
		if err != nil || !found {
			return err
		}
		hashes[filepath.ToSlash(rel)] = sum
		entries = append(entries, [2]string{filepath.ToSlash(rel), sum})
		return nil
	}
	dirs := []string{filepath.Join(t.passAbs(pass), "shots")}
	var marker captureEvidence
	strict, err := readJSON(filepath.Join(t.passAbs(pass), "capture.json"), &marker)
	if err != nil {
		return "", nil, nil, err
	}
	var diags []runxDiagnostic
	committed := false
	if strict && isRevision(marker.HeadSHA) {
		var manifest map[string]string
		if manifest, committed, err = t.committedManifest(marker.HeadSHA); err != nil {
			return "", nil, nil, err
		}
		if !committed {
			diags = append(diags, warn(DiagCaptureRevisionMissing,
				fmt.Sprintf("%s was captured at %s, which is not in this clone; its manifest and spec basis is read from the working tree, so a rig or spec change since capture stales its reviews, backlog and checkpoints", t.PassDir(pass), marker.HeadSHA),
				"fetch that revision, or capture a new pass"))
		}
		for rel, hash := range manifest {
			hashes[rel] = hash
			entries = append(entries, [2]string{rel, hash})
		}
	}
	if !committed {
		dirs = append(dirs, t.abs(t.Config.Dir))
	}
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(file string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "vendor" {
					return filepath.SkipDir
				}
				return nil
			}
			ext := filepath.Ext(file)
			if (strings.HasPrefix(file, filepath.Join(t.passAbs(pass), "shots")+string(filepath.Separator)) && (ext == ".json" || ext == ".png")) || ext == ".ts" {
				return add(file)
			}
			return nil
		})
		if err != nil {
			return "", nil, nil, err
		}
	}
	switch {
	case t.Config.Spec == "":
	case committed:
		body, found, err := t.committedFile(marker.HeadSHA, t.Config.Spec)
		if err != nil {
			return "", nil, nil, err
		}
		rel, err := filepath.Rel(t.Root, t.abs(t.Config.Spec))
		if err != nil {
			return "", nil, nil, err
		}
		if found {
			hashes[filepath.ToSlash(rel)] = digest(body, 64)
			entries = append(entries, [2]string{filepath.ToSlash(rel), hashes[filepath.ToSlash(rel)]})
		}
	default:
		if err := add(t.abs(t.Config.Spec)); err != nil {
			return "", nil, nil, err
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i][0] < entries[j][0] })
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(entries); err != nil {
		return "", nil, nil, err
	}
	return digest(strings.TrimSuffix(b.String(), "\n"), 64), hashes, append(diags, cache.save()...), nil
}

// committedManifest hashes the manifest's blobs at head, keyed like the
// working-tree walk (paths relative to Root). ok is false only when this clone
// lacks head (gc'd after its branch went, a pass copied from another clone):
// the caller reads the working tree as a legacy pass does and warns. Config
// validation keeps Dir inside Root, so any other git failure is an error.
func (t *Tool) committedManifest(head string) (hashes map[string]string, ok bool, err error) {
	present, err := t.git("cat-file", "-e", head+"^{commit}")
	if err != nil || present.Code != 0 {
		return nil, false, err
	}
	dir, err := filepath.Rel(t.Root, t.abs(t.Config.Dir))
	if err != nil {
		return nil, false, err
	}
	dir = filepath.ToSlash(dir)
	tree, err := t.git("ls-tree", "-r", "-z", head, "--", dir)
	if err != nil {
		return nil, false, err
	}
	if tree.Code != 0 {
		return nil, false, fmt.Errorf("git ls-tree %s -- %s: exit %d: %s", head, dir, tree.Code, strings.TrimSpace(tree.Stderr))
	}
	var files, oids []string
	for _, row := range strings.Split(tree.Stdout, "\x00") {
		meta, file, _ := strings.Cut(row, "\t")
		f := strings.Fields(meta) // mode, type, object
		if len(f) != 3 || f[1] != "blob" || path.Ext(file) != ".ts" ||
			slices.Contains(strings.Split(path.Dir(strings.TrimPrefix(file, dir+"/")), "/"), "vendor") {
			continue
		}
		files, oids = append(files, file), append(oids, f[2])
	}
	hashes = map[string]string{}
	if len(files) == 0 {
		return hashes, true, nil
	}
	out, err := RealExec(context.Background(), Cmd{Dir: t.Root, Args: []string{"git", "cat-file", "--batch"}, Stdin: strings.NewReader(strings.Join(oids, "\n") + "\n")})
	if err != nil {
		return nil, false, err
	}
	if out.Code != 0 {
		return nil, false, fmt.Errorf("git cat-file --batch at %s: exit %d: %s", head, out.Code, strings.TrimSpace(out.Stderr))
	}
	rest := out.Stdout
	for i, file := range files {
		// Each record is "<object> <type> <size>\n<size bytes>\n".
		header, body, _ := strings.Cut(rest, "\n")
		f := strings.Fields(header)
		size := -1
		if len(f) == 3 && f[0] == oids[i] {
			if n, err := strconv.Atoi(f[2]); err == nil {
				size = n
			}
		}
		if size < 0 || len(body) <= size {
			return nil, false, fmt.Errorf("git cat-file --batch at %s: %s: unexpected record %q", head, file, header)
		}
		hashes[file] = digest(body[:size], 64)
		rest = body[size+1:]
	}
	return hashes, true, nil
}

func (t *Tool) scoreBasis(pass int, basis string) (string, error) {
	parts := []string{basis}
	for _, file := range []string{"review/backlog.json", "publish/index.json", "publish/boards.json", "scoreboard.json", "scoreboard.md"} {
		b, err := os.ReadFile(filepath.Join(t.passAbs(pass), file))
		if errors.Is(err, fs.ErrNotExist) {
			parts = append(parts, "")
			continue
		}
		if err != nil {
			return "", err
		}
		parts = append(parts, digest(string(b), 64))
	}
	return digest(strings.Join(parts, "\n"), 64), nil
}

func (t *Tool) validCheckpoint(c Checkpoint, basis string, facts commitFacts) (bool, error) {
	if c.Basis != basis || !isRevision(c.Commit) || !facts.ancestor[c.Commit] {
		return false, nil
	}
	if c.Status != "done" {
		// A rig repair committed after the round leaves the app it judged alone.
		return facts.unchanged[c.Commit], nil
	}
	if len(c.FileDigests) == 0 {
		return false, nil
	}
	for file, hash := range c.FileDigests {
		rel, ok := repoRel(t.Root, file)
		if !ok {
			return false, nil
		}
		b, err := os.ReadFile(t.abs(rel))
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if digest(string(b), 64) != hash {
			return false, nil
		}
	}
	return true, nil
}
