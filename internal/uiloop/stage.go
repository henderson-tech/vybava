package uiloop

// stage.go owns the pass state the vitrinka review-loop reads back between
// its stages: `state` (compact counts and the next stage), `batches` (the
// reviewer batches, persisted), `merge-review` (the backlog draft) and
// `lanes` (the fix lanes by ownership, in lanes.go). The workflow's agents
// relay these envelopes; they never enumerate screens, raw files or backlog
// items themselves, because a large list handed through an agent's return
// value gets dropped or summarized (pwf-ui pass 1).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
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
}

// BatchesFile is <pass>/review/batches.json: the one definition of the
// pass's review batches, shared by the review stage and merge-review.
type BatchesFile struct {
	V       int     `json:"v"`
	Pass    int     `json:"pass"`
	Size    int     `json:"size"`
	Batches []Batch `json:"batches"`
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
	Note        string            `json:"note,omitempty"`
}

// Finishes reports whether the checkpoint closes its item for this round.
func (c Checkpoint) Finishes() bool {
	return c.Status == "done" || c.Status == "skipped" || c.Status == "blocked"
}

// NextStage is where a pass stands (vitrinka workflows-src/lib/uiloop.js nextStage).
type NextStage struct {
	Stage  string `json:"stage"` // capture | review | fix | verify | done
	Resume bool   `json:"resume"`
	Reason string `json:"reason"`
}

// StateConfig is the part of the section the workflow's briefs need.
type StateConfig struct {
	Dir         string   `json:"dir"`
	Out         string   `json:"out"`
	Spec        string   `json:"spec"`
	AppMap      string   `json:"appMap"`
	Areas       []string `json:"areas"`
	Apps        []string `json:"apps"`
	Project     string   `json:"project"`
	BoardPrefix string   `json:"boardPrefix"`
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
	BatchesFile   bool     `json:"batchesFile"`
	Size          int      `json:"size"`
	Planned       int      `json:"planned"`
	Done          []string `json:"done"`
	Left          []string `json:"left"`
	ReviewedAreas []string `json:"reviewedAreas"`
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
	Recovery           *RecoveryLane    `json:"recovery,omitempty"`
	HeadSHA            string           `json:"headSha"`
	ReviewBasis        string           `json:"reviewBasis"`
	CapturedHeadSHA    string           `json:"capturedHeadSha"`
	SourceUnchanged    bool             `json:"sourceUnchanged"`
	ScoreboardBasis    string           `json:"scoreboardBasis"`
	ScoreboardCurrent  bool             `json:"scoreboardCurrent"`
	CheckpointAPINotes []string         `json:"checkpointApiNotes"`
	Pass               int              `json:"pass"`
	PassDir            string           `json:"passDir"`
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
// whose raw file exists.
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

// rawBatchIDs lists the batch ids whose review/raw/<id>.json completes the
// batch. With provenance (capture.json) that means the current basis, the
// file's own batch id, screensRead inside the batch and covering every batch
// screen with an ok shot.
func (t *Tool) rawBatchIDs(pass int, knownBasis ...string) (map[string]bool, error) {
	return t.rawBatchEvidence(pass, false, knownBasis...)
}

// A valid partial review is mergeable but does not complete its batch.
func (t *Tool) rawBatchEvidence(pass int, partial bool, knownBasis ...string) (map[string]bool, error) {
	paths, err := filepath.Glob(filepath.Join(t.reviewDir(pass), "raw", "*.json"))
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	var marker captureEvidence
	strict, err := readJSON(filepath.Join(t.passAbs(pass), "capture.json"), &marker)
	if err != nil {
		return nil, err
	}
	var basis string
	var batches BatchesFile
	// shot names the screens with at least one ok record. A batch also holds
	// screens the pass could not shoot (unreachable, recipe-failed, error);
	// a reviewer can only list those as unreviewed, so they never hold a
	// batch open. A shot screen must be in screensRead. An unreviewed entry
	// never blocks: it is a capture or recipe defect the reviewer could not
	// judge from the shots, and re-reviewing the same shots cannot change it.
	// merge-review still keeps such a screen (a screen-level entry, or shot
	// entries naming every ok shot it has) out of reviewed and lists it as
	// unreviewed.
	shot := map[string]bool{}
	if strict {
		basis, err = t.cachedReviewBasis(pass, knownBasis)
		if err != nil {
			return nil, err
		}
		records, err := LoadRecords(t.passAbs(pass))
		if err != nil {
			return nil, err
		}
		for _, r := range records {
			if r.Status == "ok" {
				shot[r.ID] = true
			}
		}
		batches, _, err = t.loadBatches(pass, records, 0)
		if err != nil {
			return nil, err
		}
	}
	for _, p := range paths {
		if strict {
			var r rawReview
			if _, err := readJSON(p, &r); err != nil {
				return nil, err
			}
			if r.Basis != basis || r.Batch != strings.TrimSuffix(filepath.Base(p), ".json") {
				continue
			}
			valid := false
			for _, batch := range batches.Batches {
				if batch.ID != r.Batch {
					continue
				}
				valid = true
				for _, screen := range batch.Screens {
					if !partial && shot[screen] && !slices.Contains(r.ScreensRead, screen) {
						valid = false
					}
				}
				for _, screen := range r.ScreensRead {
					if !slices.Contains(batch.Screens, screen) {
						valid = false
					}
				}
			}
			if !valid {
				continue
			}
		}
		ids[strings.TrimSuffix(filepath.Base(p), ".json")] = true
	}
	return ids, nil
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
// nor status is skipped with a warning; of two files that claim one key, the
// newer counts.
func (t *Tool) loadCheckpoints(pass int, knownBasis ...string) ([]Checkpoint, []runxDiagnostic, error) {
	dir := filepath.Join(t.passAbs(pass), "fix")
	files, err := checkpointFiles(dir)
	if err != nil {
		return nil, nil, err
	}
	var diags []runxDiagnostic
	var keys []string
	claims := map[string]checkpointFile{}
	byKey := map[string]Checkpoint{}
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
		if prev, ok := claims[c.Key]; ok {
			older, newer := prev, f
			if f.modTime.Before(prev.modTime) {
				older, newer = f, prev
			}
			diags = append(diags, warn(DiagCheckpointInvalid, fmt.Sprintf("%s/fix/%s and fix/%s both checkpoint %q; the newer fix/%s counts", t.PassDir(pass), older.rel, newer.rel, c.Key, newer.rel),
				"delete fix/"+older.rel+", or move it into a fix/r<N>/ round archive"))
			if newer.rel == prev.rel {
				continue
			}
		} else {
			keys = append(keys, c.Key)
		}
		claims[c.Key], byKey[c.Key] = f, c
	}
	var out []Checkpoint
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
	for _, key := range keys {
		c := byKey[key]
		if strict {
			valid, err := t.validCheckpoint(c, basis)
			if err != nil {
				return nil, nil, err
			}
			if !valid {
				continue
			}
		}
		out = append(out, c)
	}
	return out, diags, nil
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
		c.ByStatus[f.Status]++
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
	cfg := StateConfig{Dir: c.Dir, Out: c.Out, Spec: c.Spec, AppMap: c.AppMap, Areas: c.Areas, Project: c.Vitrinka.Project, BoardPrefix: c.Vitrinka.BoardPrefix}
	for app := range c.Apps {
		cfg.Apps = append(cfg.Apps, app)
	}
	sort.Strings(cfg.Apps)
	data := StateData{Config: cfg, Areas: []AreaCount{}, Unpublished: []string{}, Sets: []StateSet{}, Boards: []BoardRow{},
		Review: StateReview{Done: []string{}, Left: []string{}, ReviewedAreas: []string{}}, Checkpoints: CheckpointCounts{ByStatus: map[string]int{}}}
	pass := o.Pass
	if pass == 0 {
		passes, err := t.Passes()
		if err != nil {
			return Result{}, err
		}
		if len(passes) > 0 {
			pass = passes[len(passes)-1]
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
	var err error
	var hashes map[string]string
	data.ReviewBasis, hashes, err = t.reviewEvidence(pass)
	if err != nil {
		return Result{}, err
	}
	var marker captureEvidence
	if _, err := readJSON(filepath.Join(t.passAbs(pass), "capture.json"), &marker); err != nil {
		return Result{}, err
	}
	data.CapturedHeadSHA = marker.HeadSHA
	data.SourceUnchanged, err = t.sourceUnchanged(marker.HeadSHA)
	if err != nil {
		return Result{}, err
	}
	if _, err := os.Stat(t.passAbs(pass)); err != nil {
		return Result{}, diag(DiagPassMissing, data.PassDir+" does not exist", "omit --pass for the latest pass")
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
	raw, err := t.rawBatchIDs(pass, data.ReviewBasis)
	if err != nil {
		return Result{}, err
	}
	data.Review.BatchesFile, data.Review.Size, data.Review.Planned = persisted, batches.Size, len(batches.Batches)
	areaLeft := map[string]bool{}
	for _, b := range batches.Batches {
		if raw[b.ID] {
			data.Review.Done = append(data.Review.Done, b.ID)
		} else {
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
	data.Next = nextStage(pass, len(records), data.Published, areas, data.Review.ReviewedAreas, backlog != nil, findings, checkpoints, o.Cap)
	if _, err := readJSON(filepath.Join(t.passAbs(pass), "fix", "recovery.json"), &data.Recovery); err != nil {
		return Result{}, err
	}
	if data.Recovery != nil {
		if data.Recovery.Lane == "" {
			return Result{}, diag(DiagCheckpointInvalid, "recovery.json has no writer identity", "restore the interrupted lane's identity and owned dirs before resuming")
		}
		data.Next = NextStage{Stage: "fix", Resume: true, Reason: "recover the interrupted writer before checkpoint filtering"}
	}
	if data.Recovery == nil && data.Shots == 0 && data.CapturedHeadSHA != "" && data.SourceUnchanged {
		data.Next = NextStage{Stage: "capture", Resume: true, Reason: "the interrupted pass has provenance but no shots yet"}
	}
	return Result{Data: data, Diagnostics: diags}, nil
}

// nextStage is vitrinka's nextStage (workflows-src/lib/uiloop.js), ported:
// capture (no shots, or not published — then resume), review (no backlog,
// or an area with an unreviewed batch), fix (open items without a finishing
// checkpoint), verify (the round is checkpointed and fixed something), done
// (nothing open, the cap reached, or a round that fixed nothing).
func nextStage(pass, shots int, published bool, areas, reviewedAreas []string, hasBacklog bool, findings []Finding, checkpoints []Checkpoint, limit int) NextStage {
	if limit < 1 {
		limit = 6
	}
	if pass == 0 || shots == 0 {
		return NextStage{Stage: "capture", Reason: "no pass with shots yet"}
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
	case open == 0:
		return NextStage{Stage: "done", Reason: fmt.Sprintf("pass %d backlog has no open item — converged", pass)}
	case todo > 0:
		return NextStage{Stage: "fix", Resume: len(finished) > 0, Reason: fmt.Sprintf("%d of %d open items without a checkpoint", todo, open)}
	case pass >= limit:
		return NextStage{Stage: "done", Reason: fmt.Sprintf("cap of %d passes reached with %d items open", limit, open)}
	case !fixed:
		return NextStage{Stage: "done", Reason: "the fix round fixed nothing — the open items need a human"}
	}
	return NextStage{Stage: "verify", Reason: fmt.Sprintf("fix round of pass %d is checkpointed", pass)}
}

// BatchesOptions are `batches`' flags.
type BatchesOptions struct {
	Pass  int
	Size  int // 0: the persisted size, else DefaultBatchSize
	Areas []string
}

// BatchesData is `ui-loop batches`.
type BatchesData struct {
	Pass    int      `json:"pass"`
	PassDir string   `json:"passDir"`
	File    string   `json:"file"`
	Size    int      `json:"size"`
	Screens int      `json:"screens"` // screens across the batches returned
	Batches []Batch  `json:"batches"` // only --areas when given
	Done    []string `json:"done"`
	Left    []string `json:"left"`
}

// Batches plans the review batches and persists them to review/batches.json.
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
	raw, err := t.rawBatchIDs(pass)
	if err != nil {
		return Result{}, err
	}
	// Re-batching mid-review would change what a finished raw file covers.
	if found && prior.Size != size && len(raw) > 0 {
		return Result{}, diag(DiagSelectionInvalid, fmt.Sprintf("the review of %s started with --size %d and has raw batches; --size %d would redefine them", t.PassDir(pass), prior.Size, size),
			fmt.Sprintf("omit --size (or pass --size %d)", prior.Size))
	}
	all := BatchesFile{V: 1, Pass: pass, Size: size, Batches: ComputeBatches(passScreens(records), t.Config.Areas, size)}
	if err := writeJSON(file, all); err != nil {
		return Result{}, err
	}
	data := BatchesData{Pass: pass, PassDir: t.PassDir(pass), File: t.PassDir(pass) + "/review/batches.json", Size: size, Batches: []Batch{}, Done: []string{}, Left: []string{}}
	for _, b := range all.Batches {
		if len(o.Areas) > 0 && !slices.Contains(o.Areas, b.Area) {
			continue
		}
		data.Batches = append(data.Batches, b)
		data.Screens += len(b.Screens)
		if raw[b.ID] {
			data.Done = append(data.Done, b.ID)
		} else {
			data.Left = append(data.Left, b.ID)
		}
	}
	return Result{Data: data}, nil
}

// ---- merge-review

// rawReview is one reviewer's <pass>/review/raw/<batch>.json.
type rawReview struct {
	Basis       string            `json:"basis"`
	ScreensRead []string          `json:"screensRead"`
	Batch       string            `json:"batch"`
	Area        string            `json:"area"`
	Findings    []json.RawMessage `json:"findings"`
	Acceptance  []struct {
		Key     string `json:"key"`
		Verdict string `json:"verdict"`
	} `json:"acceptance"`
	Unreviewed []string `json:"unreviewed"`
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
	Left       []string         `json:"left"` // planned batches without a raw file
	Findings   int              `json:"findings"`
	Open       int              `json:"open"`
	ByStatus   map[string]int   `json:"byStatus"`
	BySeverity map[string]int   `json:"bySeverity"`
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
func MergeReview(pass int, previous *Backlog, raws []rawReview, batches []Batch, okShots map[string][]string) (Backlog, []string, []ReviewProblem, []string) {
	var order []string
	items := map[string]*Finding{}
	put := func(f Finding) {
		order = append(order, f.Key)
		items[f.Key] = &f
	}
	skips := make([]map[string]bool, len(raws))
	for i, r := range raws {
		skips[i] = unreviewedScreens(r.Unreviewed, okShots)
	}
	verdicts := map[string]string{}
	for i, r := range raws {
		for _, a := range r.Acceptance {
			if r.Basis != "" && previous != nil {
				read := slices.ContainsFunc(previous.Findings, func(f Finding) bool {
					return f.Key == a.Key && slices.Contains(r.ScreensRead, f.Screen) && !skips[i][f.Screen]
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
	for _, r := range raws {
		for i, msg := range r.Findings {
			var f Finding
			if err := json.Unmarshal(msg, &f); err != nil {
				problems = append(problems, ReviewProblem{Batch: r.Batch, Index: i, Missing: []string{"decodable finding: " + err.Error()}})
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
	// reviewed: every raw batch's screens (its batches.json definition, or
	// for an id batches.json does not know, the screens its findings and
	// verdicts name) minus every screen a reviewer marked unreviewed.
	byID := map[string][]string{}
	for _, bt := range batches {
		byID[bt.ID] = bt.Screens
	}
	judged, skipped := map[string]bool{}, map[string]bool{}
	for i, r := range raws {
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
	b.Reviewed = []string{}
	for id := range judged {
		if !skipped[id] {
			b.Reviewed = append(b.Reviewed, id)
		}
	}
	sort.Strings(b.Reviewed)
	unreviewed := []string{}
	for id := range skipped {
		unreviewed = append(unreviewed, id)
	}
	sort.Strings(unreviewed)
	return b, unjudged, problems, unreviewed
}

// MergeReviewOptions are `merge-review`'s flags.
type MergeReviewOptions struct{ Pass int }

// MergeReview writes review/backlog.draft.json from the raw batches and the
// previous pass's backlog: `ui-loop merge-review`.
func (t *Tool) MergeReview(o MergeReviewOptions) (Result, error) {
	pass, err := t.resolveShotPass(o.Pass)
	if err != nil {
		return Result{}, err
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
	if len(paths) == 0 {
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
	validRaw, err := t.rawBatchEvidence(pass, true)
	if err != nil {
		return Result{}, err
	}
	completeRaw, err := t.rawBatchIDs(pass)
	if err != nil {
		return Result{}, err
	}
	raws := make([]rawReview, 0, len(paths))
	for _, p := range paths {
		if !validRaw[id(p)] {
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
	backlog, unjudged, problems, unreviewed := MergeReview(pass, previous, raws, batches.Batches, okShots)
	file := t.PassDir(pass) + "/review/backlog.draft.json"
	if err := writeJSON(filepath.Join(t.reviewDir(pass), "backlog.draft.json"), backlog); err != nil {
		return Result{}, err
	}
	counts := countBacklog(file, &backlog)
	data := MergeReviewData{Pass: pass, PassDir: t.PassDir(pass), File: file, Previous: prevRef, Raw: len(raws), Left: []string{},
		Findings: counts.Findings, Open: counts.Open, ByStatus: counts.ByStatus, BySeverity: counts.BySeverity, Reviewed: len(backlog.Reviewed),
		Unreviewed: unreviewed, Unjudged: unjudged, Problems: problems, Invalid: backlog.Validate()}
	if data.Invalid == nil {
		data.Invalid = []string{}
	}
	have := map[string]bool{}
	for _, r := range raws {
		have[r.Batch] = completeRaw[r.Batch]
	}
	var diags []runxDiagnostic
	for _, b := range batches.Batches {
		if !have[b.ID] {
			data.Left = append(data.Left, b.ID)
		}
	}
	if len(data.Left) > 0 {
		diags = append(diags, warn(DiagReviewIncomplete, fmt.Sprintf("%d of %d batches have no raw file: %s", len(data.Left), len(batches.Batches), strings.Join(data.Left, ", ")),
			"finish the review stage before writing backlog.json"))
	}
	return Result{Data: data, Diagnostics: diags}, nil
}
