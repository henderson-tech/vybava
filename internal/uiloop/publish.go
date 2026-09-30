package uiloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	Files    int    `json:"files"`
	Digest   string `json:"digest"`
	Status   string `json:"status"` // pushed | failed | halved (see HalvedInto) | skipped | planned
	URL      string `json:"url,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	Error    string `json:"error,omitempty"`
	// HalvedInto names the two sets (<key>a, <key>b) a failing set was split into.
	HalvedInto []string   `json:"halvedInto,omitempty"`
	Commands   [][]string `json:"commands,omitempty"`
}

// PublishIndex is <passDir>/publish/index.json.
type PublishIndex struct {
	Pass int            `json:"pass"`
	Sets []PublishedSet `json:"sets"`
}

const (
	adoptedLedger = ".ui-loop-adopted"
	descriptor    = ".vitrinka"
	heldDesc      = ".vitrinka.hold"
)

var boardURLRe = regexp.MustCompile(`https://\S+/boards/\S+`)

func setDigest(s Set) string {
	b, _ := json.Marshal(s) // Set contains only JSON-safe values.
	return digest(string(b), 64)
}

func fileIdentity(f PlanFile) string {
	body, _ := json.Marshal(f) // PlanFile contains only JSON-safe values.
	return f.Path + "\t" + digest(string(body), 64)
}

// publisher carries one publish run's state.
type publisher struct {
	t       *Tool
	ctx     context.Context
	passDir string
	project string
	retries int
	dryRun  bool
	receipt func(PublishedSet) error
}

func (p *publisher) vitrinka(args ...string) (CmdOut, error) {
	return p.t.Exec(p.ctx, Cmd{Dir: p.t.Root, Args: append([]string{"vitrinka"}, args...), Timeout: 5 * time.Minute})
}

func (p *publisher) captureArgs(root string, f PlanFile) []string {
	args := []string{"board", "capture", "web", "--root", root,
		"--file", filepath.Join(p.passDir, filepath.FromSlash(f.Path)),
		"--label", f.Label, "--title", f.Title, "--route", f.Route, "--note", f.Note,
		"--state", f.State, "--viewport", f.Viewport, "--no-input", "--yes"}
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

// adopt makes root a set holding exactly the plan's files: init when new,
// the descriptor held aside so `board capture` fires no per-shot push, and a
// ledger so a re-run skips a complete set or rebuilds incomplete staging.
func (p *publisher) adopt(root string, s Set) error {
	ledger := filepath.Join(root, adoptedLedger)
	have := map[string]bool{}
	if b, err := os.ReadFile(ledger); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if line != "" {
				have[line] = true
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read adoption ledger: %w", err)
	}
	want := map[string]bool{}
	for _, f := range s.Files {
		want[fileIdentity(f)] = true
	}
	complete := len(have) == len(want)
	for identity := range want {
		complete = complete && have[identity]
	}
	if !complete {
		// Capture appends to its manifest. Rebuild our disposable staging root
		// after a changed plan or partial adoption, including a cutoff between
		// a capture acknowledgement and its ledger write. The board key stays stable.
		if err := os.RemoveAll(root); err != nil {
			return err
		}
		have = map[string]bool{}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	_, errD := os.Stat(filepath.Join(root, descriptor))
	_, errH := os.Stat(filepath.Join(root, heldDesc))
	if errD != nil && !errors.Is(errD, fs.ErrNotExist) {
		return errD
	}
	if errH != nil && !errors.Is(errH, fs.ErrNotExist) {
		return errH
	}
	if errors.Is(errD, fs.ErrNotExist) && errors.Is(errH, fs.ErrNotExist) {
		out, err := p.vitrinka("board", "init", "--root", root, "--key", s.Key, "--title", s.Title, "--project", p.project, "--no-input", "--yes")
		if err != nil {
			return err
		}
		if out.Code != 0 {
			return failed("vitrinka board init", out)
		}
	}
	if err := hold(root); err != nil {
		return err
	}
	for _, f := range s.Files {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		identity := fileIdentity(f)
		if have[identity] {
			continue
		}
		out, err := p.vitrinka(p.captureArgs(root, f)...)
		if err != nil {
			return err
		}
		if out.Code != 0 {
			return failed("vitrinka board capture "+f.Path, out)
		}
		lf, err := os.OpenFile(ledger, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		_, werr := lf.WriteString(identity + "\n")
		if cerr := lf.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return werr
		}
	}
	return release(root)
}

func hold(root string) error {
	d := filepath.Join(root, descriptor)
	if _, err := os.Stat(d); err == nil {
		return os.Rename(d, filepath.Join(root, heldDesc))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func release(root string) error {
	h := filepath.Join(root, heldDesc)
	if _, err := os.Stat(h); err == nil {
		return os.Rename(h, filepath.Join(root, descriptor))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// push pushes root, retrying; the board URL comes from the --json envelope.
func (p *publisher) push(root, title string) (url string, attempts int, err error) {
	for attempts = 1; attempts <= p.retries; attempts++ {
		if err := p.ctx.Err(); err != nil {
			return "", attempts, err
		}
		out, xerr := p.vitrinka("board", "push", "--root", root, "--title", title, "--yes", "--no-input", "--no-render", "--json")
		if xerr != nil {
			return "", attempts, xerr
		}
		if out.Code == 0 {
			var env struct {
				OK   *bool `json:"ok"`
				Data struct {
					URL string `json:"url"`
				} `json:"data"`
			}
			if json.Unmarshal([]byte(out.Stdout), &env) == nil && (env.OK == nil || *env.OK) && env.Data.URL != "" {
				return env.Data.URL, attempts, nil
			}
			if env.OK != nil && !*env.OK {
				return "", attempts, fmt.Errorf("vitrinka board push returned ok=false: %s", lastLine(out.Stdout))
			}
			if m := boardURLRe.FindString(out.Stdout + "\n" + out.Stderr); m != "" {
				return m, attempts, nil
			}
		}
		if out.Code == 0 {
			err = errors.New("vitrinka board push returned no board URL")
		} else {
			err = failed("vitrinka board push", out)
		}
		if attempts < p.retries {
			p.t.Sleep(3 * time.Second)
		}
	}
	return "", p.retries, err
}

// publishSet adopts and pushes one set; a set that will not push is halved
// once (the tail moves to <key>b) and both halves are pushed.
func (p *publisher) finish(rec PublishedSet) ([]PublishedSet, error) {
	if p.receipt != nil {
		if err := p.receipt(rec); err != nil {
			return []PublishedSet{rec}, err
		}
	}
	return []PublishedSet{rec}, nil
}

func (p *publisher) publishSet(s Set, canHalve bool) ([]PublishedSet, error) {
	if err := p.ctx.Err(); err != nil {
		return nil, err
	}
	root := filepath.Join(p.passDir, "publish", "sets", s.Key)
	rec := PublishedSet{Key: s.Key, Title: s.Title, Files: len(s.Files), Digest: setDigest(s)}
	if p.dryRun {
		rec.Status = "planned"
		rec.Commands = append(rec.Commands, append([]string{"vitrinka"}, "board", "init", "--root", root, "--key", s.Key, "--title", s.Title, "--project", p.project, "--no-input", "--yes"))
		for _, f := range s.Files {
			rec.Commands = append(rec.Commands, append([]string{"vitrinka"}, p.captureArgs(root, f)...))
		}
		rec.Commands = append(rec.Commands, []string{"vitrinka", "board", "push", "--root", root, "--title", s.Title, "--yes", "--no-input", "--no-render", "--json"})
		return []PublishedSet{rec}, nil
	}
	if err := p.adopt(root, s); err != nil {
		if cancelErr := p.ctx.Err(); cancelErr != nil {
			return nil, cancelErr
		}
		rec.Status, rec.Error = "failed", err.Error()
		return p.finish(rec)
	}
	url, attempts, err := p.push(root, s.Title)
	rec.Attempts = attempts
	if err == nil {
		rec.Status, rec.URL = "pushed", url
		return p.finish(rec)
	}
	if cancelErr := p.ctx.Err(); cancelErr != nil {
		return nil, cancelErr // a cutoff is not evidence that a set needs halving
	}
	rec.Status, rec.Error = "failed", err.Error()
	if !canHalve || len(s.Files) < 2 {
		return p.finish(rec)
	}
	head, tail, ok := halves(s)
	if !ok {
		return p.finish(rec)
	}
	if err := os.RemoveAll(root); err != nil {
		rec.Error += "; " + err.Error()
		return p.finish(rec)
	}
	rec.Status, rec.HalvedInto = "halved", []string{head.Key, tail.Key}
	out, err := p.finish(rec)
	if err != nil {
		return out, err
	}
	for _, half := range []Set{head, tail} {
		rows, err := p.publishSet(half, false)
		out = append(out, rows...)
		if err != nil {
			return out, err
		}
	}
	return out, nil
}

// halves splits a set in two: <key>a takes the head, <key>b the tail. A
// shot's viewport + full pair stays together when the cut would split it.
// Deterministic, so a later run finds the same halves by key.
func halves(s Set) (head, tail Set, ok bool) {
	cut := (len(s.Files) + 1) / 2
	if cut < len(s.Files) && s.Files[cut].Full && s.Files[cut].Shot == s.Files[cut-1].Shot {
		cut++
	}
	if len(s.Files) < 2 || cut >= len(s.Files) {
		return s, s, false
	}
	head, tail = s, s
	head.Files, tail.Files = s.Files[:cut], s.Files[cut:]
	head.Bytes, tail.Bytes = 0, 0
	for _, f := range head.Files {
		head.Bytes += f.Bytes
	}
	for _, f := range tail.Files {
		tail.Bytes += f.Bytes
	}
	head.Key, head.Title = s.Key+"a", s.Title+" (a)"
	tail.Key, tail.Title = s.Key+"b", s.Title+" (b)"
	return head, tail, true
}

// Publish adopts the pass's split plan into vitrinka sets under
// <passDir>/publish/sets and pushes them one by one.
func (t *Tool) Publish(ctx context.Context, o PublishOptions) (Result, error) {
	pass, err := t.resolveShotPass(o.Pass)
	if err != nil {
		return Result{}, err
	}
	if _, err := t.LookPath("vitrinka"); err != nil && !o.DryRun {
		return Result{}, diag(DiagVitrinkaMissing, "the vitrinka CLI is not on PATH", "vybava install vitrinka-cli")
	}
	plan, diags, err := t.plan(pass, o.Areas)
	if err != nil {
		return Result{}, err
	}
	if o.Retries <= 0 {
		o.Retries = 3
	}
	p := &publisher{t: t, ctx: ctx, passDir: t.passAbs(pass), project: plan.Project, retries: o.Retries, dryRun: o.DryRun}
	indexFile := filepath.Join(p.passDir, "publish", "index.json")
	index := PublishIndex{Pass: pass, Sets: []PublishedSet{}}
	if b, err := os.ReadFile(indexFile); err == nil {
		if err := json.Unmarshal(b, &index); err != nil {
			return Result{}, fmt.Errorf("%s: %w", indexFile, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Result{}, fmt.Errorf("read publish index: %w", err)
	}
	if index.Pass != pass {
		return Result{}, fmt.Errorf("publish index belongs to pass %d, expected %d", index.Pass, pass)
	}
	prior := map[string]PublishedSet{}
	for _, s := range index.Sets {
		prior[s.Key] = s
	}
	saveState := func() error {
		index.Sets = index.Sets[:0]
		keys := make([]string, 0, len(prior))
		for key := range prior {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		for _, key := range keys {
			index.Sets = append(index.Sets, prior[key])
		}
		return writePublishIndex(indexFile, index)
	}
	if !o.DryRun {
		p.receipt = func(row PublishedSet) error {
			prior[row.Key] = row
			return saveState()
		}
	}
	var results []PublishedSet
	want := func(keys ...string) bool {
		if len(o.Sets) == 0 {
			return true
		}
		for _, k := range keys {
			if slices.Contains(o.Sets, k) {
				return true
			}
		}
		return false
	}
	pushed := func(s Set) (PublishedSet, bool) {
		was, ok := prior[s.Key]
		return was, ok && !o.Force && !o.DryRun && was.Status == "pushed" && was.URL != "" && was.Digest == setDigest(s)
	}
	for _, s := range plan.Sets {
		head, tail, split := halves(s)
		// A set halved on an earlier run is worked as its two halves, so a
		// failed half is retried by the key its diagnostic names. --force only
		// re-pushes halves already pushed; it never re-merges them into the parent.
		if was, ok := prior[s.Key]; ok && split && was.Status == "halved" {
			if !want(s.Key, head.Key, tail.Key) {
				continue
			}
			was.Digest, was.Files, was.Title = setDigest(s), len(s.Files), s.Title
			if p.receipt != nil {
				if err := p.receipt(was); err != nil {
					return Result{}, err
				}
			}
			results = append(results, was)
			for _, h := range []Set{head, tail} {
				if !want(s.Key, h.Key) {
					continue
				}
				if done, ok := pushed(h); ok {
					done.Status = "skipped"
					results = append(results, done)
					continue
				}
				rows, err := p.publishSet(h, false)
				results = append(results, rows...)
				if err != nil {
					return Result{}, err
				}
			}
			continue
		}
		if !want(s.Key, head.Key, tail.Key) {
			continue
		}
		if done, ok := pushed(s); ok {
			done.Status = "skipped"
			results = append(results, done)
			continue
		}
		rows, err := p.publishSet(s, true)
		results = append(results, rows...)
		if err != nil {
			return Result{}, err
		}
	}
	res := Result{Data: map[string]any{"pass": pass, "sets": results}, Diagnostics: diags}
	if o.DryRun {
		return res, nil
	}
	if err := saveState(); err != nil {
		return res, err
	}
	for _, r := range results {
		if r.Status == "failed" && len(r.HalvedInto) == 0 {
			res.Diagnostics = append(res.Diagnostics, errDiag(DiagPublishFailed, r.Key+": "+r.Error, fmt.Sprintf("vybava ui-loop publish --pass %d --sets %s", pass, r.Key)))
		}
	}
	res.Next = []string{fmt.Sprintf("vybava ui-loop scoreboard --pass %d --json", pass)}
	return res, nil
}

// Each acknowledged upload is durable before another set starts. Rename
// prevents a cutoff from leaving an unreadable, partially written receipt.
func writePublishIndex(file string, index PublishIndex) error {
	b, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(file), ".publish-index-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(f.Name(), file); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(file))
	if err != nil {
		return err
	}
	syncErr := dir.Sync()
	if closeErr := dir.Close(); syncErr == nil {
		syncErr = closeErr
	}
	return syncErr
}
