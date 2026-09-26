// Package redact finds secret material leaked into agent conversation history
// — Claude Code sessions, their subagents, workflow journals, persisted tool
// results and background-task output, Codex rollouts, both CLIs' prompt
// history — and overwrites it in place. Detection is internal/secretscan;
// this package owns discovery and the rewrite.
//
// The rewrite is SAME-LENGTH and in place: every span is overwritten with a
// secretscan.Fill of its exact byte length. That is load-bearing, not taste:
//
//   - the files are append-only logs a live session may be writing: a temp
//     file + rename would drop what it appends meanwhile, and a session
//     holding the old inode would keep writing into the unlinked file;
//   - internal/transcripts cursors (tokentime, operator) resume by byte
//     offset: a length change would shift every record after the first leak
//     under them, while a same-length edit past the 256-byte prefix is
//     invisible to them;
//   - nothing else holds the original: no temp copy, no backup.
//
// A span is written only while the file still holds the bytes the scan saw
// there (compare, then write), and only inside a JSON string, whole escapes
// at a time, so a JSONL record stays valid at every instant. The report and
// the audit log carry paths, lines, detectors and masked shapes — never a
// value. docs/redact.md is the operator's guide.
package redact

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/henderson-tech/vybava/internal/secretscan"
	"github.com/henderson-tech/vybava/internal/transcripts"
)

// maxWhole bounds a non-JSONL file read whole, and one JSONL record.
const maxWhole = 256 << 20

// maxListed bounds the findings listed per file; Counts stays complete.
const maxListed = 20

// Finding is one leak, rendered without its value.
type Finding struct {
	Line     int    `json:"line"`
	Detector string `json:"detector"`
	Shape    string `json:"shape"`
}

// File is one file's result. Via lists the symlinks that resolved to it
// (Claude's tasks/<agent>.output points at the subagent transcript).
type File struct {
	Path      string         `json:"path"`
	Via       []string       `json:"via,omitempty"`
	Format    string         `json:"format"`
	Spans     int            `json:"spans"`
	Counts    map[string]int `json:"counts"`
	Findings  []Finding      `json:"findings"`
	Truncated bool           `json:"truncated,omitempty"`
	Redacted  int            `json:"redacted"`
	// Changed counts spans left alone because the bytes moved under the scan
	// (a replaced file); a rerun picks them up.
	Changed int    `json:"changed,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Report is one run.
type Report struct {
	Mode    string `json:"mode"`
	Files   int    `json:"files"`
	Skipped int    `json:"skipped"`
	// Unreadable counts paths Run could not resolve; the caller adds the
	// entries Roots.Files could not read. Non-zero: a clean report is not clean.
	Unreadable int            `json:"unreadable"`
	Known      int            `json:"known"`
	Spans      int            `json:"spans"`
	Redacted   int            `json:"redacted"`
	Changed    int            `json:"changed"`
	Errors     int            `json:"errors"`
	Counts     map[string]int `json:"counts"`
	Leaky      []File         `json:"leaky"`
}

// Options tune one run.
type Options struct {
	Apply bool
	// Classes limits detection (secretscan.Tokens|…); zero means All.
	Classes secretscan.Class
	Known   *secretscan.Known
	// Audit, when set, receives one JSON line per file an apply touched.
	Audit io.Writer
	Now   func() time.Time
	// Workers scan files concurrently; zero means DefaultWorkers.
	Workers int
}

// DefaultWorkers bounds concurrent files: each holds one record (or one
// whole non-JSONL file) in memory, and a history sweep runs beside live work.
const DefaultWorkers = 4

// detect is what one run looks for: the classes and the out-of-band values.
type detect struct {
	classes secretscan.Class
	known   *secretscan.Known
}

func (d detect) find(text string) []secretscan.Span {
	return secretscan.Find(text, cmp.Or(d.classes, secretscan.All), d.known)
}

type patch struct {
	off  int64
	orig []byte
	det  string
}

// Run scans every path (symlinks resolved and deduplicated) and, with Apply,
// overwrites what it finds. A per-file failure is reported on that file and
// the run goes on; a file that vanished since it was listed is skipped.
func Run(paths []string, opts Options) Report {
	rep := Report{Mode: "scan", Counts: map[string]int{}, Known: opts.Known.Len()}
	if opts.Apply {
		rep.Mode = "apply"
	}
	files, skipped, unreadable := resolve(paths)
	rep.Skipped, rep.Unreadable = skipped, unreadable
	results := make([]scanned, len(files))
	next := make(chan int)
	var wg sync.WaitGroup
	for range cmp.Or(opts.Workers, DefaultWorkers) {
		wg.Go(func() {
			for i := range next {
				res := scanFile(files[i].path, detect{classes: opts.Classes, known: opts.Known})
				if res.Error == "" && opts.Apply && len(res.patches) > 0 {
					applyPatches(&res.File, res.patches)
				}
				res.patches = nil
				results[i] = res
			}
		})
	}
	for i := range files {
		next <- i
	}
	close(next)
	wg.Wait()
	for i, res := range results {
		if res.gone {
			rep.Skipped++
			continue
		}
		rep.Files++
		res.Via = files[i].via
		if opts.Audit != nil && res.Redacted > 0 {
			if err := writeAudit(opts, res.File); err != nil && res.Error == "" {
				res.Error = "redacted, but the audit line failed: " + err.Error()
			}
		}
		if res.Spans == 0 && res.Error == "" {
			continue
		}
		rep.Spans += res.Spans
		rep.Redacted += res.Redacted
		rep.Changed += res.Changed
		if res.Error != "" {
			rep.Errors++
		}
		for d, n := range res.Counts {
			rep.Counts[d] += n
		}
		rep.Leaky = append(rep.Leaky, res.File)
	}
	return rep
}

type resolved struct {
	path string
	via  []string
}

// resolve follows symlinks and folds every name for one file into one entry,
// so a file reached twice (tasks/<id>.output → subagents/agent-<id>.jsonl) is
// scanned and written once. Non-regular files and dangling links are skipped;
// any other failure to resolve one is unreadable — counted, never silent.
func resolve(paths []string) (out []resolved, skipped, unreadable int) {
	index := map[string]int{}
	for _, p := range paths {
		real, err := filepath.EvalSymlinks(p)
		var info os.FileInfo
		if err == nil {
			info, err = os.Stat(real)
		}
		switch {
		case errors.Is(err, fs.ErrNotExist):
			skipped++ // vanished, or a dangling link
			continue
		case err != nil:
			unreadable++ // a loop, a permission error: counted, never silent
			continue
		case !info.Mode().IsRegular():
			skipped++
			continue
		}
		i, seen := index[real]
		if !seen {
			i = len(out)
			index[real] = i
			out = append(out, resolved{path: real})
		}
		if p != real && !slicesContains(out[i].via, p) {
			out[i].via = append(out[i].via, p)
		}
	}
	return out, skipped, unreadable
}

func slicesContains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

type scanned struct {
	File
	patches []patch
	gone    bool // vanished after it was listed
}

func formatOf(path string) string {
	switch {
	case strings.HasSuffix(path, ".jsonl"):
		return "jsonl"
	case strings.HasSuffix(path, ".json"):
		return "json"
	default:
		return "text"
	}
}

func scanFile(path string, d detect) scanned {
	res := scanned{File: File{Path: path, Format: formatOf(path), Counts: map[string]int{}}}
	var err error
	if res.Format == "jsonl" {
		err = scanJSONL(&res, d)
	} else {
		err = scanWhole(&res, d)
	}
	switch {
	case errors.Is(err, os.ErrNotExist):
		res.gone = true
	case err != nil:
		res.Error = err.Error()
	}
	return res
}

// scanJSONL reads complete records through the transcripts cursor — an
// unfinished last record is left for its writer, never half-read.
func scanJSONL(res *scanned, d detect) error {
	line := 0
	sweep, err := transcripts.Scan(res.Path, transcripts.Cursor{}, false, transcripts.ScanOptions{
		Budget: math.MaxInt64, RecordLimit: maxWhole,
	}, func(record []byte, offset int64) error {
		line++
		scanRecord(res, record, offset, line, d)
		return nil
	})
	if errors.Is(err, transcripts.ErrRecordTooLarge) {
		return fmt.Errorf("a record past %d MiB was not scanned", maxWhole>>20)
	}
	if err == nil && sweep.Skipped {
		return os.ErrNotExist // resolve kept regular files only: it vanished
	}
	return err
}

func scanWhole(res *scanned, d detect) error {
	info, err := os.Stat(res.Path)
	if err != nil {
		return err
	}
	if info.Size() > maxWhole {
		return fmt.Errorf("larger than %d MiB, not scanned", maxWhole>>20)
	}
	data, err := os.ReadFile(res.Path)
	if err != nil {
		return err
	}
	if bytes.IndexByte(data[:min(len(data), 8192)], 0) >= 0 {
		return nil // binary: nothing a transcript reader would show
	}
	if res.Format == "json" && json.Valid(data) {
		scanJSON(res, data, 0, 1, d)
		return nil
	}
	scanText(res, data, 0, 1, d)
	return nil
}

// scanRecord scans one JSONL record: a valid one string by string, anything
// else as raw text (masking a broken line leaves it exactly as broken).
func scanRecord(res *scanned, record []byte, offset int64, line int, d detect) {
	body := bytes.TrimRight(record, "\r\n")
	if json.Valid(body) {
		scanJSON(res, body, offset, line, d)
		return
	}
	scanText(res, body, offset, line, d)
}

func scanJSON(res *scanned, doc []byte, offset int64, line int, d detect) {
	newlines, counted := 0, 0
	eachString(doc, func(start, end int) {
		raw := doc[start:end]
		value := decode(raw)
		spans := d.find(value)
		if len(spans) == 0 {
			return
		}
		// A pretty-printed .json spans lines; a JSONL record is one.
		newlines += bytes.Count(doc[counted:start], []byte{'\n'})
		counted = start
		for i, rr := range rawSpans(raw, spans) {
			record(res, doc, start+rr[0], start+rr[1], offset, line+newlines, spans[i].Detector, secretscan.Shape(value, spans[i]))
		}
	})
}

func scanText(res *scanned, data []byte, offset int64, line int, d detect) {
	text := string(data)
	counted := 0
	for _, s := range d.find(text) {
		line += strings.Count(text[counted:s.Start], "\n")
		counted = s.Start
		record(res, data, s.Start, s.End, offset, line, s.Detector, secretscan.Shape(text, s))
	}
}

func record(res *scanned, buf []byte, start, end int, offset int64, line int, det, shape string) {
	res.Spans++
	res.Counts[det]++
	if len(res.Findings) < maxListed {
		res.Findings = append(res.Findings, Finding{Line: line, Detector: det, Shape: shape})
	} else {
		res.Truncated = true
	}
	res.patches = append(res.patches, patch{off: offset + int64(start), orig: bytes.Clone(buf[start:end]), det: det})
}

// applyPatches overwrites each span that still holds the scanned bytes, then
// restores the modification time when nothing else wrote meanwhile, so an
// old session keeps its place in `claude --resume`.
func applyPatches(f *File, patches []patch) {
	before, err := os.Stat(f.Path)
	if err != nil {
		f.Error = err.Error()
		return
	}
	fh, err := os.OpenFile(f.Path, os.O_RDWR, 0)
	if err != nil {
		f.Error = err.Error()
		return
	}
	sort.Slice(patches, func(i, j int) bool { return patches[i].off < patches[j].off })
	cur := make([]byte, 0, 256)
	for _, p := range patches {
		cur = cur[:len(p.orig)]
		if _, err := fh.ReadAt(cur, p.off); err != nil || !bytes.Equal(cur, p.orig) {
			f.Changed++
			continue
		}
		if _, err := fh.WriteAt(secretscan.Fill(p.det, len(p.orig)), p.off); err != nil {
			f.Error = err.Error()
			break
		}
		f.Redacted++
	}
	if err := fh.Sync(); err != nil && f.Error == "" {
		f.Error = err.Error()
	}
	if err := fh.Close(); err != nil && f.Error == "" {
		f.Error = err.Error()
	}
	after, err := os.Stat(f.Path)
	if err == nil && after.Size() == before.Size() {
		err = os.Chtimes(f.Path, before.ModTime(), before.ModTime())
	}
	if err != nil && f.Error == "" {
		f.Error = "redacted, but the modification time was not restored: " + err.Error()
	}
}

type auditLine struct {
	Time     time.Time      `json:"time"`
	Path     string         `json:"path"`
	Redacted int            `json:"redacted"`
	Counts   map[string]int `json:"counts"`
}

func writeAudit(opts Options, f File) error {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	line, err := json.Marshal(auditLine{Time: now().UTC(), Path: f.Path, Redacted: f.Redacted, Counts: f.Counts})
	if err != nil {
		return err
	}
	_, err = opts.Audit.Write(append(line, '\n'))
	return err
}
