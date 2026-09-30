package uiloop

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// PublishOptions are the publish verb's flags.
type PublishOptions struct {
	Pass  int
	Areas []string
	// Sets limits publishing to these keys.
	Sets    []string
	Retries int
	// Force re-pushes sets the index already records as pushed.
	Force  bool
	DryRun bool
}

// PublishedSet is one set's outcome, kept in <passDir>/publish/index.json.
type PublishedSet struct {
	Key      string `json:"key"`
	Title    string `json:"title"`
	Area     string `json:"area,omitempty"`
	Files    int    `json:"files"`
	Digest   string `json:"digest"`
	Status   string `json:"status"` // pushed | failed | skipped | planned
	URL      string `json:"url,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	Error    string `json:"error,omitempty"`
	// Refused are the files `board capture` refused; the rest of the set was
	// adopted and pushed. A later publish retries them.
	Refused []RefusedFile `json:"refused,omitempty"`
	// Sections are the viewport × theme blocks the set's board should get.
	Sections []BoardSection `json:"sections,omitempty"`
	Commands [][]string     `json:"commands,omitempty"`
}

// RefusedFile is one file of a set that `board capture` refused.
type RefusedFile struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

// PublishIndex is <passDir>/publish/index.json.
type PublishIndex struct {
	Pass int            `json:"pass"`
	Sets []PublishedSet `json:"sets"`
	// Notes are the shots listed as text instead of uploaded (not an image
	// status), per area in config order, for the review-loop publisher to
	// render as a text card.
	Notes []AreaNotes `json:"notes"`
	// Legacy are the sets an older publish of this pass made by chunking
	// area × viewport × theme. They are never re-adopted (the pass publishes
	// fresh area sets) and never deleted here: their boards are the owner's
	// to clean up.
	Legacy []PublishedSet `json:"legacy,omitempty"`
}

const (
	adoptedLedger = ".ui-loop-adopted"
	descriptor    = ".vitrinka"
	heldDesc      = ".vitrinka.hold"
)

var boardURLRe = regexp.MustCompile(`https://\S+/boards/\S+`)

func setDigest(s Set) string {
	var b strings.Builder
	for _, f := range s.Files {
		b.WriteString(f.Path)
		b.WriteByte('\n')
	}
	return digest(b.String(), 12)
}

// publisher carries one publish run's state.
type publisher struct {
	t       *Tool
	ctx     context.Context
	passDir string
	project string
	retries int
	dryRun  bool
}

func (p *publisher) vitrinka(args ...string) (CmdOut, error) {
	return p.t.Exec(p.ctx, Cmd{Dir: p.t.Root, Args: append([]string{"vitrinka"}, args...), Timeout: 5 * time.Minute})
}

func (p *publisher) file(f PlanFile) string {
	return filepath.Join(p.passDir, filepath.FromSlash(f.Path))
}

// viewportOf is a file's --viewport. A full-content companion is an element
// screenshot of the scroller, narrower than DPR × the page viewport, which
// vitrinka's hi-dpi check refuses; so it passes its own CSS size, read from
// the PNG header. --hidpi stays on for it.
func (p *publisher) viewportOf(f PlanFile) (string, error) {
	if !f.Full {
		return f.Size, nil
	}
	w, h, err := pngSize(p.file(f))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%dx%d@%d", w/DPR, h/DPR, DPR), nil
}

// pngSize reads a PNG's pixel size from its IHDR chunk (bytes 16–23).
func pngSize(file string) (w, h int, err error) {
	fh, err := os.Open(file)
	if err != nil {
		return 0, 0, err
	}
	defer fh.Close()
	var hdr [24]byte
	if _, err := io.ReadFull(fh, hdr[:]); err != nil {
		return 0, 0, fmt.Errorf("%s: reading the PNG header: %w", file, err)
	}
	if string(hdr[:8]) != "\x89PNG\r\n\x1a\n" || string(hdr[12:16]) != "IHDR" {
		return 0, 0, fmt.Errorf("%s: not a PNG", file)
	}
	return int(binary.BigEndian.Uint32(hdr[16:20])), int(binary.BigEndian.Uint32(hdr[20:24])), nil
}

func (p *publisher) captureArgs(root string, f PlanFile, viewport string) []string {
	args := []string{"board", "capture", "web", "--root", root,
		"--file", p.file(f),
		"--label", f.Label, "--title", f.Title, "--route", f.Route, "--note", f.Note,
		"--state", f.State, "--device", f.Viewport, "--viewport", viewport, "--no-input", "--yes"}
	if f.URL != "" {
		args = append(args, "--url", f.URL)
	}
	if len(f.Src) > 0 {
		args = append(args, "--src", strings.Join(f.Src, ","))
	}
	return args
}

func failed(what string, out CmdOut) error {
	msg := strings.TrimSpace(out.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(out.Stdout)
	}
	return fmt.Errorf("%s exited %d: %s", what, out.Code, lastLine(msg))
}

// adopt makes root a set holding exactly the plan's files: init when new, and
// a ledger so a re-run only adopts what is missing. The descriptor stays in
// place, so every `board capture` fires vitrinka's detached per-file push and
// the shot joins the board within seconds; publish's own push afterwards is
// the backstop that commits the set and reports its URL and status.
//
// A file `board capture` refuses (it exits non-zero) is returned in refused
// and stays out of the ledger, so a later publish retries it; the rest of the
// set is still adopted. Only a failure to run vitrinka at all aborts.
func (p *publisher) adopt(root string, s Set) (refused []RefusedFile, err error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	// An older publish held the descriptor aside while it adopted; put it back.
	if err := release(root); err != nil {
		return nil, err
	}
	_, errD := os.Stat(filepath.Join(root, descriptor))
	if errors.Is(errD, fs.ErrNotExist) {
		out, err := p.vitrinka("board", "init", "--root", root, "--key", s.Key, "--title", s.Title, "--project", p.project, "--no-input", "--yes")
		if err != nil {
			return nil, err
		}
		if out.Code != 0 {
			return nil, failed("vitrinka board init", out)
		}
	}
	ledger, err := ledgerFor(p.passDir, root)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	if b, err := os.ReadFile(ledger); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			have[line] = true
		}
	}
	for _, f := range s.Files {
		if have[f.Path] {
			continue
		}
		vp, err := p.viewportOf(f)
		if err != nil {
			refused = append(refused, RefusedFile{Path: f.Path, Error: err.Error()})
			continue
		}
		out, err := p.vitrinka(p.captureArgs(root, f, vp)...)
		if err != nil {
			return refused, err
		}
		if out.Code != 0 {
			refused = append(refused, RefusedFile{Path: f.Path, Error: failed("vitrinka board capture", out).Error()})
			continue
		}
		lf, err := os.OpenFile(ledger, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return refused, err
		}
		_, werr := lf.WriteString(f.Path + "\n")
		if cerr := lf.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return refused, werr
		}
	}
	return refused, nil
}

// ledgerFor is the adopted-files ledger of a set root in one pass:
// <passDir>/publish/adopted/<key>. Per pass, because a root (<out>/sets/<key>)
// is shared by every pass and a ledger line is a pass-relative path. Never in
// the root — `board push` refuses a root holding
// anything but images, sidecars and the manifest, so a ledger inside it failed
// every push. A ledger an older run left in the root is moved out first.
func ledgerFor(passDir, root string) (string, error) {
	ledger := filepath.Join(passDir, "publish", "adopted", filepath.Base(root))
	if err := os.MkdirAll(filepath.Dir(ledger), 0o755); err != nil {
		return "", err
	}
	old := filepath.Join(root, adoptedLedger)
	if _, err := os.Stat(old); err == nil {
		if err := os.Rename(old, ledger); err != nil {
			return "", err
		}
	}
	return ledger, nil
}

func release(root string) error {
	h := filepath.Join(root, heldDesc)
	if _, err := os.Stat(h); err == nil {
		return os.Rename(h, filepath.Join(root, descriptor))
	}
	return nil
}

// push pushes root, retrying; the board URL comes from the --json envelope.
func (p *publisher) push(root, title string) (url string, attempts int, err error) {
	for attempts = 1; attempts <= p.retries; attempts++ {
		out, xerr := p.vitrinka("board", "push", "--root", root, "--title", title, "--yes", "--no-input", "--no-render", "--json")
		if xerr != nil {
			return "", attempts, xerr
		}
		if out.Code == 0 {
			var env struct {
				Data struct {
					URL string `json:"url"`
				} `json:"data"`
			}
			if json.Unmarshal([]byte(out.Stdout), &env) == nil && env.Data.URL != "" {
				return env.Data.URL, attempts, nil
			}
			if m := boardURLRe.FindString(out.Stdout + "\n" + out.Stderr); m != "" {
				return m, attempts, nil
			}
		}
		err = failed("vitrinka board push", out)
		if attempts < p.retries {
			p.t.Sleep(3 * time.Second)
		}
	}
	return "", p.retries, err
}

// publishSet adopts and pushes one set.
func (p *publisher) publishSet(s Set) PublishedSet {
	root := filepath.Join(filepath.Dir(p.passDir), "sets", s.Key)
	rec := PublishedSet{Key: s.Key, Title: s.Title, Area: s.Area, Files: len(s.Files), Digest: setDigest(s), Sections: s.Sections}
	if p.dryRun {
		rec.Status = "planned"
		rec.Commands = append(rec.Commands, append([]string{"vitrinka"}, "board", "init", "--root", root, "--key", s.Key, "--title", s.Title, "--project", p.project, "--no-input", "--yes"))
		for _, f := range s.Files {
			vp, err := p.viewportOf(f)
			if err != nil {
				// Listed as planned; a real publish records the file as refused.
				vp = f.Size
			}
			rec.Commands = append(rec.Commands, append([]string{"vitrinka"}, p.captureArgs(root, f, vp)...))
		}
		rec.Commands = append(rec.Commands, []string{"vitrinka", "board", "push", "--root", root, "--title", s.Title, "--yes", "--no-input", "--no-render", "--json"})
		return rec
	}
	refused, err := p.adopt(root, s)
	rec.Refused = refused
	if err != nil {
		rec.Status, rec.Error = "failed", err.Error()
		return rec
	}
	url, attempts, err := p.push(root, s.Title)
	rec.Attempts = attempts
	if err != nil {
		rec.Status, rec.Error = "failed", err.Error()
		return rec
	}
	rec.Status, rec.URL = "pushed", url
	return rec
}

// Publish adopts the pass's split plan into vitrinka sets under
// <passDir>/publish/sets, one per area, and pushes them one by one.
func (t *Tool) Publish(ctx context.Context, o PublishOptions) (Result, error) {
	pass, err := t.resolveShotPass(o.Pass)
	if err != nil {
		return Result{}, err
	}
	if _, err := t.LookPath("vitrinka"); err != nil && !o.DryRun {
		return Result{}, diag(DiagVitrinkaMissing, "the vitrinka CLI is not on PATH", "vybava install vitrinka-cli")
	}
	passDir := t.passAbs(pass)
	if _, err := os.Stat(passDir); err != nil {
		return Result{}, diag(DiagPassMissing, t.PassDir(pass)+" does not exist", "vybava ui-loop run")
	}
	records, err := LoadRecords(passDir)
	if err != nil {
		return Result{}, err
	}
	return t.publishRecords(ctx, pass, records, o)
}

// publishRecords publishes the given records of a pass: adopt, then push
// every set whose files changed since its last push (or whose push failed).
func (t *Tool) publishRecords(ctx context.Context, pass int, records []Record, o PublishOptions) (Result, error) {
	passDir := t.passAbs(pass)
	plan, diags, err := t.planRecords(pass, records, o.Areas)
	if err != nil {
		return Result{}, err
	}
	if o.Retries <= 0 {
		o.Retries = 3
	}
	p := &publisher{t: t, ctx: ctx, passDir: passDir, project: plan.Project, retries: o.Retries, dryRun: o.DryRun}
	indexFile := filepath.Join(p.passDir, "publish", "index.json")
	index := PublishIndex{Pass: pass, Sets: []PublishedSet{}, Notes: []AreaNotes{}}
	if b, err := os.ReadFile(indexFile); err == nil {
		if err := json.Unmarshal(b, &index); err != nil {
			return Result{}, fmt.Errorf("%s: %w", indexFile, err)
		}
	}
	// A row whose key is no area's set came from the chunked publish: it
	// moves to legacy, and the pass publishes fresh area sets instead.
	areaKeys := map[string]bool{}
	for _, a := range t.Config.Areas {
		areaKeys[areaKey(t.Config.Vitrinka.BoardPrefix, a)] = true
	}
	for _, r := range records {
		areaKeys[areaKey(t.Config.Vitrinka.BoardPrefix, r.Area)] = true
	}
	legacy := map[string]bool{}
	for _, s := range index.Legacy {
		legacy[s.Key] = true
	}
	current := index.Sets[:0:0]
	for _, s := range index.Sets {
		if areaKeys[s.Key] {
			current = append(current, s)
		} else if !legacy[s.Key] {
			legacy[s.Key] = true
			index.Legacy = append(index.Legacy, PublishedSet{Key: s.Key, Title: s.Title, Files: s.Files, Status: s.Status, URL: s.URL})
		}
	}
	index.Sets = current
	prior := map[string]PublishedSet{}
	for _, s := range index.Sets {
		prior[s.Key] = s
	}

	var results []PublishedSet
	for _, s := range plan.Sets {
		if len(o.Sets) > 0 && !slices.Contains(o.Sets, s.Key) {
			continue
		}
		if was, ok := prior[s.Key]; ok && !o.Force && !o.DryRun && was.Status == "pushed" && was.Digest == setDigest(s) && len(was.Refused) == 0 {
			was.Status, was.Sections = "skipped", s.Sections
			results = append(results, was)
			continue
		}
		results = append(results, p.publishSet(s))
	}
	res := Result{Data: map[string]any{"pass": pass, "sets": results, "notes": plan.Notes, "legacy": index.Legacy}, Diagnostics: diags}
	if o.DryRun {
		return res, nil
	}
	merged := map[string]PublishedSet{}
	for _, s := range index.Sets {
		merged[s.Key] = s
	}
	for _, r := range results {
		if r.Status == "skipped" {
			r.Status = "pushed"
		}
		merged[r.Key] = r
	}
	index.Sets = index.Sets[:0]
	for _, s := range plan.Sets {
		if r, ok := merged[s.Key]; ok {
			index.Sets = append(index.Sets, r)
			delete(merged, s.Key)
		}
	}
	left := make([]string, 0, len(merged))
	for k := range merged {
		left = append(left, k)
	}
	slices.Sort(left)
	for _, k := range left {
		index.Sets = append(index.Sets, merged[k])
	}
	// This run's areas replace their notes; an area it did not plan keeps its own.
	notes := slices.DeleteFunc(index.Notes, func(n AreaNotes) bool { return len(o.Areas) == 0 || slices.Contains(o.Areas, n.Area) })
	notes = append(notes, plan.Notes...)
	slices.SortStableFunc(notes, func(a, b AreaNotes) int { return orderOf(t.Config.Areas, a.Area) - orderOf(t.Config.Areas, b.Area) })
	if notes == nil {
		notes = []AreaNotes{}
	}
	index.Notes = notes
	b, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return res, err
	}
	if err := os.MkdirAll(filepath.Dir(indexFile), 0o755); err != nil {
		return res, err
	}
	if err := os.WriteFile(indexFile, append(b, '\n'), 0o644); err != nil {
		return res, err
	}
	for _, r := range results {
		if r.Status == "failed" {
			res.Diagnostics = append(res.Diagnostics, errDiag(DiagPublishFailed, r.Key+": "+r.Error, fmt.Sprintf("vybava ui-loop publish --pass %d --sets %s", pass, r.Key)))
		}
		if n := len(r.Refused); n > 0 {
			res.Diagnostics = append(res.Diagnostics, warn(DiagPublishRefused,
				fmt.Sprintf("%s: vitrinka board capture refused %d of %d files (first: %s: %s); see refused in %s", r.Key, n, r.Files, r.Refused[0].Path, r.Refused[0].Error, filepath.Join(t.PassDir(pass), "publish", "index.json")),
				fmt.Sprintf("vybava ui-loop publish --pass %d --sets %s", pass, r.Key)))
		}
	}
	if len(index.Legacy) > 0 {
		res.Diagnostics = append(res.Diagnostics, info(DiagLegacySets,
			fmt.Sprintf("%d chunked sets of an older publish of pass %d are listed under legacy in %s; their boards are not deleted", len(index.Legacy), pass, filepath.Join(t.PassDir(pass), "publish", "index.json")),
			"delete their boards once the area boards replace them"))
	}
	res.Next = []string{fmt.Sprintf("vybava ui-loop scoreboard --pass %d --json", pass)}
	return res, nil
}
