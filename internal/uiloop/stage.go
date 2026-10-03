package uiloop

// stage.go owns the pass state the vitrinka review-loop reads back between
// its stages: `state` (compact counts and the next stage), `batches` (the
// reviewer batches, persisted), `merge-review` (the backlog draft), `lanes`
// (the fix lanes by ownership, in lanes.go) and `checkpoints` (the fix
// checkpoints that count). The workflow's agents relay these envelopes; they
// never enumerate screens, raw files, backlog items or checkpoints themselves,
// because a large list handed through an agent's return value gets dropped or
// summarized (pwf-ui pass 1).

import (
	"cmp"
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
	"strings"
	"time"
)

// DefaultBatchSize is the screens a reviewer judges in one batch.
const DefaultBatchSize = 14

// Batch is one reviewer batch: an area's screens, sorted by id, in chunks.
type Batch struct {
	ID      string   `json:"id"`
	Area    string   `json:"area"`
	Screens []string `json:"screens"`
	// Digests (v2) is each screen's screenDigests digest: a reviewer copies
	// the ones of the screens it read into its raw file's screens.
	Digests map[string]string `json:"digests,omitempty"`
	// Parts (batches --split) name the halves the batch was split into
	// after a stall; they follow it in batches.json, and it is reviewed only
	// through them (stall.go).
	Parts []string `json:"parts,omitempty"`
	// Blocked (batches --block) takes the batch out of the review: it is
	// never left or claimed again, and merge-review lists its unread screens
	// as unreviewed, "<id> (stalled: <BlockedReason>)".
	Blocked       bool   `json:"blocked,omitempty"`
	BlockedReason string `json:"blockedReason,omitempty"`
}

// Carried is a screen no batch holds: its review comes from pass From
// (planCarry). Digest is its screenDigests digest when batches planned the
// carry, kept in batches.json only: a retake since moves it, and the screen
// is reopened (carriesNow).
type Carried struct {
	Screen string `json:"screen"`
	From   int    `json:"from"`
	Digest string `json:"digest,omitempty"`
}

// BatchesFile is <pass>/review/batches.json: the one definition of the
// pass's review batches, shared by the review stage and merge-review. v1
// batches are judged by the whole-pass basis; v2 batches carry per-screen
// digests and the carried screens.
type BatchesFile struct {
	V       int       `json:"v"`
	Pass    int       `json:"pass"`
	Size    int       `json:"size"`
	Batches []Batch   `json:"batches"`
	Carried []Carried `json:"carried,omitempty"`
}

// Checkpoint is one fix item's <pass>/fix/<key>.json. A key with "/" is a
// subdirectory; readers take the item from Key, else from that path
// (loadCheckpoints).
type Checkpoint struct {
	Basis       string            `json:"basis"`
	FileDigests map[string]string `json:"fileDigests,omitempty"`
	APIChanges  []string          `json:"apiChanges,omitempty"`
	Key         string            `json:"key"`
	Lane        string            `json:"lane,omitempty"`
	Status      string            `json:"status"` // done | skipped | blocked
	Commit      string            `json:"commit,omitempty"`
	Screens     []string          `json:"screens,omitempty"`
	// I18n is the strings the fix needs ({key, <locale>: text}), passed
	// through verbatim: its shape is the settle step's, never a reason to
	// refuse the checkpoint.
	I18n json.RawMessage `json:"i18n,omitempty"`
	Note string          `json:"note,omitempty"`
}

// Finishes reports whether the checkpoint closes its item for this round.
func (c Checkpoint) Finishes() bool {
	return c.Status == "done" || c.Status == "skipped" || c.Status == "blocked"
}

// NextStage is where a pass stands (nextStage); the vitrinka workflow runs it verbatim.
type NextStage struct {
	Stage  string `json:"stage"` // capture | review | fix | verify | done | wait
	Resume bool   `json:"resume"`
	Reason string `json:"reason"`
	// Only is the screens a verify, or a capture that reshoots the pass,
	// shoots, sorted; empty is a full reshoot (Reason says why). nil on
	// every other stage.
	Only []string `json:"only"`
	// Parallel is how many identical review runs to launch (reviewParallel);
	// on review only.
	Parallel int `json:"parallel,omitempty"`
}

// StateContract is the shape of `state`'s data the vitrinka workflow reads
// (workflows-src/lib/uiloop.js UILOOP_STATE_CONTRACT, which refuses a lower
// one). Bump it whenever a field a workflow reads is added or changes format,
// digests included. 2: drift, sourceUnchanged as "drift.app is empty",
// next.only and config.source/primitives. 3: review.carried and
// review.carriedFrom, backlog.carried (byStatus without carried items),
// batches v2 (per-screen digests, carried) and raws judged screen by screen.
// 4: pass leases — capture, pending, next.stage wait, next.parallel and
// batches' claimed. 5: split on stall — review.stalls and review.blocked,
// batches' parts and blocked, and planned/done/left counting parts, never a
// split batch.
const StateContract = 5

// reviewParallel is how many identical review runs the unclaimed left
// batches keep busy: one per reviewersPerRun batches, at most
// maxReviewRuns, and at least the one run that synthesizes once no batch
// is left.
func reviewParallel(unclaimed int) int {
	return max(1, min(maxReviewRuns, (unclaimed+reviewersPerRun-1)/reviewersPerRun))
}

const (
	// reviewersPerRun is the review-loop's reviewers in one run (UILOOP_MAX_REVIEWERS).
	reviewersPerRun = 4
	maxReviewRuns   = 3
)

// CaptureState is the capture a `run` holds a live lease for, the newest
// pass first; all zero while none runs.
type CaptureState struct {
	Running bool   `json:"running"`
	Pass    int    `json:"pass"`
	Since   string `json:"since"`
	Owner   string `json:"owner"`
}

// StateConfig is the part of the section the workflow's briefs need.
type StateConfig struct {
	Dir         string    `json:"dir"`
	Out         string    `json:"out"`
	Spec        string    `json:"spec"`
	AppMap      string    `json:"appMap"`
	Areas       []string  `json:"areas"`
	Apps        []string  `json:"apps"`
	Project     string    `json:"project"`
	BoardPrefix string    `json:"boardPrefix"`
	Lint        StateLint `json:"lint"`
	// Source and Primitives are the effective pathspecs and prefixes state
	// judged drift and the verify selection with.
	Source     []string `json:"source"`
	Primitives []string `json:"primitives"`
}

// StateLint is the lint the pass's shots were linted with (its run.json, which
// the config does not stale), else the config's, defaults filled, so a
// reviewer brief quotes the values the shot records measured, not the spec's.
type StateLint struct {
	Grid        int `json:"grid"`
	TouchTarget int `json:"touchTarget"`
}

// AreaCount is one area's screens in the pass.
type AreaCount struct {
	Area    string `json:"area"`
	Screens int    `json:"screens"`
}

// StateSet is one area set of the publish index.
type StateSet struct {
	Area   string `json:"area"`
	Key    string `json:"key"`
	Status string `json:"status"`
	URL    string `json:"url,omitempty"`
}

// StateReview is the review stage's progress.
type StateReview struct {
	// BatchesFile is true when review/batches.json exists; otherwise the
	// batches are computed with DefaultBatchSize and not written.
	BatchesFile bool `json:"batchesFile"`
	Size        int  `json:"size"`
	// Planned, Done and Left count the batches a reviewer takes: a split
	// batch's parts, never the split batch. A blocked batch is neither done
	// nor left: it is in Blocked.
	Planned       int            `json:"planned"`
	Done          []string       `json:"done"`
	Left          []string       `json:"left"`
	Blocked       []BlockedBatch `json:"blocked"`
	ReviewedAreas []string       `json:"reviewedAreas"`
	// Stalls counts each batch's reviewer stalls (batches --stall), the
	// batches that had one only.
	Stalls map[string]int `json:"stalls"`
	// Carried counts the screens batches.json carries into this pass and
	// CarriedFrom names the pass they came from (null when none carry). A
	// carried screen retaken since is reopened (carriesNow): not counted, and
	// its area is not reviewed until batches plans the pass again.
	Carried     int  `json:"carried"`
	CarriedFrom *int `json:"carriedFrom"`
}

// BacklogCounts summarizes a backlog without its bodies.
type BacklogCounts struct {
	File       string         `json:"file"`
	Findings   int            `json:"findings"`
	Open       int            `json:"open"`
	ByStatus   map[string]int `json:"byStatus"`
	BySeverity map[string]int `json:"bySeverity"` // open findings only
	// Reviewed is len(reviewed); -1 when the backlog has no reviewed list.
	Reviewed int `json:"reviewed"`
	// Carried counts the items copied from an earlier pass (carriedFrom).
	// ByStatus leaves them out, as the scoreboard does: they are that pass's
	// verdicts, not this one's, so ByStatus plus Carried sums to Findings.
	Carried int `json:"carried"`
}

// PreviousBacklog is the newest earlier pass that has a backlog.
type PreviousBacklog struct {
	Pass int    `json:"pass"`
	File string `json:"file"`
	Open int    `json:"open"`
}

// CheckpointCounts summarizes the pass's checkpoints (loadCheckpoints).
type CheckpointCounts struct {
	Total    int            `json:"total"`
	ByStatus map[string]int `json:"byStatus"`
}

// BoardRow is one row of publish/boards.json (the review-loop's Boards agent writes it).
type BoardRow struct {
	Area    string `json:"area"`
	URL     string `json:"url"`
	Slug    string `json:"slug,omitempty"`
	Section string `json:"section,omitempty"`
}

// StateData is `ui-loop state`: a pass's state as counts, never item bodies.
type RecoveryLane struct {
	Lane string   `json:"lane"`
	Kind string   `json:"kind"`
	Dirs []string `json:"dirs"`
	Keys []string `json:"keys"`
}

// StateData carries only the interrupted writer's bounded ownership, never item bodies.
type StateData struct {
	// Vybava is this binary's version (`vybava --version`); Contract is StateContract.
	Vybava             string           `json:"vybava"`
	Contract           int              `json:"contract"`
	Recovery           *RecoveryLane    `json:"recovery,omitempty"`
	HeadSHA            string           `json:"headSha"`
	ReviewBasis        string           `json:"reviewBasis"`
	CapturedHeadSHA    string           `json:"capturedHeadSha"`
	Drift              *Drift           `json:"drift"`           // nil without provenance, or when this clone lacks the captured revision
	SourceUnchanged    bool             `json:"sourceUnchanged"` // drift.app is empty
	ScoreboardBasis    string           `json:"scoreboardBasis"`
	ScoreboardCurrent  bool             `json:"scoreboardCurrent"`
	CheckpointAPINotes []string         `json:"checkpointApiNotes"`
	Pass               int              `json:"pass"`
	PassDir            string           `json:"passDir"`
	Capture            CaptureState     `json:"capture"`
	Pending            *int             `json:"pending"` // a newer pass with no shots and no live capture; Pass is the newest with shots
	Config             StateConfig      `json:"config"`
	Shots              int              `json:"shots"`
	Screens            int              `json:"screens"`
	Areas              []AreaCount      `json:"areas"`
	Published          bool             `json:"published"`
	Unpublished        []string         `json:"unpublished"`
	Sets               []StateSet       `json:"sets"`
	Review             StateReview      `json:"review"`
	HasBacklog         bool             `json:"hasBacklog"`
	Backlog            *BacklogCounts   `json:"backlog"`
	Previous           *PreviousBacklog `json:"previous"`
	Checkpoints        CheckpointCounts `json:"checkpoints"`
	Boards             []BoardRow       `json:"boards"`
	Next               NextStage        `json:"next"`
}

// StateOptions are `state`'s flags.
type StateOptions struct {
	Pass int
	Cap  int // pass cap for the next stage (default 6)
	// Primitives are directory prefixes holding shared primitives (nil: the config's).
	Primitives []string
}

// screen is one screen of a pass: its id and area.
type screen struct{ id, area string }

// passScreens lists each screen of the records once, in record order.
func passScreens(records []Record) []screen {
	seen := map[string]bool{}
	var out []screen
	for _, r := range records {
		if !seen[r.ID] {
			seen[r.ID] = true
			out = append(out, screen{r.ID, r.Area})
		}
	}
	return out
}

// ComputeBatches is the deterministic reviewer batching: each area's screens
// sorted by id, in chunks of size, areas in config order (the rest by name).
// A batch id (<area>-<n>) is stable, so a resumed review skips the batches
// whose raw file exists. It counts screens, never image weight: a batch
// that stalls its reviewer is split instead (stall.go).
func ComputeBatches(screens []screen, areaOrder []string, size int) []Batch {
	if size < 1 {
		size = DefaultBatchSize
	}
	byArea := map[string][]string{}
	var areas []string
	for _, s := range screens {
		if _, ok := byArea[s.area]; !ok {
			areas = append(areas, s.area)
		}
		byArea[s.area] = append(byArea[s.area], s.id)
	}
	sortAreas(areaOrder, areas)
	out := []Batch{}
	for _, area := range areas {
		ids := byArea[area]
		sort.Strings(ids)
		for i := 0; i < len(ids); i += size {
			out = append(out, Batch{ID: fmt.Sprintf("%s-%d", area, i/size+1), Area: area, Screens: ids[i:min(i+size, len(ids))]})
		}
	}
	return out
}

func (t *Tool) reviewDir(pass int) string { return filepath.Join(t.passAbs(pass), "review") }

// readJSON decodes file into v; found is false when it does not exist.
func readJSON(file string, v any) (found bool, err error) {
	b, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return true, fmt.Errorf("%s: %w", file, err)
	}
	return true, nil
}

func writeJSON(file string, v any) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicJSONBytes(file, b)
}

// loadBatches reads review/batches.json; without it, it computes the batches
// with size (DefaultBatchSize when 0) and reports persisted=false.
func (t *Tool) loadBatches(pass int, records []Record, size int) (BatchesFile, bool, error) {
	var f BatchesFile
	found, err := readJSON(filepath.Join(t.reviewDir(pass), "batches.json"), &f)
	if err != nil {
		return f, false, err
	}
	if found {
		return f, true, nil
	}
	if size < 1 {
		size = DefaultBatchSize
	}
	return BatchesFile{V: 1, Pass: pass, Size: size, Batches: ComputeBatches(passScreens(records), t.Config.Areas, size)}, false, nil
}

// rawBatchIDs lists the batch ids the pass's raw reviews complete (rawBatchEvidence).
func (t *Tool) rawBatchIDs(pass int, known ...passSnapshot) (map[string]bool, error) {
	ev, err := t.rawBatchEvidence(pass, known...)
	return ev.complete, err
}

// rawEvidence is what a pass's review/raw/*.json prove.
type rawEvidence struct {
	// merged are the raw files merge-review takes, by name (.json trimmed).
	merged map[string]bool
	// complete are the batch ids the raws complete.
	complete map[string]bool
	// digests are the screens' current screenDigests; nil without provenance.
	digests map[string]string
}

// rawBatchEvidence judges review/raw/*.json. Without provenance (no
// capture.json) every raw file is merged and completes the batch of its name.
// With provenance a raw is judged by its shape:
//
//   - v1 (basis, screensRead) is judged whole: it is merged when it carries
//     the current basis and its own batch id and its screensRead stays inside
//     the batch, and it completes the batch when screensRead also names every
//     batch screen with an ok shot. A valid partial review is mergeable but
//     does not complete its batch.
//   - v2 (screens: {id: digest}, copied from its batch's digests) is judged
//     screen by screen: a screen counts as read when its digest is the
//     screen's current one, so a --resume retake reopens that screen alone
//     and the rest of the raw still counts. Every v2 raw is merged; merge-review
//     keeps only what it says of the screens it read. A batch is complete when
//     every screen in it with an ok shot was read by some v2 raw, whatever the
//     raws are named, so split parts and a hand-merged raw complete their
//     batch together. A batch without an ok shot needs a v2 raw naming it.
//
// Either way a split batch is complete exactly when its parts are (settle).
func (t *Tool) rawBatchEvidence(pass int, known ...passSnapshot) (rawEvidence, error) {
	ev := rawEvidence{merged: map[string]bool{}, complete: map[string]bool{}}
	paths, err := filepath.Glob(filepath.Join(t.reviewDir(pass), "raw", "*.json"))
	if err != nil {
		return ev, err
	}
	var marker captureEvidence
	strict, err := readJSON(filepath.Join(t.passAbs(pass), "capture.json"), &marker)
	if err != nil {
		return ev, err
	}
	if !strict {
		for _, p := range paths {
			name := strings.TrimSuffix(filepath.Base(p), ".json")
			ev.merged[name], ev.complete[name] = true, true
		}
		// Only a persisted batches.json can hold a split.
		var batches BatchesFile
		if _, err := readJSON(filepath.Join(t.reviewDir(pass), "batches.json"), &batches); err != nil {
			return ev, err
		}
		batches.settle(ev.complete)
		return ev, nil
	}
	snap, err := t.snapshot(pass, known)
	if err != nil {
		return ev, err
	}
	records, err := LoadRecords(t.passAbs(pass))
	if err != nil {
		return ev, err
	}
	if ev.digests, err = t.screenDigests(pass, records, snap.hashes); err != nil {
		return ev, err
	}
	batches, _, err := t.loadBatches(pass, records, 0)
	if err != nil {
		return ev, err
	}
	// shot names the screens with at least one ok record. A batch also holds
	// screens the pass could not shoot (unreachable, recipe-failed, error);
	// a reviewer can only list those as unreviewed, so they never hold a
	// batch open. A shot screen must be read. An unreviewed entry never
	// blocks: it is a capture or recipe defect the reviewer could not judge
	// from the shots, and re-reviewing the same shots cannot change it.
	// merge-review still keeps such a screen (a screen-level entry, or shot
	// entries naming every ok shot it has) out of reviewed and lists it as
	// unreviewed.
	shot := map[string]bool{}
	for _, r := range records {
		if r.Status == "ok" {
			shot[r.ID] = true
		}
	}
	read, named := map[string]bool{}, map[string]bool{}
	for _, p := range paths {
		name := strings.TrimSuffix(filepath.Base(p), ".json")
		var r rawReview
		if _, err := readJSON(p, &r); err != nil {
			return ev, err
		}
		if r.Screens != nil {
			ev.merged[name] = true
			named[cmp.Or(r.Batch, name)] = true
			maps.Copy(read, r.readNow(ev.digests))
			continue
		}
		i := slices.IndexFunc(batches.Batches, func(b Batch) bool { return b.ID == r.Batch })
		if r.Basis != snap.basis || r.Batch != name || i < 0 {
			continue
		}
		batch := batches.Batches[i]
		if slices.ContainsFunc(r.ScreensRead, func(s string) bool { return !slices.Contains(batch.Screens, s) }) {
			continue
		}
		ev.merged[name] = true
		if !slices.ContainsFunc(batch.Screens, func(s string) bool { return shot[s] && !slices.Contains(r.ScreensRead, s) }) {
			ev.complete[name] = true
		}
	}
	for _, b := range batches.Batches {
		covered := !slices.ContainsFunc(b.Screens, func(s string) bool { return shot[s] && !read[s] })
		if covered && (named[b.ID] || slices.ContainsFunc(b.Screens, func(s string) bool { return shot[s] })) {
			ev.complete[b.ID] = true
		}
	}
	batches.settle(ev.complete)
	return ev, nil
}

// archivedRound is a <pass>/fix/r<N>/ directory: an earlier fix round's
// checkpoints, moved aside when a later round rewrote their items. It never counts.
var archivedRound = regexp.MustCompile(`^r[0-9]+$`)

// checkpointFile is one candidate checkpoint: its slash-separated path under
// <pass>/fix/ and when it was last written.
type checkpointFile struct {
	rel     string
	modTime time.Time
}

// checkpointFiles walks <pass>/fix/ for checkpoint files. Writers name a
// checkpoint after its backlog key, so a key with "/" lands in a
// subdirectory; the walk descends into those, never into a top-level r<N>/
// round archive, and leaves out lanes.json and recovery.json.
func checkpointFiles(dir string) ([]checkpointFile, error) {
	var out []checkpointFile
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if archivedRound.MatchString(rel) {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".json") || rel == lanesFile || rel == "recovery.json" {
			return nil
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil // renamed away mid-walk
		}
		if err != nil {
			return err
		}
		out = append(out, checkpointFile{rel, info.ModTime()})
		return nil
	})
	return out, err
}

// loadCheckpoints reads the pass's checkpoint files (checkpointFiles), one
// per item: the item is the file's `key`, else its path under fix/ without
// .json. A file that does not decode (cut off mid-write) or has neither key
// nor status is skipped with a warning; in a pass with provenance a stale
// file (validCheckpoint) is not admitted. Only then are duplicates resolved:
// of two admitted files that claim one key, one counts (supersedes) and the
// other is warned, so a stale file can never shadow a current one.
func (t *Tool) loadCheckpoints(pass int, knownBasis ...string) ([]Checkpoint, []runxDiagnostic, error) {
	dir := filepath.Join(t.passAbs(pass), "fix")
	files, err := checkpointFiles(dir)
	if err != nil {
		return nil, nil, err
	}
	var marker captureEvidence
	strict, err := readJSON(filepath.Join(t.passAbs(pass), "capture.json"), &marker)
	if err != nil {
		return nil, nil, err
	}
	var basis string
	if strict {
		basis, err = t.cachedReviewBasis(pass, knownBasis)
		if err != nil {
			return nil, nil, err
		}
	}
	type admitted struct {
		file checkpointFile
		cp   Checkpoint
	}
	var diags []runxDiagnostic
	var keys []string
	counts := map[string]admitted{}
	shadowed := map[string][]string{}
	for _, f := range files {
		var c Checkpoint
		if _, err := readJSON(filepath.Join(dir, filepath.FromSlash(f.rel)), &c); err != nil || (c.Key == "" && c.Status == "") {
			diags = append(diags, warn(DiagCheckpointInvalid, t.PassDir(pass)+"/fix/"+f.rel+" is not a checkpoint ({key, status})",
				"rewrite it, or delete it so its item is fixed again"))
			continue
		}
		if c.Key == "" {
			c.Key = strings.TrimSuffix(f.rel, ".json")
		}
		if strict {
			valid, err := t.validCheckpoint(c, basis)
			if err != nil {
				return nil, nil, err
			}
			if !valid {
				continue
			}
		}
		prev, ok := counts[c.Key]
		switch {
		case !ok:
			keys = append(keys, c.Key)
		case supersedes(f, prev.file, c.Key):
			shadowed[c.Key] = append(shadowed[c.Key], prev.file.rel)
		default:
			shadowed[c.Key] = append(shadowed[c.Key], f.rel)
			continue
		}
		counts[c.Key] = admitted{f, c}
	}
	out := make([]Checkpoint, 0, len(keys))
	for _, key := range keys {
		won := counts[key]
		for _, rel := range shadowed[key] {
			diags = append(diags, warn(DiagCheckpointInvalid, fmt.Sprintf("%s/fix/%s and fix/%s both checkpoint %q; fix/%s counts", t.PassDir(pass), rel, won.file.rel, key, won.file.rel),
				"delete fix/"+rel+", or move it into a fix/r<N>/ round archive"))
		}
		out = append(out, won.cp)
	}
	return out, diags, nil
}

// supersedes reports whether checkpoint file a counts over b, both admitted
// for key: the writers' own path fix/<key>.json first, then the newer write,
// then the lower path so the choice never depends on walk order.
func supersedes(a, b checkpointFile, key string) bool {
	canonical := key + ".json"
	if (a.rel == canonical) != (b.rel == canonical) {
		return a.rel == canonical
	}
	if !a.modTime.Equal(b.modTime) {
		return a.modTime.After(b.modTime)
	}
	return a.rel < b.rel
}

// CheckpointsData is `ui-loop checkpoints`: the pass's checkpoints exactly as
// state and lanes count them (loadCheckpoints), one per item. A workflow reads
// a round's changed screens and i18n strings here, never by globbing
// fix/*.json, which misses a slash key's subdirectory and admits stale files.
type CheckpointsData struct {
	V           int          `json:"v"`
	Pass        int          `json:"pass"`
	PassDir     string       `json:"passDir"`
	Checkpoints []Checkpoint `json:"checkpoints"`
}

// CheckpointsOptions are `checkpoints`' flags.
type CheckpointsOptions struct {
	Pass int
}

// Checkpoints lists the checkpoints that count for the pass: `ui-loop checkpoints`.
func (t *Tool) Checkpoints(o CheckpointsOptions) (Result, error) {
	pass, err := t.resolveShotPass(o.Pass)
	if err != nil {
		return Result{}, err
	}
	cps, diags, err := t.loadCheckpoints(pass)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: CheckpointsData{V: 1, Pass: pass, PassDir: t.PassDir(pass), Checkpoints: cps}, Diagnostics: diags}, nil
}

// previousBacklog finds the newest pass before `pass` that has a backlog.
func (t *Tool) previousBacklog(pass int) (*Backlog, *PreviousBacklog, error) {
	passes, err := t.Passes()
	if err != nil {
		return nil, nil, err
	}
	for i := len(passes) - 1; i >= 0; i-- {
		if passes[i] >= pass {
			continue
		}
		file := filepath.Join(t.reviewDir(passes[i]), "backlog.json")
		if _, err := os.Stat(file); err != nil {
			continue
		}
		b, err := LoadBacklog(file)
		if err != nil {
			return nil, nil, err
		}
		open := 0
		for _, f := range b.Findings {
			if f.Open() {
				open++
			}
		}
		return b, &PreviousBacklog{Pass: passes[i], File: t.PassDir(passes[i]) + "/review/backlog.json", Open: open}, nil
	}
	return nil, nil, nil
}

// loadPassBacklog reads <pass>/review/backlog.json strictly; nil when absent.
func (t *Tool) loadPassBacklog(pass int, knownBasis ...string) (*Backlog, error) {
	file := filepath.Join(t.reviewDir(pass), "backlog.json")
	if _, err := os.Stat(file); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	b, err := LoadBacklog(file)
	if err != nil {
		return nil, err
	}
	if b.Pass != pass {
		return nil, diag(DiagBacklogInvalid, fmt.Sprintf("%s/review/backlog.json is the backlog of pass %d", t.PassDir(pass), b.Pass),
			fmt.Sprintf("its \"pass\" must be %d: re-run the review stage's synthesis", pass))
	}
	current, err := t.backlogEvidenceCurrent(pass, file, knownBasis...)
	if err != nil {
		return nil, err
	}
	if !current {
		return nil, nil
	}
	return b, nil
}

func countBacklog(file string, b *Backlog) *BacklogCounts {
	c := &BacklogCounts{File: file, ByStatus: map[string]int{}, BySeverity: map[string]int{}, Reviewed: -1}
	c.Findings = len(b.Findings)
	for _, f := range b.Findings {
		if f.CarriedFrom > 0 {
			c.Carried++
		} else {
			c.ByStatus[f.Status]++
		}
		if f.Open() {
			c.Open++
			c.BySeverity[f.Severity]++
		}
	}
	if b.Reviewed != nil {
		c.Reviewed = len(b.Reviewed)
	}
	return c
}

// publishedSets reads the publish index: one set per area, legacy rows ignored.
func (t *Tool) publishedSets(pass int, areas []string) (sets []StateSet, unpublished []string, err error) {
	var index PublishIndex
	found, err := readJSON(filepath.Join(t.passAbs(pass), "publish", "index.json"), &index)
	if err != nil {
		return nil, nil, err
	}
	sets = []StateSet{}
	byArea := map[string]StateSet{}
	if found {
		for _, s := range index.Sets {
			area := s.Area
			if area == "" {
				for _, a := range areas {
					if s.Key == areaKey(t.Config.Vitrinka.BoardPrefix, a) {
						area = a
					}
				}
			}
			row := StateSet{Area: area, Key: s.Key, Status: s.Status, URL: s.URL}
			sets = append(sets, row)
			byArea[area] = row
		}
	}
	unpublished = []string{}
	for _, a := range areas {
		if s, ok := byArea[a]; !ok || (s.Status != "pushed" && s.Status != "skipped") {
			unpublished = append(unpublished, a)
		}
	}
	return sets, unpublished, nil
}

// boardRows reads publish/boards.json of the pass, else of the newest earlier pass.
func (t *Tool) boardRows(pass int) ([]BoardRow, error) {
	passes, err := t.Passes()
	if err != nil {
		return nil, err
	}
	for i := len(passes) - 1; i >= 0; i-- {
		if passes[i] > pass {
			continue
		}
		var rows []BoardRow
		found, err := readJSON(filepath.Join(t.passAbs(passes[i]), "publish", "boards.json"), &rows)
		if err != nil {
			return nil, err
		}
		if found {
			return rows, nil
		}
	}
	return []BoardRow{}, nil
}

// State reads a pass back: `ui-loop state`.
func (t *Tool) State(o StateOptions) (Result, error) {
	c := t.Config
	primitives := o.Primitives
	if primitives == nil {
		primitives = append([]string{}, c.Primitives...)
	}
	cfg := StateConfig{Dir: c.Dir, Out: c.Out, Spec: c.Spec, AppMap: c.AppMap, Areas: c.Areas, Project: c.Vitrinka.Project, BoardPrefix: c.Vitrinka.BoardPrefix,
		Lint: StateLint{Grid: c.Lint.Grid, TouchTarget: c.Lint.TouchTarget}, Source: c.SourceOrDefault(), Primitives: primitives}
	for app := range c.Apps {
		cfg.Apps = append(cfg.Apps, app)
	}
	sort.Strings(cfg.Apps)
	data := StateData{Vybava: t.Version, Contract: StateContract, Config: cfg, Areas: []AreaCount{}, Unpublished: []string{}, Sets: []StateSet{}, Boards: []BoardRow{},
		Review:      StateReview{Done: []string{}, Left: []string{}, Blocked: []BlockedBatch{}, ReviewedAreas: []string{}, Stalls: map[string]int{}},
		Checkpoints: CheckpointCounts{ByStatus: map[string]int{}}}
	running, capture, err := t.liveCapture()
	if err != nil {
		return Result{}, err
	}
	if capture != nil {
		data.Capture = CaptureState{Running: true, Pass: running, Since: capture.StartedAt, Owner: capture.Owner}
	}
	// The default is the newest pass with shots (newestShotPass):
	// a newer, shot-less one is a capture still starting (Capture) or one
	// that never shot (Pending, which the next run reuses), and routing to
	// it sent every concurrent run to the one pass.
	pass := o.Pass
	if pass == 0 {
		var newest int
		if pass, newest, err = t.newestShotPass(); err != nil {
			return Result{}, err
		}
		if newest != pass && newest != data.Capture.Pass {
			data.Pending = &newest
		}
	}
	if pass == 0 {
		data.Next = NextStage{Stage: "capture", Reason: "no pass with shots yet"}
		return Result{Data: data, Next: []string{"vybava ui-loop run"}}, nil
	}
	data.Pass, data.PassDir = pass, t.PassDir(pass)
	head, headErr := t.git("rev-parse", "--verify", "HEAD")
	if headErr != nil {
		return Result{}, headErr
	}
	if head.Code == 0 {
		data.HeadSHA = strings.TrimSpace(head.Stdout)
	}
	var hashes map[string]string
	var evidenceDiags []runxDiagnostic
	data.ReviewBasis, hashes, evidenceDiags, err = t.reviewEvidence(pass)
	if err != nil {
		return Result{}, err
	}
	var marker captureEvidence
	if _, err := readJSON(filepath.Join(t.passAbs(pass), "capture.json"), &marker); err != nil {
		return Result{}, err
	}
	data.CapturedHeadSHA = marker.HeadSHA
	drift, provenance, err := t.drift(marker.HeadSHA)
	if err != nil {
		return Result{}, err
	}
	// unjudged is why the drift cannot be weighed; nextStage then reshoots.
	// Outside git no capture has provenance, so there is nothing to weigh.
	var unjudged string
	switch {
	case provenance:
		capped := drift.capped()
		data.Drift, data.SourceUnchanged = &capped, len(drift.App) == 0
	case marker.HeadSHA != "":
		unjudged = fmt.Sprintf("was captured at %s, which this clone lacks (%s)", marker.HeadSHA, DiagCaptureRevisionMissing)
	case data.HeadSHA != "":
		unjudged = "has no capture provenance"
	}
	if _, err := os.Stat(t.passAbs(pass)); err != nil {
		return Result{}, diag(DiagPassMissing, data.PassDir+" does not exist", "omit --pass for the latest pass")
	}
	var run struct {
		Lint StateLint `json:"lint"`
	}
	ran, err := readJSON(filepath.Join(t.passAbs(pass), "run.json"), &run)
	if err != nil {
		return Result{}, err
	}
	if ran {
		data.Config.Lint = run.Lint
	}
	records, err := LoadRecords(t.passAbs(pass))
	if err != nil {
		return Result{}, err
	}
	data.Shots = len(records)
	screens := passScreens(records)
	data.Screens = len(screens)
	perArea := map[string]int{}
	var areas []string
	for _, s := range screens {
		if perArea[s.area] == 0 {
			areas = append(areas, s.area)
		}
		perArea[s.area]++
	}
	sortAreas(c.Areas, areas)
	for _, a := range areas {
		data.Areas = append(data.Areas, AreaCount{Area: a, Screens: perArea[a]})
	}

	if data.Sets, data.Unpublished, err = t.publishedSets(pass, areas); err != nil {
		return Result{}, err
	}
	data.Published = len(records) > 0 && len(data.Unpublished) == 0
	if data.CapturedHeadSHA != "" && len(records) > 0 {
		plan, _, err := t.planRecords(pass, records, nil, hashes)
		if err != nil {
			return Result{}, err
		}
		var index PublishIndex
		if _, err := readJSON(filepath.Join(t.passAbs(pass), "publish", "index.json"), &index); err != nil {
			return Result{}, err
		}
		for _, set := range plan.Sets {
			matched := slices.ContainsFunc(index.Sets, func(row PublishedSet) bool {
				return row.Key == set.Key && row.Digest == setDigest(set) && row.URL != "" && len(row.Refused) == 0 && (row.Status == "pushed" || row.Status == "skipped")
			})
			data.Published = data.Published && matched
		}
	}

	batches, persisted, err := t.loadBatches(pass, records, 0)
	if err != nil {
		return Result{}, err
	}
	evidence, err := t.rawBatchEvidence(pass, passSnapshot{basis: data.ReviewBasis, hashes: hashes})
	if err != nil {
		return Result{}, err
	}
	leaves := batches.leaves()
	data.Review.BatchesFile, data.Review.Size, data.Review.Planned = persisted, batches.Size, len(leaves)
	if data.Review.Stalls, err = t.stalls(pass); err != nil {
		return Result{}, err
	}
	carried, reopened := carriesNow(batches.Carried, evidence.digests)
	if data.Review.Carried = len(carried); data.Review.Carried > 0 {
		data.Review.CarriedFrom = &carried[0].From
	}
	areaLeft := map[string]bool{}
	for _, s := range screens {
		areaLeft[s.area] = areaLeft[s.area] || slices.Contains(reopened, s.id)
	}
	// A blocked batch holds no area open: its unread screens go to the
	// backlog as unreviewed, so the synthesis comes.
	for _, b := range leaves {
		switch {
		case evidence.complete[b.ID]:
			data.Review.Done = append(data.Review.Done, b.ID)
		case b.Blocked:
			data.Review.Blocked = append(data.Review.Blocked, BlockedBatch{Batch: b.ID, Reason: b.BlockedReason})
		default:
			data.Review.Left = append(data.Review.Left, b.ID)
			areaLeft[b.Area] = true
		}
	}
	for _, a := range areas {
		if !areaLeft[a] {
			data.Review.ReviewedAreas = append(data.Review.ReviewedAreas, a)
		}
	}

	backlog, err := t.loadPassBacklog(pass, data.ReviewBasis)
	if err != nil {
		return Result{}, err
	}
	if backlog != nil {
		data.HasBacklog = true
		data.Backlog = countBacklog(data.PassDir+"/review/backlog.json", backlog)
	}
	if _, data.Previous, err = t.previousBacklog(pass); err != nil {
		return Result{}, err
	}
	checkpoints, diags, err := t.loadCheckpoints(pass, data.ReviewBasis)
	if err != nil {
		return Result{}, err
	}
	diags = append(evidenceDiags, diags...)
	data.Checkpoints.Total = len(checkpoints)
	for _, cp := range checkpoints {
		data.Checkpoints.ByStatus[cp.Status]++
	}
	if data.Boards, err = t.boardRows(pass); err != nil {
		return Result{}, err
	}
	var findings []Finding
	if backlog != nil {
		findings = backlog.Findings
	}
	data.ScoreboardBasis, err = t.scoreBasis(pass, data.ReviewBasis)
	if err != nil {
		return Result{}, err
	}
	var score struct {
		Basis  string     `json:"basis"`
		Posted []BoardRow `json:"posted"`
	}
	found, err := readJSON(filepath.Join(t.reviewDir(pass), "scoreboard-receipt.json"), &score)
	if err != nil {
		return Result{}, err
	}
	data.ScoreboardCurrent = found && score.Basis == data.ScoreboardBasis && len(data.Boards) > 0
	for _, area := range areas {
		data.ScoreboardCurrent = data.ScoreboardCurrent && slices.ContainsFunc(data.Boards, func(board BoardRow) bool { return board.Area == area && board.URL != "" })
	}
	for _, name := range []string{"scoreboard.json", "scoreboard.md"} {
		if _, err := os.Stat(filepath.Join(t.passAbs(pass), name)); errors.Is(err, fs.ErrNotExist) {
			data.ScoreboardCurrent = false
		} else if err != nil {
			return Result{}, err
		}
	}
	for _, board := range data.Boards {
		posted := slices.ContainsFunc(score.Posted, func(row BoardRow) bool { return row.Area == board.Area && row.URL == board.URL })
		data.ScoreboardCurrent = data.ScoreboardCurrent && posted
	}
	for _, cp := range checkpoints {
		data.CheckpointAPINotes = append(data.CheckpointAPINotes, cp.APIChanges...)
	}
	data.Next = nextStage(pass, records, data.Published, areas, data.Review.ReviewedAreas, backlog != nil, findings, checkpoints, drift.App, unjudged, primitives, o.Cap)
	// Batches a live run has claimed need no new run: counting them sent
	// each of K parallel runs on to launch K more.
	if data.Next.Stage == "review" {
		unclaimed, err := t.unclaimed(pass, data.Review.Left)
		if err != nil {
			return Result{}, err
		}
		data.Next.Parallel = reviewParallel(unclaimed)
	}
	if _, err := readJSON(filepath.Join(t.passAbs(pass), "fix", "recovery.json"), &data.Recovery); err != nil {
		return Result{}, err
	}
	if data.Recovery != nil {
		if data.Recovery.Lane == "" {
			return Result{}, diag(DiagCheckpointInvalid, "recovery.json has no writer identity", "restore the interrupted lane's identity and owned dirs before resuming")
		}
		data.Next = NextStage{Stage: "fix", Resume: true, Reason: "recover the interrupted writer before checkpoint filtering"}
	}
	if data.Recovery == nil && data.CapturedHeadSHA != "" && (data.Shots == 0 || data.Next.Stage == "capture" && data.Next.Resume) {
		// A resume is capture's own provenance check, which still counts the
		// rig: a pass it would refuse is shot again instead.
		resumable, err := t.sourceUnchanged(marker.HeadSHA)
		if err != nil {
			return Result{}, err
		}
		switch {
		case resumable && data.Shots == 0:
			data.Next = NextStage{Stage: "capture", Resume: true, Reason: "the interrupted pass has provenance but no shots yet"}
		case !resumable && data.Shots > 0:
			data.Next = NextStage{Stage: "capture", Only: []string{}, Reason: fmt.Sprintf("pass %d has shots that are not published, and its tree changed since capture, so it cannot resume: reshoot", pass)}
		}
	}
	// A running capture moves the pass under every stage, so nothing runs
	// beside it: the stages wait for it, whichever pass it shoots.
	if c := data.Capture; c.Running {
		data.Next = NextStage{Stage: "wait", Reason: fmt.Sprintf("pass %d capture running since %s (%s)", c.Pass, c.Since, c.Owner)}
	}
	return Result{Data: data, Diagnostics: diags}, nil
}

// nextStage is the one router; vitrinka's review-loop runs it verbatim:
// capture (no shots, or not published — then resume), capture again as a
// full reshoot when the app drifted before the review finished, review (no
// backlog, or an area with an unreviewed batch), fix (open items without a
// finishing checkpoint; drift is expected while fixing), done (nothing open
// and no app drift, the cap reached, or a round that fixed nothing), else
// verify (the round is checkpointed, or a converged pass's app drifted) with
// verifySelection's screens. appDrift is drift.app uncapped. unjudged, when
// set, is why the pass's drift cannot be weighed (it has no provenance, or
// this clone lacks its revision): the pass may have drifted, so it is reshot
// in full wherever app drift would reshoot it, an unpublished one included.
func nextStage(pass int, records []Record, published bool, areas, reviewedAreas []string, hasBacklog bool, findings []Finding, checkpoints []Checkpoint, appDrift []string, unjudged string, primitives []string, limit int) NextStage {
	if limit < 1 {
		limit = 6
	}
	if pass == 0 || len(records) == 0 {
		return NextStage{Stage: "capture", Reason: "no pass with shots yet"}
	}
	if !published && unjudged != "" {
		return NextStage{Stage: "capture", Only: []string{}, Reason: fmt.Sprintf("pass %d has shots that are not published, and it %s: reshoot", pass, unjudged)}
	}
	if !published {
		return NextStage{Stage: "capture", Resume: true, Reason: fmt.Sprintf("pass %d has shots that are not published", pass)}
	}
	var unreviewed []string
	for _, a := range areas {
		if !slices.Contains(reviewedAreas, a) {
			unreviewed = append(unreviewed, a)
		}
	}
	if !hasBacklog || len(unreviewed) > 0 {
		if len(appDrift) > 0 {
			return NextStage{Stage: "capture", Only: []string{}, Reason: fmt.Sprintf("application source changed since pass %d was captured (%s): reshoot before the review", pass, samplePaths(appDrift))}
		}
		if unjudged != "" {
			return NextStage{Stage: "capture", Only: []string{}, Reason: fmt.Sprintf("pass %d %s: reshoot before the review", pass, unjudged)}
		}
		reason := "no backlog yet"
		if len(unreviewed) > 0 {
			reason = "areas not reviewed: " + strings.Join(unreviewed, ", ")
		}
		return NextStage{Stage: "review", Resume: hasBacklog, Reason: reason}
	}
	finished := map[string]bool{}
	fixed := false
	for _, c := range checkpoints {
		if c.Finishes() {
			finished[c.Key] = true
		}
		fixed = fixed || c.Status == "done"
	}
	open, todo := 0, 0
	for _, f := range findings {
		if f.Open() {
			open++
			if !finished[f.Key] {
				todo++
			}
		}
	}
	switch {
	case todo > 0:
		return NextStage{Stage: "fix", Resume: len(finished) > 0, Reason: fmt.Sprintf("%d of %d open items without a checkpoint", todo, open)}
	case open == 0 && len(appDrift) == 0 && unjudged == "":
		return NextStage{Stage: "done", Reason: fmt.Sprintf("pass %d backlog has no open item — converged", pass)}
	case pass >= limit:
		return NextStage{Stage: "done", Reason: fmt.Sprintf("cap of %d passes reached with %d items open", limit, open)}
	case open > 0 && !fixed:
		return NextStage{Stage: "done", Reason: "the fix round fixed nothing — the open items need a human"}
	}
	reason := fmt.Sprintf("fix round of pass %d is checkpointed", pass)
	switch {
	case open > 0:
	case unjudged != "":
		reason = fmt.Sprintf("pass %d has no open item", pass)
	default:
		reason = fmt.Sprintf("pass %d converged, but application source changed since capture (%s)", pass, samplePaths(appDrift))
	}
	only, full := verifySelection(records, findings, checkpoints, appDrift, primitives)
	if unjudged != "" {
		only, full = []string{}, fmt.Sprintf("it %s → full reshoot", unjudged)
	}
	if full != "" {
		reason += "; " + full
	}
	return NextStage{Stage: "verify", Reason: reason, Only: only}
}

// verifySelection is the screens a verify reshoots, sorted: every open
// finding's, every done checkpoint's, and every screen whose shot records'
// sourceFiles meet the app drift (equal, or one a directory prefix of the
// other). An app change under a primitives prefix reshoots everything, and
// so does a selection that comes out empty: only is then empty and full
// says why.
func verifySelection(records []Record, findings []Finding, checkpoints []Checkpoint, appDrift, primitives []string) (only []string, full string) {
	for _, p := range appDrift {
		if underPrefix(p, primitives) {
			return []string{}, "primitive changed → full reshoot: " + p
		}
	}
	only = []string{}
	for _, f := range findings {
		if f.Open() && f.Screen != "" {
			only = append(only, f.Screen)
		}
	}
	for _, c := range checkpoints {
		if c.Status == "done" {
			only = append(only, c.Screens...)
		}
	}
	for _, r := range records {
		if slices.ContainsFunc(r.SourceFiles, func(src string) bool {
			return slices.ContainsFunc(appDrift, func(p string) bool {
				return underPrefix(path.Clean(src), []string{p}) || underPrefix(p, []string{src})
			})
		}) {
			only = append(only, r.ID)
		}
	}
	sort.Strings(only)
	if only = slices.Compact(only); len(only) == 0 {
		return only, "no screen's sourceFiles name the changed source → full reshoot"
	}
	return only, ""
}

// samplePaths names up to three paths, then how many more there are.
func samplePaths(paths []string) string {
	if len(paths) <= 3 {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(paths[:3], ", "), len(paths)-3)
}

// BatchesOptions are `batches`' flags.
type BatchesOptions struct {
	Pass  int
	Size  int // 0: the persisted size, else DefaultBatchSize
	Areas []string
	// Claim > 0 claims up to Claim left batches for Owner (a run id) as
	// batch-<id> leases held for TTL (0: DefaultClaimTTL); 0 claims nothing.
	Claim int
	Owner string
	TTL   time.Duration
	// Split, Stall and Block (at most one) act on that batch of the
	// persisted batches.json instead of planning (stall.go): Split halves it
	// (Claim then claims among its parts), Stall counts a reviewer stall of
	// it and Block takes it out of the review for Reason.
	Split, Stall, Block, Reason string
}

// BatchesData is `ui-loop batches`.
type BatchesData struct {
	Pass    int    `json:"pass"`
	PassDir string `json:"passDir"`
	File    string `json:"file"`
	Size    int    `json:"size"`
	Screens int    `json:"screens"` // screens across the batches returned
	// Batches are the batches a reviewer takes, a split batch's parts in its
	// place (batches.json keeps the split batch too); only --areas when
	// given. Done and Left are of these, a blocked batch in neither.
	Batches []Batch  `json:"batches"`
	Done    []string `json:"done"`
	Left    []string `json:"left"`
	// Carried are the screens no batch holds because they carry an earlier
	// pass's review (planCarry), sorted by screen, {screen, from} only (the
	// digest stays in batches.json); only --areas when given.
	Carried []Carried `json:"carried"`
	// Claimed (--claim only, else absent) are the left batches claimed for
	// --owner (the ones it already held first, then unclaimed or
	// stale-claimed ones), in batch order. A reviewer takes only these.
	Claimed *[]Batch `json:"claimed,omitempty"`
}

// Batches plans the review batches and persists them to review/batches.json.
// A review that started without v2 batches (a raw file exists and
// batches.json is v1 or missing) keeps v1 batches, because a carry would
// re-chunk the batches its v1 raws were judged against; any other is planned
// v2 (planBatches), every run, so a retake since the last plan is seen;
// either way the plan keeps the file's splits and blocks (keepSplits).
func (t *Tool) Batches(o BatchesOptions) (Result, error) {
	pass, err := t.resolveShotPass(o.Pass)
	if err != nil {
		return Result{}, err
	}
	records, err := LoadRecords(t.passAbs(pass))
	if err != nil {
		return Result{}, err
	}
	if len(records) == 0 {
		return Result{}, diag(DiagPassMissing, t.PassDir(pass)+" holds no shot records", "vybava ui-loop run")
	}
	for _, a := range o.Areas {
		if !slices.Contains(t.Config.Areas, a) {
			return Result{}, diag(DiagSelectionInvalid, "--areas names "+a+", which uiLoop.areas does not list", "vybava ui-loop batches")
		}
	}
	if o.Claim < 0 || o.Claim > 0 && o.Owner == "" {
		return Result{}, diag(DiagSelectionInvalid, fmt.Sprintf("--claim %d needs a positive count and --owner (the run the claims are for)", o.Claim),
			"vybava ui-loop batches --claim 4 --owner <run id> --json")
	}
	if err := o.actionProblem(pass); err != nil {
		return Result{}, err
	}
	switch {
	case o.Split != "":
		return t.splitBatch(pass, o)
	case o.Stall != "":
		return t.stallBatch(pass, o)
	case o.Block != "":
		return t.blockBatch(pass, o)
	}
	file := filepath.Join(t.reviewDir(pass), "batches.json")
	var prior BatchesFile
	found, err := readJSON(file, &prior)
	if err != nil {
		return Result{}, err
	}
	size := o.Size
	if size == 0 {
		size = DefaultBatchSize
		if found && prior.Size > 0 {
			size = prior.Size
		}
	}
	snap, err := t.snapshot(pass, nil)
	if err != nil {
		return Result{}, err
	}
	raw, err := t.rawBatchIDs(pass, snap)
	if err != nil {
		return Result{}, err
	}
	// Re-batching mid-review would change what a finished raw file covers.
	if found && prior.Size != size && len(raw) > 0 {
		return Result{}, diag(DiagSelectionInvalid, fmt.Sprintf("the review of %s started with --size %d and has raw batches; --size %d would redefine them", t.PassDir(pass), prior.Size, size),
			fmt.Sprintf("omit --size (or pass --size %d)", prior.Size))
	}
	started, err := filepath.Glob(filepath.Join(t.reviewDir(pass), "raw", "*.json"))
	if err != nil {
		return Result{}, err
	}
	all := BatchesFile{V: 1, Pass: pass, Size: size, Batches: ComputeBatches(passScreens(records), t.Config.Areas, size)}
	if len(started) == 0 || (found && prior.V >= 2) {
		if all, err = t.planBatches(pass, records, size, snap); err != nil {
			return Result{}, err
		}
	}
	// Under the lease mutex, against the file as it is now: a split or a
	// block written since the read above is kept, and the stalls of a batch
	// the plan redrew go with it.
	err = t.underLeases(pass, func() error {
		var now BatchesFile
		if _, err := readJSON(file, &now); err != nil {
			return err
		}
		var redrawn []string
		all, redrawn = all.keepSplits(now)
		if err := writeJSON(file, all); err != nil {
			return err
		}
		return t.clearStalls(pass, redrawn)
	})
	if err != nil {
		return Result{}, err
	}
	// Done and left are judged against the batches just written.
	if raw, err = t.rawBatchIDs(pass, snap); err != nil {
		return Result{}, err
	}
	data := BatchesData{Pass: pass, PassDir: t.PassDir(pass), File: t.PassDir(pass) + "/review/batches.json", Size: size, Batches: []Batch{}, Done: []string{}, Left: []string{}, Carried: []Carried{}}
	areaOf := map[string]string{}
	for _, s := range passScreens(records) {
		areaOf[s.id] = s.area
	}
	for _, c := range all.Carried {
		if len(o.Areas) == 0 || slices.Contains(o.Areas, areaOf[c.Screen]) {
			data.Carried = append(data.Carried, Carried{Screen: c.Screen, From: c.From})
		}
	}
	for _, b := range all.leaves() {
		if len(o.Areas) > 0 && !slices.Contains(o.Areas, b.Area) {
			continue
		}
		data.Batches = append(data.Batches, b)
		data.Screens += len(b.Screens)
		switch {
		case raw[b.ID]:
			data.Done = append(data.Done, b.ID)
		case !b.Blocked:
			data.Left = append(data.Left, b.ID)
		}
	}
	// Only a left batch is claimable: one complete by its raws' per-screen
	// digests needs no reviewer, whoever claimed it, and a blocked one gets none.
	if o.Claim > 0 {
		if o.TTL <= 0 {
			o.TTL = DefaultClaimTTL
		}
		ids, err := t.claimBatches(pass, data.Left, o.Claim, o.Owner, o.TTL)
		if err != nil {
			return Result{}, err
		}
		claimed := []Batch{}
		for _, b := range data.Batches {
			if slices.Contains(ids, b.ID) {
				claimed = append(claimed, b)
			}
		}
		data.Claimed = &claimed
	}
	return Result{Data: data}, nil
}

// ---- merge-review

// rawReview is one reviewer's <pass>/review/raw/<batch>.json: v1 carries the
// pass's basis and screensRead, v2 the screens it read with the digests its
// batch gave them (rawBatchEvidence).
type rawReview struct {
	Basis       string            `json:"basis"`
	ScreensRead []string          `json:"screensRead"`
	Screens     map[string]string `json:"screens"`
	Batch       string            `json:"batch"`
	Area        string            `json:"area"`
	Findings    []json.RawMessage `json:"findings"`
	Acceptance  []struct {
		Key     string `json:"key"`
		Verdict string `json:"verdict"`
	} `json:"acceptance"`
	Unreviewed []string `json:"unreviewed"`
}

// readNow is the screens a v2 raw read at their current digest.
func (r rawReview) readNow(digests map[string]string) map[string]bool {
	read := map[string]bool{}
	for id, d := range r.Screens {
		if d != "" && d == digests[id] {
			read[id] = true
		}
	}
	return read
}

// ReviewProblem is a raw finding merge-review could not take as is.
type ReviewProblem struct {
	Batch   string   `json:"batch"`
	Index   int      `json:"index"`
	Screen  string   `json:"screen,omitempty"`
	Title   string   `json:"title,omitempty"`
	Missing []string `json:"missing"`
}

// MergeReviewData is `ui-loop merge-review`.
type MergeReviewData struct {
	Pass       int              `json:"pass"`
	PassDir    string           `json:"passDir"`
	File       string           `json:"file"`
	Previous   *PreviousBacklog `json:"previous"`
	Raw        int              `json:"raw"`
	Left       []string         `json:"left"` // planned batches (parts, not a split batch) no raw completes, blocked ones aside
	Findings   int              `json:"findings"`
	Open       int              `json:"open"`
	ByStatus   map[string]int   `json:"byStatus"` // carried items aside (BacklogCounts)
	BySeverity map[string]int   `json:"bySeverity"`
	Carried    int              `json:"carried"` // items copied from an earlier pass
	Reviewed   int              `json:"reviewed"`
	Unreviewed []string         `json:"unreviewed"`
	// Unjudged are previous open items no reviewer gave a verdict; they
	// stand as not-met until an agent judges them.
	Unjudged []string `json:"unjudged"`
	// Problems are raw findings left out of the draft for an agent to repair.
	Problems []ReviewProblem `json:"problems"`
	// Invalid lists what the draft still breaks of the backlog contract.
	Invalid []string `json:"invalid"`
}

var (
	severityRank = map[string]int{"broken": 3, "needs-work": 2, "polish": 1}
	verdictRank  = map[string]int{"not-met": 3, "partly": 2, "met": 1}
	slugRe       = regexp.MustCompile(`(?i)[^a-z0-9]+`)
)

// BacklogKey is a fresh finding's key: its screen and title, so the same
// defect filed by two reviewers is one item.
func BacklogKey(screen, title string) string {
	slug := func(s string) string { return strings.ToLower(strings.Trim(slugRe.ReplaceAllString(s, "-"), "-")) }
	k := slug(screen) + "-" + slug(title)
	if len(k) > 80 {
		k = k[:80]
	}
	return strings.TrimRight(k, "-")
}

// unreviewedScreens reads a reviewer's unreviewed entries into the screens
// they take out of reviewed. An entry is a screen id or one shot
// (<id>@<viewport>.<theme>, or <id>@<viewport> for every theme), maybe
// followed by a parenthesised why. A screen-level entry skips its screen.
// Shot entries skip it only when they name every ok shot it has (okShots:
// screen id to its ok shot keys); otherwise it was judged at its other shots.
func unreviewedScreens(entries []string, okShots map[string][]string) map[string]bool {
	skipped, named := map[string]bool{}, map[string][]string{}
	for _, e := range entries {
		id := strings.TrimSpace(e)
		if i := strings.IndexAny(id, " (\t"); i >= 0 {
			id = id[:i]
		}
		screen, shot, isShot := strings.Cut(id, "@")
		switch {
		case screen == "":
		case isShot:
			named[screen] = append(named[screen], shot)
		default:
			skipped[screen] = true
		}
	}
	for screen, shots := range named {
		judgedElsewhere := slices.ContainsFunc(okShots[screen], func(key string) bool {
			_, vt, _ := strings.Cut(key, "@")
			return !slices.ContainsFunc(shots, func(s string) bool { return vt == s || strings.HasPrefix(vt, s+".") })
		})
		if !judgedElsewhere {
			skipped[screen] = true
		}
	}
	return skipped
}

func union(a, b []string) []string {
	out := slices.Clone(a)
	for _, v := range b {
		if !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// MergeReview folds the raw batches and the previous backlog into the draft.
// okShots maps a screen id to its ok shot keys (<id>@<viewport>.<theme>).
// digests are the screens' current digests (nil without provenance): a v2
// raw speaks only for the screens it read at their current digest, so its
// findings and verdicts on any other screen are left out. The screens
// batches carries from the previous pass take that pass's items on them
// as they stood, with carriedFrom, and count as reviewed; a carried screen
// retaken since (carriesNow) takes nothing and is unreviewed. A blocked
// batch's screens no raw read or listed are unreviewed, as
// "<id> (stalled: <reason>)" when they have an ok shot.
func MergeReview(pass int, previous *Backlog, raws []rawReview, batches BatchesFile, okShots map[string][]string, digests map[string]string) (Backlog, []string, []ReviewProblem, []string) {
	var order []string
	items := map[string]*Finding{}
	put := func(f Finding) {
		order = append(order, f.Key)
		items[f.Key] = &f
	}
	skips := make([]map[string]bool, len(raws))
	// reads are the screens each v2 raw read at their current digest; nil for v1.
	reads := make([]map[string]bool, len(raws))
	for i, r := range raws {
		skips[i] = unreviewedScreens(r.Unreviewed, okShots)
		if r.Screens != nil && digests != nil {
			reads[i] = r.readNow(digests)
		}
	}
	readBy := func(i int, screen string) bool {
		if reads[i] != nil {
			return reads[i][screen]
		}
		return slices.Contains(raws[i].ScreensRead, screen)
	}
	// carried is true for a screen that takes the previous pass's items and
	// false for one that cannot (retaken since, or carried from another pass).
	carried := map[string]bool{}
	current, reopened := carriesNow(batches.Carried, digests)
	for _, c := range current {
		carried[c.Screen] = previous != nil && c.From == previous.Pass
	}
	for _, id := range reopened {
		carried[id] = false
	}
	verdicts := map[string]string{}
	for i, r := range raws {
		for _, a := range r.Acceptance {
			if (r.Basis != "" || reads[i] != nil) && previous != nil {
				read := slices.ContainsFunc(previous.Findings, func(f Finding) bool {
					return f.Key == a.Key && readBy(i, f.Screen) && !skips[i][f.Screen]
				})
				if !read {
					continue
				}
			}
			if verdictRank[a.Verdict] > verdictRank[verdicts[a.Key]] {
				verdicts[a.Key] = a.Verdict
			}
		}
	}
	unjudged := []string{}
	if previous != nil {
		for _, p := range previous.Findings {
			if carried[p.Screen] {
				p.CarriedFrom = previous.Pass
				put(p)
				continue
			}
			if !p.Open() {
				continue
			}
			v := verdicts[p.Key]
			if v == "" {
				unjudged = append(unjudged, p.Key)
				v = "not-met"
			}
			p.Status = v
			put(p)
		}
	}
	problems := []ReviewProblem{}
	for ri, r := range raws {
		for i, msg := range r.Findings {
			var f Finding
			if err := json.Unmarshal(msg, &f); err != nil {
				problems = append(problems, ReviewProblem{Batch: r.Batch, Index: i, Missing: []string{"decodable finding: " + err.Error()}})
				continue
			}
			if reads[ri] != nil && f.Screen != "" && !reads[ri][f.Screen] {
				continue
			}
			if f.Area == "" {
				f.Area = r.Area
			}
			var missing []string
			for field, v := range map[string]string{"screen": f.Screen, "title": f.Title, "acceptance": f.Acceptance} {
				if strings.TrimSpace(v) == "" {
					missing = append(missing, field)
				}
			}
			if len(f.Files) == 0 {
				missing = append(missing, "files")
			}
			if severityRank[f.Severity] == 0 {
				missing = append(missing, "severity")
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				problems = append(problems, ReviewProblem{Batch: r.Batch, Index: i, Screen: f.Screen, Title: f.Title, Missing: missing})
				continue
			}
			if f.Key == "" {
				f.Key = BacklogKey(f.Screen, f.Title)
			}
			cur, ok := items[f.Key]
			if !ok {
				f.Status = "open"
				put(f)
				continue
			}
			// A previous item's key is its verdict, never a second finding.
			if cur.Status != "open" {
				continue
			}
			if severityRank[f.Severity] > severityRank[cur.Severity] {
				cur.Severity = f.Severity
			}
			cur.Viewports, cur.Themes = union(cur.Viewports, f.Viewports), union(cur.Themes, f.Themes)
			cur.Shots, cur.Files = union(cur.Shots, f.Shots), union(cur.Files, f.Files)
		}
	}
	b := Backlog{V: 1, Pass: pass, Findings: []Finding{}}
	for _, k := range order {
		f := items[k]
		if f.Files == nil {
			f.Files = []string{}
		}
		b.Findings = append(b.Findings, *f)
	}
	// reviewed: every v1 raw batch's screens (its batches.json definition, or
	// for an id batches.json does not know, the screens its findings and
	// verdicts name), every screen a v2 raw read at its current digest and
	// every carried screen, minus every screen a reviewer marked unreviewed.
	// A screen of a batch a v2 raw names that no raw read stays unreviewed.
	byID := map[string][]string{}
	for _, bt := range batches.Batches {
		byID[bt.ID] = bt.Screens
	}
	judged, skipped := map[string]bool{}, map[string]bool{}
	for id, fromPrevious := range carried {
		if fromPrevious {
			judged[id] = true
		} else {
			skipped[id] = true
		}
	}
	var named []string
	for i, r := range raws {
		if reads[i] != nil {
			maps.Copy(judged, reads[i])
			named = append(named, r.Batch)
			for id := range skips[i] {
				skipped[id] = true
			}
			continue
		}
		ids, known := byID[r.Batch]
		if !known {
			for _, msg := range r.Findings {
				var f struct {
					Screen string `json:"screen"`
				}
				if json.Unmarshal(msg, &f) == nil && f.Screen != "" {
					ids = append(ids, f.Screen)
				}
			}
		}
		for _, id := range ids {
			if r.Basis != "" && !slices.Contains(r.ScreensRead, id) {
				skipped[id] = true
				continue
			}
			judged[id] = true
		}
		for id := range skips[i] {
			skipped[id] = true
		}
	}
	for _, batch := range named {
		for _, id := range byID[batch] {
			if !judged[id] {
				skipped[id] = true
			}
		}
	}
	// A screen of a blocked batch that no raw judged, and no reviewer listed
	// unreviewed with a why of its own, stalled; one without an ok shot is
	// unreviewed by its capture, not the stall, so it keeps its plain id.
	stalled := map[string]string{}
	for _, bt := range batches.leaves() {
		for _, id := range bt.Screens {
			listed := slices.ContainsFunc(skips, func(s map[string]bool) bool { return s[id] })
			if bt.Blocked && !judged[id] && !listed {
				skipped[id] = true
				if len(okShots[id]) > 0 {
					stalled[id] = bt.BlockedReason
				}
			}
		}
	}
	b.Reviewed = []string{}
	for id := range judged {
		if !skipped[id] {
			b.Reviewed = append(b.Reviewed, id)
		}
	}
	sort.Strings(b.Reviewed)
	unreviewed := slices.Sorted(maps.Keys(skipped))
	for i, id := range unreviewed {
		if why, ok := stalled[id]; ok {
			unreviewed[i] = id + " (stalled: " + why + ")"
		}
	}
	return b, unjudged, problems, unreviewed
}

// MergeReviewOptions are `merge-review`'s flags.
type MergeReviewOptions struct {
	Pass int
	// Owner (a run id) holds the synth lease past this verb, for the
	// synthesis that follows, until TTL (0: DefaultLeaseTTL); without it the
	// lease is this process's and is released on exit.
	Owner string
	TTL   time.Duration
}

// MergeReview writes review/backlog.draft.json from the raw batches and the
// previous pass's backlog: `ui-loop merge-review`. It takes the pass's synth
// lease first, so of N identical review runs only one synthesizes.
func (t *Tool) MergeReview(o MergeReviewOptions) (_ Result, err error) {
	pass, err := t.resolveShotPass(o.Pass)
	if err != nil {
		return Result{}, err
	}
	if o.TTL <= 0 {
		o.TTL = DefaultLeaseTTL
	}
	req := leaseReq{owner: o.Owner, ttl: o.TTL}
	if o.Owner == "" {
		req = processLease("", "merge-review", o.TTL)
	}
	lease, held, err := t.acquireLease(pass, leaseSynth, req)
	if err != nil {
		return Result{}, err
	}
	if held != nil {
		return Result{}, leaseHeld(pass, held)
	}
	if req.pid != 0 {
		defer t.dropLease(pass, leaseSynth, lease, &err)
	}
	records, err := LoadRecords(t.passAbs(pass))
	if err != nil {
		return Result{}, err
	}
	batches, persisted, err := t.loadBatches(pass, records, 0)
	if err != nil {
		return Result{}, err
	}
	if !persisted {
		if err := writeJSON(filepath.Join(t.reviewDir(pass), "batches.json"), batches); err != nil {
			return Result{}, err
		}
	}
	paths, err := filepath.Glob(filepath.Join(t.reviewDir(pass), "raw", "*.json"))
	if err != nil {
		return Result{}, err
	}
	// A pass whose every screen carried has no batch, and one whose every
	// batch is blocked has none to review, so nothing to wait for.
	leaves := batches.leaves()
	if len(paths) == 0 && slices.ContainsFunc(leaves, func(b Batch) bool { return !b.Blocked }) {
		return Result{}, diag(DiagPassMissing, t.PassDir(pass)+"/review/raw holds no reviewer batch", "run the review stage first")
	}
	// Raw files in batch order, then by name for ids batches.json does not know.
	rank := map[string]int{}
	for i, b := range batches.Batches {
		rank[b.ID] = i + 1
	}
	id := func(p string) string { return strings.TrimSuffix(filepath.Base(p), ".json") }
	sort.SliceStable(paths, func(i, j int) bool {
		ri, rj := rank[id(paths[i])], rank[id(paths[j])]
		if (ri == 0) != (rj == 0) {
			return ri != 0
		}
		if ri != rj {
			return ri < rj
		}
		return paths[i] < paths[j]
	})
	evidence, err := t.rawBatchEvidence(pass)
	if err != nil {
		return Result{}, err
	}
	raws := make([]rawReview, 0, len(paths))
	for _, p := range paths {
		if !evidence.merged[id(p)] {
			continue
		}
		var r rawReview
		if _, err := readJSON(p, &r); err != nil {
			return Result{}, diag(DiagReviewInvalid, err.Error(), "re-run that review batch (delete its raw file)")
		}
		if r.Batch == "" {
			r.Batch = id(p)
		}
		raws = append(raws, r)
	}
	previous, prevRef, err := t.previousBacklog(pass)
	if err != nil {
		return Result{}, err
	}
	okShots := map[string][]string{}
	for _, r := range records {
		if r.Status == "ok" {
			okShots[r.ID] = append(okShots[r.ID], r.Key())
		}
	}
	backlog, unjudged, problems, unreviewed := MergeReview(pass, previous, raws, batches, okShots, evidence.digests)
	file := t.PassDir(pass) + "/review/backlog.draft.json"
	if err := writeJSON(filepath.Join(t.reviewDir(pass), "backlog.draft.json"), backlog); err != nil {
		return Result{}, err
	}
	counts := countBacklog(file, &backlog)
	data := MergeReviewData{Pass: pass, PassDir: t.PassDir(pass), File: file, Previous: prevRef, Raw: len(raws), Left: []string{},
		Findings: counts.Findings, Open: counts.Open, ByStatus: counts.ByStatus, BySeverity: counts.BySeverity, Carried: counts.Carried, Reviewed: len(backlog.Reviewed),
		Unreviewed: unreviewed, Unjudged: unjudged, Problems: problems, Invalid: backlog.Validate()}
	if data.Invalid == nil {
		data.Invalid = []string{}
	}
	var diags []runxDiagnostic
	for _, b := range leaves {
		if !evidence.complete[b.ID] && !b.Blocked {
			data.Left = append(data.Left, b.ID)
		}
	}
	if len(data.Left) > 0 {
		diags = append(diags, warn(DiagReviewIncomplete, fmt.Sprintf("%d of %d batches have no raw file: %s", len(data.Left), len(leaves), strings.Join(data.Left, ", ")),
			"finish the review stage before writing backlog.json"))
	}
	return Result{Data: data, Diagnostics: diags}, nil
}
