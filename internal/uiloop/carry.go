package uiloop

// carry.go carries a screen's review into the next pass when its pixels did
// not move: `batches` leaves it out of the reviewer batches and merge-review
// copies the previous pass's verdicts on it (planCarry).

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"io"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
)

// planBatches is a pass's v2 batches: the screens planCarry carries drop out
// and each batch carries its screens' digests.
func (t *Tool) planBatches(pass int, records []Record, size int, snap passSnapshot) (BatchesFile, error) {
	carried, err := t.planCarry(pass, records, snap)
	if err != nil {
		return BatchesFile{}, err
	}
	digests, err := t.screenDigests(pass, records, snap.hashes)
	if err != nil {
		return BatchesFile{}, err
	}
	skip := map[string]bool{}
	for i, c := range carried {
		skip[c.Screen] = true
		carried[i].Digest = digests[c.Screen]
	}
	screens := slices.DeleteFunc(passScreens(records), func(s screen) bool { return skip[s.id] })
	f := BatchesFile{V: 2, Pass: pass, Size: size, Batches: ComputeBatches(screens, t.Config.Areas, size), Carried: carried}
	for i, b := range f.Batches {
		f.Batches[i].Digests = map[string]string{}
		for _, id := range b.Screens {
			f.Batches[i].Digests[id] = digests[id]
		}
	}
	return f, nil
}

// planCarry is the screens of pass that carry the review of pass-1, sorted.
// Nothing carries unless pass-1 has a backlog (so merge-review folds it) that
// is current (its basis.json matches) and lists reviewed, and every batch of
// pass-1 is complete or blocked. A screen then carries when that backlog
// lists it in reviewed and names it in no open item (any status but met), it
// has an ok shot here, its manifest entry and the spec are as pass-1
// recorded them (screenEvidence: a verdict judged against other known issues
// or other rules is not evidence here), and its ok shots here and there are
// the same viewport × theme set whose PNGs (the viewport capture and the full
// companion) are byte-equal or differ in at most uiLoop.review.carryTolerance
// of their pixels; a size change is a move.
func (t *Tool) planCarry(pass int, records []Record, snap passSnapshot) ([]Carried, error) {
	carried := []Carried{}
	previous, ref, err := t.previousBacklog(pass)
	if err != nil || previous == nil || ref.Pass != pass-1 || previous.Reviewed == nil || previous.Pass != ref.Pass {
		return carried, err
	}
	from := ref.Pass
	then, err := t.snapshot(from, nil)
	if err != nil {
		return nil, err
	}
	current, err := t.backlogEvidenceCurrent(from, filepath.Join(t.reviewDir(from), "backlog.json"), then.basis)
	if err != nil || !current {
		return carried, err
	}
	thenRecords := then.records
	batches, _, err := t.loadBatches(from, thenRecords, 0)
	if err != nil {
		return nil, err
	}
	done, err := t.rawBatchIDs(from, then)
	if err != nil {
		return nil, err
	}
	// A blocked batch finished its review too: its screens are unreviewed
	// there, so they never carry.
	if slices.ContainsFunc(batches.leaves(), func(b Batch) bool { return !done[b.ID] && !b.Blocked }) {
		return carried, nil
	}
	open := map[string]bool{}
	for _, f := range previous.Findings {
		if f.Open() {
			open[f.Screen] = true
		}
	}
	here, err := t.okFiles(pass, records, snap)
	if err != nil {
		return nil, err
	}
	there, err := t.okFiles(from, thenRecords, then)
	if err != nil {
		return nil, err
	}
	judgedNow, err := t.screenEvidence(pass, records, snap.hashes)
	if err != nil {
		return nil, err
	}
	judgedThen, err := t.screenEvidence(from, thenRecords, then.hashes)
	if err != nil {
		return nil, err
	}
	tolerance := t.Config.CarryToleranceOrDefault()
	for _, s := range passScreens(records) {
		if open[s.id] || !slices.Contains(previous.Reviewed, s.id) || len(here[s.id]) == 0 || judgedThen[s.id] == nil ||
			judgedNow[s.id].Spec != judgedThen[s.id].Spec || !reflect.DeepEqual(judgedNow[s.id].Screen, judgedThen[s.id].Screen) {
			continue
		}
		same, err := sameShots(here[s.id], there[s.id], tolerance)
		if err != nil {
			return nil, err
		}
		if same {
			carried = append(carried, Carried{Screen: s.id, From: from})
		}
	}
	sort.Slice(carried, func(i, j int) bool { return carried[i].Screen < carried[j].Screen })
	return carried, nil
}

// carriesNow splits the carries batches.json planned by the screens' current
// digests (nil without provenance, when every carry stands): a carried
// screen whose digest moved since (a --resume retake) is reopened, so it is
// neither carried nor reviewed until batches plans the pass again.
func carriesNow(carried []Carried, digests map[string]string) (current []Carried, reopened []string) {
	for _, c := range carried {
		if digests == nil || (c.Digest != "" && c.Digest == digests[c.Screen]) {
			current = append(current, c)
		} else {
			reopened = append(reopened, c.Screen)
		}
	}
	return current, reopened
}

// shotPNG is one PNG of a shot: its path ("" when the shot has none) and its
// SHA256 from the pass's snapshot.
type shotPNG struct{ path, sha string }

// okFiles maps each screen to its ok shots (<viewport>.<theme>) and their
// viewport and full PNGs.
func (t *Tool) okFiles(pass int, records []Record, snap passSnapshot) (map[string]map[string][2]shotPNG, error) {
	prefix, err := filepath.Rel(t.Root, t.passAbs(pass))
	if err != nil {
		return nil, err
	}
	out := map[string]map[string][2]shotPNG{}
	for _, r := range records {
		if r.Status != "ok" {
			continue
		}
		var files [2]shotPNG
		for i, name := range []string{r.Files.Viewport, r.Files.Full} {
			if name != "" {
				rel := path.Join(filepath.ToSlash(prefix), r.Dir, name)
				files[i] = shotPNG{path: t.abs(rel), sha: snap.hashes[rel]}
			}
		}
		if out[r.ID] == nil {
			out[r.ID] = map[string][2]shotPNG{}
		}
		out[r.ID][r.Viewport+"."+r.Theme] = files
	}
	return out, nil
}

// sameShots reports whether a screen's ok shots in two passes are the same
// viewport × theme set whose PNGs match (samePNG).
func sameShots(now, before map[string][2]shotPNG, tolerance float64) (bool, error) {
	if len(now) != len(before) {
		return false, nil
	}
	for key, files := range now {
		prior, ok := before[key]
		if !ok {
			return false, nil
		}
		for i := range files {
			if same, err := samePNG(files[i], prior[i], tolerance); err != nil || !same {
				return false, err
			}
		}
	}
	return true, nil
}

// samePNG compares one PNG of a shot across two passes: both absent, the same
// bytes, or samePixels.
func samePNG(a, b shotPNG, tolerance float64) (bool, error) {
	switch {
	case a.path == "" || b.path == "":
		return a.path == b.path, nil
	case a.sha != "" && a.sha == b.sha:
		return true, nil
	}
	return samePixels(a.path, b.path, tolerance)
}

// samePixels reports whether two PNGs of one size differ in at most tolerance
// of their pixels, RGBA compared exactly. A size change, or a file that holds
// no decodable PNG, is a move.
func samePixels(a, b string, tolerance float64) (bool, error) {
	var sizes [2]image.Config
	for i, file := range []string{a, b} {
		c, ok, err := readPNG(file, png.DecodeConfig)
		if err != nil || !ok {
			return false, err
		}
		sizes[i] = c
	}
	if sizes[0].Width != sizes[1].Width || sizes[0].Height != sizes[1].Height {
		return false, nil
	}
	var pix [2]*image.RGBA
	for i, file := range []string{a, b} {
		img, ok, err := readPNG(file, png.Decode)
		if err != nil || !ok {
			return false, err
		}
		pix[i] = toRGBA(img)
	}
	w, h := sizes[0].Width, sizes[0].Height
	limit := int(tolerance * float64(w*h))
	differ := 0
	for y := 0; y < h; y++ {
		rowA := pix[0].Pix[y*pix[0].Stride : y*pix[0].Stride+4*w]
		rowB := pix[1].Pix[y*pix[1].Stride : y*pix[1].Stride+4*w]
		if bytes.Equal(rowA, rowB) {
			continue
		}
		for x := 0; x < 4*w; x += 4 {
			if !bytes.Equal(rowA[x:x+4], rowB[x:x+4]) {
				if differ++; differ > limit {
					return false, nil
				}
			}
		}
	}
	return true, nil
}

// readPNG decodes file with decode; ok is false when it holds no decodable PNG.
func readPNG[T any](file string, decode func(io.Reader) (T, error)) (v T, ok bool, err error) {
	f, err := os.Open(file)
	if err != nil {
		return v, false, err
	}
	defer f.Close()
	v, err = decode(bufio.NewReader(f))
	var format png.FormatError
	var unsupported png.UnsupportedError
	switch {
	case errors.As(err, &format), errors.As(err, &unsupported), errors.Is(err, io.ErrUnexpectedEOF):
		return v, false, nil
	case err != nil:
		return v, false, fmt.Errorf("%s: %w", file, err)
	}
	return v, true, nil
}

// toRGBA is img as *image.RGBA at the origin (what png.Decode gives an RGB
// screenshot already).
func toRGBA(img image.Image) *image.RGBA {
	if rgba, ok := img.(*image.RGBA); ok && rgba.Rect.Min == (image.Point{}) {
		return rgba
	}
	b := img.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(rgba, rgba.Rect, img, b.Min, draw.Src)
	return rgba
}
