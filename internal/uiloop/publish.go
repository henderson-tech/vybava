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
// ledger so a re-run only adopts what is missing.
func (p *publisher) adopt(root string, s Set) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	_, errD := os.Stat(filepath.Join(root, descriptor))
	_, errH := os.Stat(filepath.Join(root, heldDesc))
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
	ledger := filepath.Join(root, adoptedLedger)
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
		_, werr := lf.WriteString(f.Path + "\n")
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
	}
	return nil
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

// publishSet adopts and pushes one set; a set that will not push is halved
// once (the tail moves to <key>b) and both halves are pushed.
func (p *publisher) publishSet(s Set, canHalve bool) []PublishedSet {
	root := filepath.Join(p.passDir, "publish", "sets", s.Key)
	rec := PublishedSet{Key: s.Key, Title: s.Title, Files: len(s.Files), Digest: setDigest(s)}
	if p.dryRun {
		rec.Status = "planned"
		rec.Commands = append(rec.Commands, append([]string{"vitrinka"}, "board", "init", "--root", root, "--key", s.Key, "--title", s.Title, "--project", p.project, "--no-input", "--yes"))
		for _, f := range s.Files {
			rec.Commands = append(rec.Commands, append([]string{"vitrinka"}, p.captureArgs(root, f)...))
		}
		rec.Commands = append(rec.Commands, []string{"vitrinka", "board", "push", "--root", root, "--title", s.Title, "--yes", "--no-input", "--no-render", "--json"})
		return []PublishedSet{rec}
	}
	if err := p.adopt(root, s); err != nil {
		rec.Status, rec.Error = "failed", err.Error()
		return []PublishedSet{rec}
	}
	url, attempts, err := p.push(root, s.Title)
	rec.Attempts = attempts
	if err == nil {
		rec.Status, rec.URL = "pushed", url
		return []PublishedSet{rec}
	}
	rec.Status, rec.Error = "failed", err.Error()
	if !canHalve || len(s.Files) < 2 {
		return []PublishedSet{rec}
	}
	head, tail, ok := halves(s)
	if !ok {
		return []PublishedSet{rec}
	}
	if err := os.RemoveAll(root); err != nil {
		rec.Error += "; " + err.Error()
		return []PublishedSet{rec}
	}
	rec.Status, rec.HalvedInto = "halved", []string{head.Key, tail.Key}
	out := []PublishedSet{rec}
	out = append(out, p.publishSet(head, false)...)
	out = append(out, p.publishSet(tail, false)...)
	return out
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
	}
	prior := map[string]PublishedSet{}
	for _, s := range index.Sets {
		prior[s.Key] = s
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
		return was, ok && !o.Force && !o.DryRun && was.Status == "pushed" && was.Digest == setDigest(s)
	}
	for _, s := range plan.Sets {
		head, tail, split := halves(s)
		// A set halved on an earlier run is worked as its two halves, so a
		// failed half is retried by the key its diagnostic names.
		if was, ok := prior[s.Key]; ok && split && !o.Force && was.Status == "halved" && was.Digest == setDigest(s) {
			if !want(s.Key, head.Key, tail.Key) {
				continue
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
				results = append(results, p.publishSet(h, false)...)
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
		results = append(results, p.publishSet(s, true)...)
	}
	res := Result{Data: map[string]any{"pass": pass, "sets": results}, Diagnostics: diags}
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
		for _, k := range []string{s.Key, s.Key + "a", s.Key + "b"} {
			if r, ok := merged[k]; ok {
				index.Sets = append(index.Sets, r)
				delete(merged, k)
			}
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
		if r.Status == "failed" && len(r.HalvedInto) == 0 {
			res.Diagnostics = append(res.Diagnostics, errDiag(DiagPublishFailed, r.Key+": "+r.Error, fmt.Sprintf("vybava ui-loop publish --pass %d --sets %s", pass, r.Key)))
		}
	}
	res.Next = []string{fmt.Sprintf("vybava ui-loop scoreboard --pass %d --json", pass)}
	return res, nil
}
