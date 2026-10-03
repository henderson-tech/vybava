package uiloop

// stall.go splits a review batch on a stall instead of looping on it: the
// review-loop counts a reviewer's watchdog stall (`batches --stall`), halves
// a batch that stalled twice (`batches --split`) and blocks a part that
// stalled twice (`batches --block`), whose unread screens merge-review then
// lists as unreviewed, so the synthesis always comes. Image weight does not
// predict a stall (pwf-ui pass 4: portal-overview-3, 154 PNGs and 1,245 MP,
// was reviewed while portal-shell-2, 77 and 297 MP, stalled), so
// ComputeBatches stays screen-count based and nothing re-batches by weight.
//
// A split is recorded in batches.json: the batch keeps its entry and names
// its parts, which follow it in the list (a part split again is followed by
// its own parts). The batch is reviewed only through its parts (leaves), is
// complete exactly when they all are (settle), and a re-plan keeps both
// (keepSplits).

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Attempts is review/attempts/<batch>.json: the reviewer stalls a batch took
// (batches --stall), with when each was counted (RFC3339) in At.
type Attempts struct {
	Stalls int      `json:"stalls"`
	At     []string `json:"at"`
}

// BlockedBatch is a batch batches --block took out of the review.
type BlockedBatch struct {
	Batch  string `json:"batch"`
	Reason string `json:"reason"`
}

// SplitData is `batches --split`: the batch's parts in order, and with
// --claim the ones claimed for --owner.
type SplitData struct {
	Pass    int      `json:"pass"`
	PassDir string   `json:"passDir"`
	File    string   `json:"file"`
	Batch   string   `json:"batch"`
	Parts   []Batch  `json:"parts"`
	Claimed *[]Batch `json:"claimed,omitempty"`
}

// StallData is `batches --stall`: the batch's stalls so far, this one included.
type StallData struct {
	Batch  string `json:"batch"`
	Stalls int    `json:"stalls"`
}

// leaves are the batches a reviewer takes, in order: every batch not split
// into parts.
func (f BatchesFile) leaves() []Batch {
	return slices.DeleteFunc(slices.Clone(f.Batches), func(b Batch) bool { return len(b.Parts) > 0 })
}

func (f BatchesFile) index(id string) int {
	return slices.IndexFunc(f.Batches, func(b Batch) bool { return b.ID == id })
}

// halves splits b in batch order: the first ceil(n/2) screens are <id>.1,
// the rest <id>.2, each with the digests of its own screens.
func (b Batch) halves() []Batch {
	n := (len(b.Screens) + 1) / 2
	return []Batch{b.part(b.ID+".1", b.Screens[:n]), b.part(b.ID+".2", b.Screens[n:])}
}

// part is a part of b holding screens, with b's digests of them (none for a v1 batch).
func (b Batch) part(id string, screens []string) Batch {
	p := Batch{ID: id, Area: b.Area, Screens: slices.Clone(screens)}
	if b.Digests != nil {
		p.Digests = map[string]string{}
		for _, s := range screens {
			p.Digests[s] = b.Digests[s]
		}
	}
	return p
}

// keepSplits is f, a fresh plan, with prior's splits and blocks: a batch
// planned again with its prior screens keeps its block and its parts, which
// follow it with their screens frozen and the plan's digests. A batch whose
// screens moved (a carry gained or lost re-chunks its area) is a new batch,
// neither split nor blocked, because it holds screens the old one did not;
// the raws of its old parts still count screen by screen. redrawn are the
// prior batches f no longer holds with their screens (such a batch and its
// old parts): their stalls were another batch's (clearStalls).
func (f BatchesFile) keepSplits(prior BatchesFile) (_ BatchesFile, redrawn []string) {
	byID := map[string]Batch{}
	for _, b := range prior.Batches {
		byID[b.ID] = b
	}
	out := make([]Batch, 0, len(f.Batches))
	var keep func(b, planned Batch)
	keep = func(b, planned Batch) {
		was := byID[b.ID]
		b.Parts, b.Blocked, b.BlockedReason = was.Parts, was.Blocked, was.BlockedReason
		out = append(out, b)
		for _, id := range was.Parts {
			if p, ok := byID[id]; ok {
				keep(planned.part(id, p.Screens), planned)
			}
		}
	}
	for _, b := range f.Batches {
		if was, ok := byID[b.ID]; ok && slices.Equal(was.Screens, b.Screens) {
			keep(b, b)
		} else {
			out = append(out, b)
		}
	}
	f.Batches = out
	for _, b := range prior.Batches {
		if i := f.index(b.ID); i < 0 || !slices.Equal(f.Batches[i].Screens, b.Screens) {
			redrawn = append(redrawn, b.ID)
		}
	}
	return f, redrawn
}

// settle judges the split batches by their parts: a part is complete when
// its batch is by its own raws (a raw of the whole batch read the part's
// screens too), and a split batch exactly when all its parts are. Parts
// follow their batch, so one pass down and one up settle nested splits.
func (f BatchesFile) settle(complete map[string]bool) {
	for _, b := range f.Batches {
		if complete[b.ID] {
			for _, id := range b.Parts {
				complete[id] = true
			}
		}
	}
	for i := len(f.Batches) - 1; i >= 0; i-- {
		if b := f.Batches[i]; len(b.Parts) > 0 {
			complete[b.ID] = !slices.ContainsFunc(b.Parts, func(id string) bool { return !complete[id] })
		}
	}
}

// leavesUnder are the leaves of id's split, in order: id itself when it was
// never split.
func (f BatchesFile) leavesUnder(id string) []Batch {
	i := f.index(id)
	if i < 0 {
		return nil
	}
	if len(f.Batches[i].Parts) == 0 {
		return []Batch{f.Batches[i]}
	}
	var out []Batch
	for _, p := range f.Batches[i].Parts {
		out = append(out, f.leavesUnder(p)...)
	}
	return out
}

// actionProblem refuses a --split, --stall or --block the flags cannot mean:
// two at once, --block without --reason (or --reason without it), or
// --claim beside anything but --split.
func (o BatchesOptions) actionProblem(pass int) error {
	acts := slices.DeleteFunc([]string{o.Split, o.Stall, o.Block}, func(s string) bool { return s == "" })
	switch {
	case len(acts) > 1:
		return diag(DiagSelectionInvalid, "--split, --stall and --block each act on one batch: pass one of them",
			fmt.Sprintf("vybava ui-loop batches --pass %d --stall <id> --json", pass))
	case o.Block != "" && strings.TrimSpace(o.Reason) == "":
		return diag(DiagSelectionInvalid, "--block "+o.Block+" needs --reason, the why its screens are listed unreviewed with",
			fmt.Sprintf("vybava ui-loop batches --pass %d --block %s --reason \"reviewer stalled twice\" --json", pass, o.Block))
	case o.Reason != "" && o.Block == "":
		return diag(DiagSelectionInvalid, "--reason goes with --block <id>", fmt.Sprintf("vybava ui-loop batches --pass %d --block <id> --reason %q --json", pass, o.Reason))
	case o.Claim > 0 && (o.Stall != "" || o.Block != ""):
		return diag(DiagSelectionInvalid, "--claim plans or splits; --stall and --block claim nothing",
			fmt.Sprintf("vybava ui-loop batches --pass %d --claim %d --owner %s --json", pass, o.Claim, o.Owner))
	}
	return nil
}

// noBatch refuses an id batches.json does not hold.
func (t *Tool) noBatch(pass int, flag, id string) error {
	return diag(DiagSelectionInvalid, fmt.Sprintf("%s/review/batches.json holds no batch %s", t.PassDir(pass), id),
		fmt.Sprintf("vybava ui-loop batches --pass %d --json (lists the batch ids), then --%s <id>", pass, flag))
}

// editBatches rewrites review/batches.json through edit under the pass's
// lease mutex, which Batches writes a re-plan under too, so a split or a
// block never races a re-plan that would drop it. edit reports whether it
// changed the file; a refusal is its error. A pass without batches.json is
// refused before the mutex is taken: --flag has no batch to act on yet.
func (t *Tool) editBatches(pass int, flag string, edit func(*BatchesFile) (bool, error)) error {
	file := filepath.Join(t.reviewDir(pass), "batches.json")
	if _, err := os.Stat(file); errors.Is(err, fs.ErrNotExist) {
		return diag(DiagPassMissing, t.PassDir(pass)+"/review/batches.json does not exist: no batch is planned yet",
			fmt.Sprintf("vybava ui-loop batches --pass %d --json (plans them), then --%s <id>", pass, flag))
	} else if err != nil {
		return err
	}
	return t.underLeases(pass, func() error {
		var f BatchesFile
		if _, err := readJSON(file, &f); err != nil {
			return err
		}
		changed, err := edit(&f)
		if err != nil || !changed {
			return err
		}
		return writeJSON(file, f)
	})
}

// dropClaim removes batch id's claim, whoever holds it; the caller holds the
// lease mutex. A split or blocked batch is never reviewed again as itself.
func (t *Tool) dropClaim(pass int, id string) error {
	if err := os.Remove(t.leaseFile(pass, batchLease(id))); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// attemptsFile is review/attempts/<id>.json, batch id's stalls.
func (t *Tool) attemptsFile(pass int, id string) string {
	return filepath.Join(t.reviewDir(pass), "attempts", id+".json")
}

// clearStalls removes the stalls of the batches a re-plan redrew
// (keepSplits); the caller holds the lease mutex. A batch planned under such
// an id since holds other screens, so its stalls count from one.
func (t *Tool) clearStalls(pass int, ids []string) error {
	for _, id := range ids {
		if err := os.Remove(t.attemptsFile(pass, id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// splitBatch is `batches --split`: it halves o.Split (Batch.halves), records
// the parts on it in batches.json and releases its claim. A batch already
// split answers its parts unchanged; one screen cannot split, and a blocked
// batch stays out of the review. With --claim it claims up to that many of
// the left batches of the split for --owner.
func (t *Tool) splitBatch(pass int, o BatchesOptions) (Result, error) {
	var file BatchesFile
	var parts []Batch
	err := t.editBatches(pass, "split", func(f *BatchesFile) (bool, error) {
		file = *f
		i := f.index(o.Split)
		if i < 0 {
			return false, t.noBatch(pass, "split", o.Split)
		}
		b := f.Batches[i]
		switch {
		case len(b.Parts) > 0:
			for _, id := range b.Parts {
				j := f.index(id)
				if j < 0 {
					return false, fmt.Errorf("%s/review/batches.json: batch %s names part %s, which it does not hold", t.PassDir(pass), b.ID, id)
				}
				parts = append(parts, f.Batches[j])
			}
			return false, nil
		case b.Blocked:
			return false, diag(DiagSelectionInvalid, fmt.Sprintf("batch %s is blocked (%s): it is out of the review, never split", b.ID, b.BlockedReason),
				fmt.Sprintf("vybava ui-loop state --pass %d --json", pass))
		case len(b.Screens) < 2:
			return false, diag(DiagSelectionInvalid, fmt.Sprintf("batch %s holds %d screen and cannot split", b.ID, len(b.Screens)),
				fmt.Sprintf("vybava ui-loop batches --pass %d --block %s --reason \"<why>\" --json", pass, b.ID))
		}
		parts = b.halves()
		f.Batches[i].Parts = []string{parts[0].ID, parts[1].ID}
		f.Batches = slices.Insert(f.Batches, i+1, parts...)
		file = *f
		return true, t.dropClaim(pass, b.ID)
	})
	if err != nil {
		return Result{}, err
	}
	data := SplitData{Pass: pass, PassDir: t.PassDir(pass), File: t.PassDir(pass) + "/review/batches.json", Batch: o.Split, Parts: parts}
	if o.Claim > 0 {
		if o.TTL <= 0 {
			o.TTL = DefaultClaimTTL
		}
		done, err := t.rawBatchIDs(pass)
		if err != nil {
			return Result{}, err
		}
		var left []string
		for _, b := range file.leavesUnder(o.Split) {
			if !done[b.ID] && !b.Blocked {
				left = append(left, b.ID)
			}
		}
		ids, err := t.claimBatches(pass, left, o.Claim, o.Owner, o.TTL)
		if err != nil {
			return Result{}, err
		}
		claimed := []Batch{}
		for _, b := range file.leavesUnder(o.Split) {
			if slices.Contains(ids, b.ID) {
				claimed = append(claimed, b)
			}
		}
		data.Claimed = &claimed
	}
	return Result{Data: data}, nil
}

// stallBatch is `batches --stall`: it counts one reviewer stall of o.Stall
// in review/attempts/<id>.json. Only a batch a reviewer takes stalls: a split
// batch is reviewed through its parts and a blocked one by nobody.
func (t *Tool) stallBatch(pass int, o BatchesOptions) (Result, error) {
	var a Attempts
	err := t.editBatches(pass, "stall", func(f *BatchesFile) (bool, error) {
		i := f.index(o.Stall)
		switch {
		case i < 0:
			return false, t.noBatch(pass, "stall", o.Stall)
		case len(f.Batches[i].Parts) > 0:
			return false, diag(DiagSelectionInvalid, fmt.Sprintf("batch %s is split into %s: stall the part the reviewer took", o.Stall, strings.Join(f.Batches[i].Parts, ", ")),
				fmt.Sprintf("vybava ui-loop batches --pass %d --stall %s --json", pass, f.Batches[i].Parts[0]))
		case f.Batches[i].Blocked:
			return false, diag(DiagSelectionInvalid, fmt.Sprintf("batch %s is blocked (%s): no reviewer takes it, so it never stalls", o.Stall, f.Batches[i].BlockedReason),
				fmt.Sprintf("vybava ui-loop state --pass %d --json", pass))
		}
		file := t.attemptsFile(pass, o.Stall)
		if _, err := readJSON(file, &a); err != nil {
			return false, err
		}
		a.Stalls++
		a.At = append(a.At, t.Now().UTC().Format(time.RFC3339))
		return false, writeJSON(file, a)
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Data: StallData{Batch: o.Stall, Stalls: a.Stalls}}, nil
}

// blockBatch is `batches --block`: it takes o.Block out of the review for
// o.Reason and releases its claim. A split batch is blocked through its parts.
func (t *Tool) blockBatch(pass int, o BatchesOptions) (Result, error) {
	err := t.editBatches(pass, "block", func(f *BatchesFile) (bool, error) {
		i := f.index(o.Block)
		if i < 0 {
			return false, t.noBatch(pass, "block", o.Block)
		}
		b := &f.Batches[i]
		if len(b.Parts) > 0 {
			return false, diag(DiagSelectionInvalid, fmt.Sprintf("batch %s is split into %s: block a part", b.ID, strings.Join(b.Parts, ", ")),
				fmt.Sprintf("vybava ui-loop batches --pass %d --block %s --reason \"<why>\" --json", pass, b.Parts[0]))
		}
		b.Blocked, b.BlockedReason = true, o.Reason
		return true, t.dropClaim(pass, b.ID)
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Data: BlockedBatch{Batch: o.Block, Reason: o.Reason}}, nil
}

// stalls reads review/attempts/*.json: each batch's stalls, the nonzero ones only.
func (t *Tool) stalls(pass int) (map[string]int, error) {
	files, err := filepath.Glob(filepath.Join(t.reviewDir(pass), "attempts", "*.json"))
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, file := range files {
		var a Attempts
		if _, err := readJSON(file, &a); err != nil {
			return nil, err
		}
		if a.Stalls > 0 {
			out[strings.TrimSuffix(filepath.Base(file), ".json")] = a.Stalls
		}
	}
	return out, nil
}
