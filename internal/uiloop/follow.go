package uiloop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// FollowOptions are publish --follow's flags.
type FollowOptions struct {
	Pass  int
	Areas []string
	// From is the pass directory on the box (user@host:path); default
	// publish.from + /<out>/pass-<n>/.
	From      string
	Interval  time.Duration
	UntilIdle time.Duration
	Retries   int
}

// DoneFile is <passDir>/done.json, written by the harness teardown when a
// capture run ends (harness/teardown.ts DoneFile). Run is the createdAt of
// the run.json it finished, so the marker of an earlier run (before a
// --resume) never ends a later one.
type DoneFile struct {
	V          int    `json:"v"`
	Pass       int    `json:"pass"`
	Run        string `json:"run"`
	FinishedAt string `json:"finishedAt"`
	Shots      int    `json:"shots"`
}

// FollowData is what follow reports when it stops.
type FollowData struct {
	Pass  int    `json:"pass"`
	From  string `json:"from"`
	Ticks int    `json:"ticks"`
	// Done: the run's done.json arrived.
	Done bool `json:"done"`
	// Final counts the shots published (images) or listed (notes).
	Final int            `json:"final"`
	Sets  []PublishedSet `json:"sets"`
	Notes []AreaNotes    `json:"notes"`
}

// followFetchTries is how many failed fetches in a row end a follow.
const followFetchTries = 5

// resumeKeeps are the statuses a --resume run keeps (harness/capture.ts
// FINAL_STATUSES); a record with one is final even when an earlier run took it.
var resumeKeeps = []string{"ok", "unreachable"}

func readRunFile(passDir string) (RunFile, error) {
	var run RunFile
	b, err := os.ReadFile(filepath.Join(passDir, "run.json"))
	if err != nil {
		return run, err
	}
	if err := json.Unmarshal(b, &run); err != nil {
		return run, fmt.Errorf("%s: %w", filepath.Join(passDir, "run.json"), err)
	}
	return run, nil
}

// runDone reports whether the teardown of the run described by run.json
// has written its done.json.
func runDone(passDir string, run RunFile) bool {
	b, err := os.ReadFile(filepath.Join(passDir, "done.json"))
	if err != nil {
		return false
	}
	var d DoneFile
	return json.Unmarshal(b, &d) == nil && d.Run == run.CreatedAt
}

// finalRecords keeps the records the running capture will not rewrite:
// taken by this run (capturedAt at or after run.json's createdAt), a status
// --resume keeps, or any once the run is done. Their captures must be on
// disk too: rsync moves each file into place whole, but may bring a record
// before its PNG.
func finalRecords(passDir string, records []Record, run RunFile, done bool) []Record {
	started, serr := time.Parse(time.RFC3339, run.CreatedAt)
	var out []Record
	for _, r := range records {
		if !done && !slices.Contains(resumeKeeps, r.Status) {
			at, err := time.Parse(time.RFC3339Nano, r.CapturedAt)
			if serr != nil || err != nil || at.Before(started) {
				continue
			}
		}
		complete := true
		for _, f := range []string{r.Files.Viewport, r.Files.Full} {
			if f == "" {
				continue
			}
			if _, err := os.Stat(filepath.Join(passDir, filepath.FromSlash(r.Dir), f)); err != nil {
				complete = false
			}
		}
		if complete {
			out = append(out, r)
		}
	}
	return out
}

// Follow publishes a pass while a box captures it: each tick rsyncs the pass
// back (never its .auth/ storage states or playwright/ output), publishes
// every shot that became final — the same plan, ledger and index as
// Publish — and sleeps. Each adopted shot rides vitrinka's detached per-file
// push onto its area's board at once; the tick then pushes every area set
// that gained files, the backstop that records its URL and status. It stops once the run's done.json
// has arrived and no new shot has for --until-idle.
func (t *Tool) Follow(ctx context.Context, o FollowOptions) (Result, error) {
	pass, err := t.ResolvePass(o.Pass, true)
	if err != nil {
		return Result{}, err
	}
	if _, err := t.LookPath("vitrinka"); err != nil {
		return Result{}, diag(DiagVitrinkaMissing, "the vitrinka CLI is not on PATH", "vybava install vitrinka-cli")
	}
	if _, err := t.LookPath("rsync"); err != nil {
		return Result{}, diag(DiagFetchFailed, "rsync is not on PATH", "install rsync")
	}
	passDir := t.passAbs(pass)
	run, err := readRunFile(passDir)
	if errors.Is(err, fs.ErrNotExist) {
		return Result{}, diag(DiagPassMissing, t.PassDir(pass)+"/run.json does not exist: follow reads the run it waits for", "vybava ui-loop run --print")
	}
	if err != nil {
		return Result{}, err
	}
	from := o.From
	if from == "" && t.Config.Publish.From != "" {
		from = strings.TrimRight(t.Config.Publish.From, "/") + "/" + t.PassDir(pass)
	}
	if from == "" {
		return Result{}, diag(DiagFollowSource, "no box to fetch the pass from", fmt.Sprintf("--from devops:ws/<workspace>/<app-dir>/%s/, or set publish.from", t.PassDir(pass)))
	}
	from = strings.TrimRight(from, "/") + "/"
	if o.Interval <= 0 {
		o.Interval = 30 * time.Second
	}
	if o.UntilIdle <= 0 {
		o.UntilIdle = 10 * time.Minute
	}
	fetch := []string{"rsync", "-a", "--exclude=/.auth/", "--exclude=/playwright/", "--exclude=/publish/", "--exclude=/run.json", "--exclude=*.tmp-*", from, passDir + "/"}

	data := FollowData{Pass: pass, From: from}
	var last Result
	var warnings []runxDiagnostic
	seen := map[string]bool{}
	lastNew := t.Now()
	fails := 0
	for {
		data.Ticks++
		out, err := t.Exec(ctx, Cmd{Dir: t.Root, Args: fetch, Timeout: 15 * time.Minute})
		if err != nil {
			return Result{}, err
		}
		if out.Code != 0 {
			fails++
			fmt.Fprintf(t.Log, "ui-loop follow: tick %d: %v\n", data.Ticks, failed("rsync", out))
			if fails >= followFetchTries {
				host, _, _ := strings.Cut(from, ":")
				return Result{}, diag(DiagFetchFailed, fmt.Sprintf("%d fetches in a row failed: %v", fails, failed("rsync "+from, out)), "check the box answers: ssh "+host)
			}
		} else {
			fails = 0
		}
		records, err := LoadRecords(passDir)
		if err != nil {
			return Result{}, err
		}
		data.Done = runDone(passDir, run)
		final := finalRecords(passDir, records, run, data.Done)
		fresh := 0
		for _, r := range final {
			if k := r.Key() + "\x00" + r.CapturedAt; !seen[k] {
				seen[k] = true
				fresh++
			}
		}
		if fresh > 0 {
			lastNew = t.Now()
		}
		data.Final = len(final)
		// Every tick, not only one with new shots: a set whose push failed is
		// retried on the next.
		if len(final) > 0 {
			if last, err = t.publishRecords(ctx, pass, final, PublishOptions{Areas: o.Areas, Retries: o.Retries}); err != nil {
				return Result{}, err
			}
		}
		fmt.Fprintf(t.Log, "ui-loop follow: tick %d: %d new, %d final, done %v\n", data.Ticks, fresh, len(final), data.Done)
		idle := t.Now().Sub(lastNew)
		if data.Done && idle >= o.UntilIdle {
			break
		}
		if !data.Done && idle >= 3*o.UntilIdle {
			warnings = append(warnings, warn(DiagRunUnfinished, fmt.Sprintf("no done.json for the run of %s and no new shot for %s", run.CreatedAt, idle), fmt.Sprintf("vybava ui-loop run --resume --pass %d, then follow again", pass)))
			break
		}
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		t.Sleep(o.Interval)
	}

	index := PublishIndex{Sets: []PublishedSet{}, Notes: []AreaNotes{}}
	if b, err := os.ReadFile(filepath.Join(passDir, "publish", "index.json")); err == nil {
		if err := json.Unmarshal(b, &index); err != nil {
			return Result{}, err
		}
	}
	data.Sets, data.Notes = index.Sets, index.Notes
	return Result{
		Data:        data,
		Diagnostics: append(last.Diagnostics, warnings...),
		Next:        []string{fmt.Sprintf("vybava ui-loop scoreboard --pass %d --json", pass)},
	}, nil
}
