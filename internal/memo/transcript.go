package memo

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/transcripts"
)

// Citations is what one transcript said about the ledger: bare ids (`#45`,
// `^m45`, `[[LEDGER#^m45]]`), alias-scoped ids (`[[fixit-team/LEDGER#^m12]]`,
// `memo show fixit-team#12`), and every note file the Read tool opened.
type Citations struct {
	Cites     map[int]bool            `json:"cites"` // bare #12, ^m12, [[LEDGER#^m12]]: the personal home
	Team      map[int]bool            `json:"team"`  // #t12, ^t12, [[LEDGER#^t12]]: the team home
	Shows     map[int]bool            `json:"shows"`
	TeamShows map[int]bool            `json:"team_shows"`
	Scoped    map[string]map[int]bool `json:"scoped"` // alias -> ids cited
	Reads     map[string]bool         `json:"reads"`  // absolute paths of Read tool calls
}

func newCitations() Citations {
	return Citations{Cites: map[int]bool{}, Team: map[int]bool{}, Shows: map[int]bool{}, TeamShows: map[int]bool{}, Scoped: map[string]map[int]bool{}, Reads: map[string]bool{}}
}

var (
	hashCiteRE = regexp.MustCompile(`(?:^|[^\w#&/])#(t?)(\d+)\b`)
	blockRefRE = regexp.MustCompile(`\^([mt])(\d+)\b`)
	wikiCiteRE = regexp.MustCompile(`\[\[(?:([a-z0-9]+(?:-[a-z0-9]+)*)/)?LEDGER#\^([mt])(\d+)\]\]`)
	showCmdRE  = regexp.MustCompile(`\bmemo show ((?:[a-z0-9]+(?:-[a-z0-9]+)*)?#?t?\d+)\b`)
)

// transcriptScan is what the Stop hook keeps per transcript between turns:
// where the last sweep stopped and every citation read up to there, so each
// Stop reads only the bytes written since and still credits the whole
// session's citations, exactly as a full rescan would.
type transcriptScan struct {
	Path      string             `json:"path"`
	Cursor    transcripts.Cursor `json:"cursor"`
	Citations Citations          `json:"citations"`
}

// scanSweepBudget bounds one Scan call; harvest loops until nothing is
// pending, so it only sizes how often the prefix is re-verified.
const scanSweepBudget = 64 << 20

// scansPruneAge drops the state of transcripts untouched this long — Claude
// Code deletes transcripts after its 30-day cleanup period.
const scansPruneAge = 35 * 24 * time.Hour

// scansDir is where the per-transcript scan state lives.
func (e Env) scansDir() string {
	return filepath.Join(e.UserHome, ".local", "state", "vybava", "memo", "scans")
}

func (e Env) scanStatePath(transcript string) string {
	sum := sha256.Sum256([]byte(transcript))
	return filepath.Join(e.scansDir(), hex.EncodeToString(sum[:12])+".json")
}

// ScanTranscript collects the citations in a Claude Code transcript's
// assistant text and tool inputs, reading through internal/transcripts from
// where the previous call for the same transcript stopped. A replaced or
// truncated transcript (the cursor's prefix or size no longer fits) is read
// again from byte 0. Malformed lines are skipped; a missing transcript is
// returned as the os.ErrNotExist error.
func (e Env) ScanTranscript(path string) (Citations, error) {
	if _, err := os.Stat(path); err != nil {
		return Citations{}, err
	}
	statePath := e.scanStatePath(path)
	state, known, err := loadScan(statePath, path)
	if err != nil {
		return Citations{}, err
	}
	cites := state.Citations
	cur, changed := state.Cursor, false
	for {
		fresh := newCitations()
		res, err := transcripts.Scan(path, cur, known, transcripts.ScanOptions{
			Budget: scanSweepBudget, RecordLimit: 64 << 20, SkipOversize: true,
		}, func(line []byte, _ int64) error {
			fresh.collectLine(line)
			return nil
		})
		if err != nil {
			return Citations{}, err
		}
		if res.Skipped {
			break // unchanged since the last sweep, or gone between stat and open
		}
		if res.Reset {
			cites = fresh // a different file now: what was read before no longer applies
		} else {
			cites.merge(fresh)
		}
		cur, known, changed = res.Cursor, true, true
		if res.Pending == 0 || res.Read == 0 {
			break
		}
	}
	if changed {
		if err := e.saveScan(statePath, transcriptScan{Path: path, Cursor: cur, Citations: cites}); err != nil {
			return Citations{}, err
		}
	}
	return cites, nil
}

// loadScan reads a transcript's scan state. A missing file, one written for
// another transcript (a hash collision) or one that no longer decodes is no
// state: the transcript is read from byte 0.
func loadScan(statePath, transcript string) (transcriptScan, bool, error) {
	raw, err := os.ReadFile(statePath)
	if errors.Is(err, fs.ErrNotExist) {
		return transcriptScan{Citations: newCitations()}, false, nil
	}
	if err != nil {
		return transcriptScan{}, false, fmt.Errorf("read transcript scan state: %w", err)
	}
	var s transcriptScan
	if json.Unmarshal(raw, &s) != nil || s.Path != transcript {
		return transcriptScan{Citations: newCitations()}, false, nil
	}
	s.Citations.fill()
	return s, true, nil
}

// saveScan writes a transcript's scan state atomically and, at most once a
// day, drops the state of transcripts nobody has scanned for scansPruneAge.
func (e Env) saveScan(statePath string, s transcriptScan) error {
	body, err := json.Marshal(s)
	if err != nil {
		return err
	}
	dir := filepath.Dir(statePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create transcript scan state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".scan-*")
	if err != nil {
		return fmt.Errorf("write transcript scan state: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write transcript scan state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write transcript scan state: %w", err)
	}
	if err := os.Rename(tmp.Name(), statePath); err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("write transcript scan state: %w", err)
	}
	return pruneScans(dir, time.Now())
}

// pruneScans removes scan state older than scansPruneAge; a marker file's
// mtime keeps the directory walk to once a day.
func pruneScans(dir string, now time.Time) error {
	marker := filepath.Join(dir, ".pruned")
	if info, err := os.Stat(marker); err == nil && now.Sub(info.ModTime()) < 24*time.Hour {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("prune transcript scan state: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue // another Stop removed or replaced it
		}
		if err != nil {
			return fmt.Errorf("prune transcript scan state: %w", err)
		}
		if now.Sub(info.ModTime()) > scansPruneAge {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("prune transcript scan state: %w", err)
			}
		}
	}
	return os.WriteFile(marker, nil, 0o600)
}

// collectLine collects the citations of one transcript line; only assistant
// records count, and a line that is not a record is skipped.
func (c *Citations) collectLine(line []byte) {
	rec, err := transcripts.DecodeClaude(line)
	if err != nil || rec.Type != "assistant" {
		return
	}
	for _, text := range rec.Message.Texts() {
		c.collect(text)
	}
	for _, use := range rec.Message.ToolUses() {
		c.collectToolUse(use.Name, use.Input)
	}
}

// merge adds every citation of o to c.
func (c *Citations) merge(o Citations) {
	for _, pair := range []struct{ to, from map[int]bool }{
		{c.Cites, o.Cites}, {c.Team, o.Team}, {c.Shows, o.Shows}, {c.TeamShows, o.TeamShows},
	} {
		for id := range pair.from {
			pair.to[id] = true
		}
	}
	for alias, ids := range o.Scoped {
		for id := range ids {
			c.scoped(alias)[id] = true
		}
	}
	for path := range o.Reads {
		c.Reads[path] = true
	}
}

// fill gives a decoded Citations a map wherever the JSON had none.
func (c *Citations) fill() {
	for _, m := range []*map[int]bool{&c.Cites, &c.Team, &c.Shows, &c.TeamShows} {
		if *m == nil {
			*m = map[int]bool{}
		}
	}
	if c.Scoped == nil {
		c.Scoped = map[string]map[int]bool{}
	}
	if c.Reads == nil {
		c.Reads = map[string]bool{}
	}
}

func (c *Citations) collect(text string) {
	for _, m := range wikiCiteRE.FindAllStringSubmatch(text, -1) {
		id, _ := strconv.Atoi(m[3])
		if m[1] != "" {
			c.scoped(m[1])[id] = true
		} else {
			c.cite(m[2] == "t", id)
		}
	}
	for _, m := range hashCiteRE.FindAllStringSubmatch(text, -1) {
		id, _ := strconv.Atoi(m[2])
		c.cite(m[1] == "t", id)
	}
	for _, m := range blockRefRE.FindAllStringSubmatch(text, -1) {
		id, _ := strconv.Atoi(m[2])
		c.cite(m[1] == "t", id)
	}
}

func (c *Citations) scoped(alias string) map[int]bool {
	if c.Scoped[alias] == nil {
		c.Scoped[alias] = map[int]bool{}
	}
	return c.Scoped[alias]
}

func (c *Citations) collectToolUse(name string, input json.RawMessage) {
	var fields struct {
		FilePath string `json:"file_path"`
		Command  string `json:"command"`
	}
	_ = json.Unmarshal(input, &fields)
	switch name {
	case "Read":
		if strings.HasSuffix(fields.FilePath, ".md") {
			c.Reads[fields.FilePath] = true
		}
	case "Bash":
		for _, m := range showCmdRE.FindAllStringSubmatch(fields.Command, -1) {
			if ref, d := ParseRef(m[1]); d == nil {
				switch {
				case ref.Alias != "":
					c.scoped(ref.Alias)[ref.ID] = true
				case ref.Team:
					c.TeamShows[ref.ID] = true
				default:
					c.Shows[ref.ID] = true
				}
			}
		}
	}
	c.collect(string(input))
}

func (c *Citations) cite(team bool, id int) {
	if team {
		c.Team[id] = true
	} else {
		c.Cites[id] = true
	}
}
