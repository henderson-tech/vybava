package tokentime

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/transcripts"
)

// Options configure one index pass.
type Options struct {
	// ClaudeRoot is Claude Code's projects directory (~/.claude/projects).
	ClaudeRoot string
	// CodexDir is the Codex directory holding sessions/ (~/.codex).
	CodexDir string
	// SideDirs hold work products no project owns — a deliverables or
	// backups home (~/Exports, ~/Backups): like the agents' own state, a
	// write there never moves where an AI minute goes (see focus.go).
	SideDirs []string
	// Budget bounds the bytes read this pass; zero reads everything pending.
	Budget int64
	// Now overrides the clock (tests).
	Now func() time.Time
	// Context stops a pass between files and between sweeps of one file;
	// everything read before that is committed.
	Context context.Context

	// commitFiles overrides how many files a commit waits for (tests).
	commitFiles int
	// killAfter stops the pass dead after that many files, skipping the
	// final commit — what SIGKILL does (tests).
	killAfter int
}

// Commit cadence: a pass killed at any moment keeps everything committed
// before it, so progress is committed in bounded chunks, never once per pass.
const (
	commitBytes = 16 * 1024 * 1024 // read since the last commit, also mid-file
	commitFiles = 256              // files touched since the last commit
	commitEvery = time.Second      // wall time since the last commit
)

// errKilled is what a test-simulated SIGKILL returns.
var errKilled = errors.New("tokentime: pass killed (test)")

// IndexReport summarises one pass.
type IndexReport struct {
	Files     int   `json:"files"`
	Opened    int   `json:"opened"`
	ReadBytes int64 `json:"readBytes"`
	// PendingBytes is what a later pass can still read: complete records the
	// budget left behind. Tails and errored files are reported apart.
	PendingBytes int64 `json:"pendingBytes"`
	// StaleTails are files ending in an unterminated record older than
	// staleTail — a writer that died mid-line; no pass can ever read it.
	StaleTails     []string `json:"staleTails"`
	StaleTailBytes int64    `json:"staleTailBytes"`
	// ErroredBytes are the unread bytes of files that failed this pass.
	ErroredBytes int64 `json:"erroredBytes"`
	// BeatsPendingBytes is the beats backlog still unread: bytes passes read
	// before beats existed, or before AI minutes followed the focus, owed one
	// more read (beats only).
	BeatsPendingBytes int64 `json:"beatsPendingBytes"`
	// PointsPendingBytes is the limit-points backlog still unread: rollout
	// bytes passes read before points existed, owed one more read (points only).
	PointsPendingBytes int64 `json:"pointsPendingBytes"`
	// Points are the limit points this pass added; copies already kept are not.
	Points       int64    `json:"points"`
	Responses    int64    `json:"responses"`
	Duplicates   int64    `json:"duplicates"`
	Resets       int      `json:"resets"`
	DecodeErrors int64    `json:"decodeErrors"`
	FileErrors   []string `json:"fileErrors"`
	DurationMs   int64    `json:"durationMs"`
}

// ErrBusy means another pass holds the index lock.
var ErrBusy = errors.New("another tokentime index pass is running")

// sweepBytes bounds one Scan call inside a file.
const sweepBytes = 16 * transcripts.DefaultBudget

// UnknownCodexModel names Codex usage seen before any turn_context.
const UnknownCodexModel = "codex-unknown"

type fileRow struct {
	cur   transcripts.Cursor
	state string
	// tail: the bytes after cur are an unterminated last record.
	tail bool
	// beats is the file's beats backlog (a beatsLag); '' = read before beats existed.
	beats string
	// points is a rollout's limit-points backlog (a pointsLag); '' = read
	// before points existed. Only the points backlog writes it.
	points string
	// focus is the file's focus debt (a beatsLag, see focusOf); '' = read
	// before AI minutes followed the focus. Only its first read inserts it and
	// only focusDebts move it.
	focus string
}

// beatsLag is a file's beats backlog: bytes [Cursor.Offset, Until) were read
// before this store recorded beats. Everything a pass reads from now on
// records its beats as it goes, so only this region is ever read once more —
// beats only, charging nothing.
type beatsLag struct {
	Cursor transcripts.Cursor `json:"cursor"`
	Until  int64              `json:"until"`
	// State is a rollout's parse state at Cursor.
	State *codexState `json:"state,omitempty"`
	// Written is the last write (unix nanoseconds) of the content whose beats
	// the debt owes, recorded when the debt opens: what a lost debt moves
	// coverage past, however many passes later the loss is found. The token
	// cursor cannot stand in for it, since a rewrite moves that to today. A
	// debt an older build encoded without it takes it on decode (lagOf).
	Written int64 `json:"written,omitempty"`
}

// beatsDone is the stored backlog of a file whose every read byte has its beats.
const beatsDone = "{}"

func (l beatsLag) done() bool { return l.Cursor.Offset >= l.Until }

func (l beatsLag) encode() string {
	if l.done() {
		return beatsDone
	}
	raw, _ := json.Marshal(l)
	return string(raw)
}

// lagOf reads a stored backlog. A file read before beats existed (empty) owes
// everything up to where its stored token cursor stands, written by the time
// that cursor saw; beats are idempotent, so an unreadable backlog is owed
// again from byte 0 rather than lost. An open debt an older build encoded
// without Written takes the stored cursor's the same way, and keeps it once
// saved, before a later rewrite can move that cursor.
func lagOf(stored string, cur transcripts.Cursor) beatsLag {
	var l beatsLag
	if stored == "" || json.Unmarshal([]byte(stored), &l) != nil {
		return beatsLag{Until: cur.Offset, Written: cur.Modified}
	}
	if l.Written == 0 && !l.done() {
		l.Written = cur.Modified
	}
	return l
}

// pointsLag is a rollout's limit-points backlog: bytes [Cursor.Offset, Until)
// were read before this store recorded limit points. It is a beatsLag without
// Written: no answer vouches for points being complete, so a debt lost with
// its file moves nothing.
type pointsLag = beatsLag

// pointsDone is the stored backlog of a rollout whose every read byte has its
// points — also what a file first read after points existed is inserted with.
const pointsDone = beatsDone

// pointsOf reads a stored points backlog. A rollout read before points
// existed (an empty column) owes everything up to where its stored cursor
// stood when this pass began — what the pass reads after it records its
// points as it goes. Points are idempotent, so an unreadable backlog is owed
// again from byte 0.
func pointsOf(stored string, cur transcripts.Cursor) pointsLag {
	var l pointsLag
	if stored == "" || json.Unmarshal([]byte(stored), &l) != nil {
		return pointsLag{Until: cur.Offset}
	}
	return l
}

// staleTail is how long an unterminated last record may wait for its writer
// before it is reported instead of counted as pending.
const staleTail = 10 * time.Minute

type target struct {
	path  string
	codex bool
	info  os.FileInfo
}

type bucketKey struct {
	hour  int64
	root  string
	model string
}

type bucketVal struct {
	lane Lane
	c    Counts
}

type beatKey struct {
	minute int64
	root   string
	kind   int
}

type spanKey struct {
	session string
	hour    int64
	root    string
}

type span struct{ first, last int64 }

type seenRow struct {
	src int
	day int64
}

// codexState is the per-rollout parse state persisted beside its cursor, so a
// sweep boundary never loses the owner, the model or the legacy baseline.
type codexState struct {
	Owner      string                  `json:"owner,omitempty"`
	Created    int64                   `json:"created,omitempty"`
	Cwd        string                  `json:"cwd,omitempty"`
	Model      string                  `json:"model,omitempty"`
	Receipts   bool                    `json:"receipts,omitempty"`
	Prev       *transcripts.CodexUsage `json:"prev,omitempty"`
	LastLegacy *transcripts.CodexUsage `json:"lastLegacy,omitempty"`
	// Human: a person drives the thread (its owner header says so). Nil in
	// state saved before beats existed.
	Human *bool `json:"human,omitempty"`
	// Staged: LastLegacy's minute went into the focus staging, so the receipt
	// it is charged as is found with it.
	Staged bool `json:"staged,omitempty"`
	// Focus is where the agent works — the one field a Claude transcript's
	// state carries too.
	Focus focusState `json:"focus,omitzero"`
}

// readMode is what one read of a rollout's bytes records.
type readMode int

const (
	// readTokens is the forward read: it charges each response and records
	// its beats and every limit point.
	readTokens readMode = iota
	// readBeats is the beats backlog: beats only (see catchUp).
	readBeats
	// readPoints is the points backlog: limit points only (see pointsBacklog).
	readPoints
	// readFocus re-reads history under the focus rule: AI minutes into the
	// staging only (see focusBacklog).
	readFocus
)

func (m readMode) String() string {
	switch m {
	case readBeats:
		return "beats"
	case readPoints:
		return "points"
	case readFocus:
		return "focus"
	}
	return "tokens"
}

type indexer struct {
	s       *Store
	ctx     context.Context
	now     time.Time
	report  IndexReport
	buckets map[bucketKey]*bucketVal
	spans   map[spanKey]*span
	beats   map[beatKey]struct{}
	points  map[int64]limitPoint
	newSeen map[int64]seenRow
	files   map[string]fileRow
	// debts are the points backlogs the points backlog moved, by path: its
	// own column, so no other writer of a file row can put an older one back.
	debts map[string]string
	// read is every file row a token read left this pass; commits never clear it.
	read     map[string]fileRow
	rootMemo map[string]string
	newRoots map[string]string
	projects map[string]int64
	sessions map[string]int64
	dirty    int64
	seenStmt *sql.Stmt

	// lost is every row whose backlog outran its shrunk file this pass, and
	// lostSince the latest last write among them. Only the final commit writes
	// them, with the coverage move: a pass killed before it pays nothing.
	lost      map[string]fileRow
	lostSince int64

	// staging: history read before the focus rule is being re-attributed
	// (meta focus_rule unset). Every AI minute then also lands in staged and
	// its response in found, until settleFocus swaps them in.
	staging bool
	staged  map[stageKey]struct{}
	found   map[foundKey]struct{}
	// focusDebts are the focus debts moved this pass, by path (files.focus).
	focusDebts map[string]string
	// settleFocus: the final commit settles the re-attribution.
	settleFocus bool
	repoMemo    map[string]repoAnswer
	spelled     map[string]string // a touched root → its on-disk case
	// sideDirs hold the agents' own state and Options.SideDirs, where a write
	// is no project's own work.
	sideDirs []string
}

// Index reads everything written since the last pass into the buckets.
func (s *Store) Index(opts Options) (IndexReport, error) {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	started := time.Now()
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return IndexReport{}, err
	}
	unlock, err := tryLock(filepath.Join(s.Dir, "index.lock"))
	if err != nil {
		return IndexReport{}, err
	}
	defer unlock()
	if err := s.prepare(); err != nil {
		return IndexReport{}, err
	}

	known, err := s.loadFiles()
	if err != nil {
		return IndexReport{}, err
	}
	targets, walked, err := discover(opts)
	if err != nil {
		return IndexReport{}, err
	}
	ix, err := s.newIndexer()
	if err != nil {
		return IndexReport{}, err
	}
	defer ix.seenStmt.Close()
	rule, err := meta(s.db, "focus_rule")
	if err != nil {
		return IndexReport{}, err
	}
	ix.staging = rule != focusRule
	ix.sideDirs = []string{filepath.Clean(opts.CodexDir)}
	for _, d := range opts.SideDirs {
		if d != "" {
			ix.sideDirs = append(ix.sideDirs, filepath.Clean(d))
		}
	}
	if filepath.Base(opts.ClaudeRoot) == "projects" { // ~/.claude/projects: plans, memory and handoffs sit beside it
		ix.sideDirs = append(ix.sideDirs, filepath.Dir(filepath.Clean(opts.ClaudeRoot)))
	}
	// Files already under a cursor first — live sessions append there — then
	// new files newest-first, so a cold backfill under a budget fills today
	// before history. Order never changes totals: dedupe is by identity.
	sort.SliceStable(targets, func(i, j int) bool {
		_, ki := known[targets[i].path]
		_, kj := known[targets[j].path]
		if ki != kj {
			return ki
		}
		return targets[i].info.ModTime().After(targets[j].info.ModTime())
	})
	ix.report.Files = len(targets)
	ix.report.FileErrors, ix.report.StaleTails = []string{}, []string{}
	ix.now = now()
	ctx := opts.Context
	if ctx == nil {
		ctx = context.Background()
	}
	ix.ctx = ctx
	perCommit := commitFiles
	if opts.commitFiles > 0 {
		perCommit = opts.commitFiles
	}
	touched, sinceCommit, lastCommit := 0, 0, time.Now()
	for _, t := range targets {
		row, ok := known[t.path]
		if ctx.Err() != nil {
			// Stopped: what is left is pending, what was read gets committed.
			ix.report.PendingBytes += max(0, t.info.Size()-row.cur.Offset)
			continue
		}
		if ok && row.cur.Unchanged(t.info) {
			ix.settle(t, row)
			continue
		}
		if ok && row.tail && row.cur.Size == t.info.Size() && row.cur.Modified == t.info.ModTime().UnixNano() {
			ix.tail(t, row.cur) // nothing appended: the same unterminated tail
			ix.settle(t, row)
			continue
		}
		remaining := int64(0)
		if opts.Budget > 0 {
			remaining = opts.Budget - ix.report.ReadBytes
			if remaining <= 0 {
				ix.report.PendingBytes += max(0, t.info.Size()-row.cur.Offset)
				continue
			}
		}
		if err := ix.file(t, row, ok, remaining); err != nil {
			return IndexReport{}, err
		}
		touched++
		sinceCommit++
		if opts.killAfter > 0 && touched == opts.killAfter {
			return IndexReport{}, errKilled
		}
		if ix.dirty >= commitBytes || sinceCommit >= perCommit || time.Since(lastCommit) >= commitEvery {
			if err := ix.flush(); err != nil {
				return IndexReport{}, err
			}
			sinceCommit, lastCommit = 0, time.Now()
		}
	}
	if err := ix.backlog(targets, known, opts.Budget, perCommit); err != nil {
		return IndexReport{}, err
	}
	refocused, err := ix.focusBacklog(targets, known, opts.Budget, perCommit)
	if err != nil {
		return IndexReport{}, err
	}
	if err := ix.pointsBacklog(targets, known, opts.Budget, perCommit); err != nil {
		return IndexReport{}, err
	}
	since, err := meta(s.db, "beats_since")
	if err != nil {
		return IndexReport{}, err
	}
	tx, err := ix.begin()
	if err != nil {
		return IndexReport{}, err
	}
	// Beats are complete from the day after beatsSince (unix seconds).
	beatsSince, _ := strconv.ParseInt(since, 10, 64)
	if since == "" {
		// Claude Code deletes transcripts: the oldest still on disk when beats
		// began bounds how far back its beats can be complete. Rollouts are
		// kept, so they never bound it.
		beatsSince = oldestClaude(targets)
	}
	// A file that shrank under its backlog lost beats like a vanished one.
	beatsSince = max(beatsSince, ix.lostSince)
	maps.Copy(ix.files, ix.lost)
	if walked {
		// Cursors of vanished files go; their buckets and beats stay forever.
		present := make(map[string]bool, len(targets))
		for _, t := range targets {
			present[t.path] = true
		}
		for path, row := range known {
			if !present[path] {
				if lag := lagOf(row.beats, row.cur); row.beats != beatsDone && !lag.done() {
					// Gone with beats unread: no day up to the last write of the
					// content it owed for is complete any more. Its cursor may
					// already be a rewrite's, so the debt's Written bounds it.
					beatsSince = max(beatsSince, lag.Written/int64(time.Second))
				}
				if _, err := tx.Exec("DELETE FROM files WHERE path = ?", path); err != nil {
					tx.Rollback()
					return IndexReport{}, err
				}
			}
		}
	}
	if err := setMeta(tx, "pending_bytes", strconv.FormatInt(ix.report.PendingBytes, 10)); err != nil {
		tx.Rollback()
		return IndexReport{}, err
	}
	if err := setMeta(tx, "beats_pending_bytes", strconv.FormatInt(ix.report.BeatsPendingBytes, 10)); err != nil {
		tx.Rollback()
		return IndexReport{}, err
	}
	if err := setMeta(tx, "points_pending_bytes", strconv.FormatInt(ix.report.PointsPendingBytes, 10)); err != nil {
		tx.Rollback()
		return IndexReport{}, err
	}
	if next := strconv.FormatInt(beatsSince, 10); next != since {
		if err := setMeta(tx, "beats_since", next); err != nil {
			tx.Rollback()
			return IndexReport{}, err
		}
	}
	if err := setMeta(tx, "last_index_at", now().UTC().Format(time.RFC3339)); err != nil {
		tx.Rollback()
		return IndexReport{}, err
	}
	// Every file on disk re-read: the re-attribution settles with this commit.
	if ix.settleFocus = refocused; refocused {
		ix.pendingFound(targets, known)
	}
	if err := ix.commit(tx); err != nil {
		return IndexReport{}, err
	}
	ix.report.DurationMs = time.Since(started).Milliseconds()
	return ix.report, ctx.Err() // stopped early: progress kept, the caller learns why
}

// discover lists every transcript and rollout. walked is false when a root
// could not be listed, so vanished-file cleanup must not run.
func discover(opts Options) ([]target, bool, error) {
	var targets []target
	walked := true
	claude, skipped, err := transcripts.WalkClaude(opts.ClaudeRoot)
	if err != nil {
		return nil, false, fmt.Errorf("list %s: %w", opts.ClaudeRoot, err)
	}
	if skipped > 0 {
		walked = false // an unseen file is not a vanished one
	}
	for _, f := range claude {
		targets = append(targets, target{path: f.Path, info: f.Info})
	}
	rollouts, err := transcripts.RolloutPathsIn(opts.CodexDir, time.Time{})
	if err != nil {
		return nil, false, fmt.Errorf("list %s: %w", opts.CodexDir, err)
	}
	for _, path := range rollouts {
		info, err := os.Lstat(path)
		if err != nil {
			walked = false // raced a move; keep its cursor until it settles
			continue
		}
		targets = append(targets, target{path: path, codex: true, info: info})
	}
	return targets, walked, nil
}

func (s *Store) loadFiles() (map[string]fileRow, error) {
	rows, err := s.db.Query("SELECT path, cursor, state, tail, beats, points, focus FROM files")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	known := map[string]fileRow{}
	for rows.Next() {
		var path, cursor, state, beats, points, focus string
		var tail bool
		if err := rows.Scan(&path, &cursor, &state, &tail, &beats, &points, &focus); err != nil {
			return nil, err
		}
		var cur transcripts.Cursor
		if err := json.Unmarshal([]byte(cursor), &cur); err != nil {
			continue // unreadable cursor: re-read the file; identities stop double counting
		}
		known[path] = fileRow{cur: cur, state: state, tail: tail, beats: beats, points: points, focus: focus}
	}
	return known, rows.Err()
}

func (s *Store) newIndexer() (*indexer, error) {
	ix := &indexer{
		s: s, rootMemo: map[string]string{}, projects: map[string]int64{}, sessions: map[string]int64{}, read: map[string]fileRow{}, lost: map[string]fileRow{},
		repoMemo: map[string]repoAnswer{}, spelled: map[string]string{},
	}
	ix.reset()
	rows, err := s.db.Query("SELECT cwd, root FROM roots")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var cwd, root string
		if err := rows.Scan(&cwd, &root); err != nil {
			rows.Close()
			return nil, err
		}
		ix.rootMemo[cwd] = root
	}
	rows.Close()
	if ix.seenStmt, err = s.db.Prepare("SELECT 1 FROM seen WHERE id = ?"); err != nil {
		return nil, err
	}
	return ix, nil
}

func (ix *indexer) reset() {
	ix.buckets = map[bucketKey]*bucketVal{}
	ix.spans = map[spanKey]*span{}
	ix.beats = map[beatKey]struct{}{}
	ix.points = map[int64]limitPoint{}
	ix.newSeen = map[int64]seenRow{}
	ix.files = map[string]fileRow{}
	ix.debts = map[string]string{}
	ix.newRoots = map[string]string{}
	ix.staged = map[stageKey]struct{}{}
	ix.found = map[foundKey]struct{}{}
	ix.focusDebts = map[string]string{}
	ix.dirty = 0
}

// file reads one transcript or rollout until it is caught up, the pass budget
// runs out or the pass is stopped. A long file commits between sweeps.
func (ix *indexer) file(t target, row fileRow, known bool, budget int64) error {
	cur := row.cur
	var cs codexState
	if row.state != "" {
		_ = json.Unmarshal([]byte(row.state), &cs)
	}
	parse := ix.claudeLine(&cs, readTokens)
	if t.codex {
		parse = ix.codexLine(&cs, readTokens)
	}
	var read, pending, unsaved int64
	opened, tail, fresh := false, false, !known
	save := func() {
		raw, _ := json.Marshal(cs)
		state := string(raw)
		// Never read before: every minute it records follows the focus. One
		// replaced or re-read while history awaits the re-read keeps its debt:
		// what this read finds seen it stages as copies (copyFound).
		focus := row.focus
		if fresh {
			focus = beatsDone
		}
		lag := lagOf(row.beats, row.cur)
		switch {
		case fresh:
			// Never read before: every response it charges records its minute.
			lag = beatsLag{}
		case !lag.done() || t.codex && cs.Human == nil:
			// An open debt tracks the token cursor: a file replaced or re-read
			// while it owes beats holds responses charged before beats existed,
			// which this read finds seen and records no minute for. State from
			// before beats cannot tell a person's prompt from a spawned
			// thread's until the backlog reads the owner header either.
			lag.Until = max(lag.Until, cur.Offset)
			if t.codex && cs.Human == nil {
				// Its prompts went unrecorded, so the debt now owes what this
				// read saw too. Any other read records the beats of what it
				// reads, and Written stays with the content the debt opened on.
				lag.Written = max(lag.Written, cur.Modified)
			}
		}
		ix.files[t.path] = fileRow{cur: cur, state: state, tail: tail, beats: lag.encode(), focus: focus}
		ix.read[t.path] = ix.files[t.path]
		ix.dirty += unsaved
		unsaved = 0
	}
	for {
		sweep := int64(sweepBytes)
		if budget > 0 {
			sweep = min(sweep, budget-read)
		}
		res, err := transcripts.Scan(t.path, cur, known, transcripts.ScanOptions{Budget: sweep, SkipOversize: true}, parse)
		if err != nil {
			// Retried next pass, but never counted as pending: a file that keeps
			// failing must not hold coverage incomplete forever.
			ix.report.FileErrors = append(ix.report.FileErrors, fmt.Sprintf("%s: %v", t.path, err))
			ix.report.ErroredBytes += max(0, t.info.Size()-cur.Offset)
			ix.report.ReadBytes += read
			return nil
		}
		if res.Skipped {
			return nil
		}
		opened = true
		if res.Reset {
			ix.report.Resets++
		}
		cur, known = res.Cursor, true
		read += res.Read
		unsaved += res.Read
		pending = res.Pending
		// A sweep that stopped short of its budget hit the end of the file: what
		// is left is an unterminated record, not unread work.
		tail = pending > 0 && res.Read < sweep
		if pending == 0 || res.Read == 0 || (budget > 0 && read >= budget) || ix.ctx.Err() != nil {
			break
		}
		if ix.dirty+unsaved >= commitBytes {
			save()
			if err := ix.flush(); err != nil {
				return err
			}
		}
	}
	if opened {
		ix.report.Opened++
	}
	ix.report.ReadBytes += read
	if tail {
		ix.tail(t, cur)
	} else {
		ix.report.PendingBytes += pending
	}
	if !t.codex && ix.whole(cs.Focus.Msg) {
		ix.flushMsg(&cs.Focus, readTokens)
	}
	save()
	return nil
}

// tail classifies an unterminated last record: a fresh one is a writer still
// at work and simply waits; one older than staleTail is reported.
func (ix *indexer) tail(t target, cur transcripts.Cursor) {
	if ix.now.Sub(t.info.ModTime()) < staleTail {
		return
	}
	ix.report.StaleTails = append(ix.report.StaleTails, t.path)
	ix.report.StaleTailBytes += max(0, t.info.Size()-cur.Offset)
}

// claudeLine parses one transcript record: it records the record's beat and,
// with tokens, charges its response. A backlog read records beats only, the
// seen identities neither read nor written; mode says which. A message's AI
// minute waits in cs.Focus until its tool calls are in (see focusState).
func (ix *indexer) claudeLine(cs *codexState, mode readMode) func([]byte, int64) error {
	f := &cs.Focus
	return func(line []byte, offset int64) error {
		if offset == 0 {
			// First read, or the file was replaced: a message the old content
			// left pending is whole, and charged — the new one finds it seen.
			ix.flushMsg(f, mode)
			*cs = codexState{}
		}
		if transcripts.ClaudeHumanLine(line) {
			if rec, err := transcripts.DecodeClaude(line); err == nil && rec.HumanPrompt() {
				ix.flushMsg(f, mode)
				f.rebase(ix.root(rec.Cwd)) // a person's prompt: the focus is the cwd again
				if mode != readFocus {
					ix.beat(rec.Timestamp, rec.Cwd, beatHuman)
				}
				return nil
			}
		}
		if !transcripts.ClaudeUsageLine(line) {
			return nil
		}
		rec, err := transcripts.DecodeClaude(line)
		if err != nil {
			ix.report.DecodeErrors++
			return nil
		}
		u, model := rec.Message.Usage, rec.Message.Model
		if rec.Type != "assistant" || u == nil || model == "" || model == transcripts.SyntheticModel || rec.Timestamp.IsZero() {
			return nil
		}
		id := firstNonEmpty(rec.Message.ID, rec.RequestID, rec.UUID)
		if id == "" {
			return nil
		}
		ix.syncBase(f, rec.Cwd, mode)
		// Repeats of one message sit next to each other, one per content block.
		if f.Msg == nil || f.Msg.ID != id {
			ix.flushMsg(f, mode)
			// A copy charges nothing: its minute is the charged original's.
			// The backlog cannot tell one (see catchUp).
			beat := mode != readTokens || !ix.seen(identity("claude", id), srcClaude, rec.Timestamp)
			if beat && mode == readTokens {
				w5, w1 := u.CacheWrites()
				c := Counts{Input: u.InputTokens, Output: u.OutputTokens, CacheWrite5m: w5, CacheWrite1h: w1, CacheRead: u.CacheReadInputTokens, Responses: 1}
				ix.add(rec.Timestamp, rec.Cwd, model, LaneOf(model, Anthropic), c, "claude:"+rec.SessionID)
			}
			f.Msg = &claudeMsg{ID: id, At: rec.Timestamp.Unix(), Beat: beat, Copy: !beat && ix.staging}
		}
		if bytes.Contains(line, []byte(`"tool_use"`)) {
			for _, use := range rec.Message.ToolUses() {
				ix.touch(f, claudeWrites(use, rec.Cwd))
			}
		}
		return nil
	}
}

// codexLine is claudeLine for a rollout, threading its parse state. Its mode
// says what the read records: see readMode.
func (ix *indexer) codexLine(cs *codexState, mode readMode) func([]byte, int64) error {
	tokens := mode == readTokens
	return func(line []byte, offset int64) error {
		if offset == 0 {
			*cs = codexState{} // first read, or the file was replaced
		}
		if !transcripts.RolloutUsageLine(line) && (mode == readPoints || !transcripts.RolloutUserLine(line) && !transcripts.RolloutCallLine(line)) {
			return nil // a prompt is a beat, a call moves the focus; neither is a point
		}
		var entry transcripts.RolloutLine
		if err := json.Unmarshal(line, &entry); err != nil {
			ix.report.DecodeErrors++
			return nil
		}
		ts, _ := time.Parse(time.RFC3339Nano, entry.Timestamp)
		switch entry.Type {
		case transcripts.RolloutSessionMeta:
			// The first header owns the rollout; forks then copy ancestor headers.
			var meta transcripts.SessionMeta
			if cs.Owner != "" || json.Unmarshal(entry.Payload, &meta) != nil || meta.ID == "" {
				return nil
			}
			cs.Owner, cs.Cwd = meta.ID, meta.CWD
			human := meta.Interactive()
			cs.Human = &human
			created := ts
			if at, err := time.Parse(time.RFC3339Nano, meta.Timestamp); err == nil {
				created = at
			}
			cs.Created = created.UnixMilli()
			ix.syncBase(&cs.Focus, cs.Cwd, mode)
		case transcripts.RolloutTurnContext:
			var tc transcripts.TurnContext
			if json.Unmarshal(entry.Payload, &tc) != nil {
				return nil
			}
			if tc.Model != "" {
				cs.Model = tc.Model
			}
			if tc.CWD != "" {
				cs.Cwd = tc.CWD
				ix.syncBase(&cs.Focus, cs.Cwd, mode)
			}
		case transcripts.RolloutResponseItem:
			var it transcripts.ResponseItem
			if mode == readPoints || cs.Owner == "" || ts.UnixMilli() < cs.Created || json.Unmarshal(entry.Payload, &it) != nil {
				return nil // copied fork history wrote in its ancestor's time
			}
			if it.Type == transcripts.ItemFunctionCall || it.Type == transcripts.ItemCustomCall {
				// A response's calls precede its token_count and receipt.
				ix.syncBase(&cs.Focus, cs.Cwd, mode)
				ix.touch(&cs.Focus, codexWrites(it, cs.Cwd))
			}
		case transcripts.RolloutUsageRecord:
			var r transcripts.UsageRecord
			if mode == readPoints || json.Unmarshal(entry.Payload, &r) != nil || cs.Owner == "" || r.ThreadID != cs.Owner ||
				r.ResponseID == "" || !r.Usage.Valid() || ts.IsZero() || ts.UnixMilli() < cs.Created {
				return nil // copied fork history keeps its original owner's thread id
			}
			first := !cs.Receipts
			cs.Receipts = true
			key := identity("codex-receipt", cs.Owner, r.ResponseID)
			if first && cs.LastLegacy != nil && sameUsage(*cs.LastLegacy, r.Usage) {
				// The producer persisted this response's token_count first; it
				// is already charged — and beat. Remember the receipt, charge
				// nothing; it is found where its token_count's minute is.
				if tokens {
					ix.seen(key, srcCodex, ts)
				}
				if ix.staging && cs.Staged {
					ix.found[foundKey{id: key, day: ts.Unix() / 86400}] = struct{}{}
				}
				cs.Focus.Touched = nil
				return nil
			}
			if tokens && ix.seen(key, srcCodex, ts) {
				ix.codexCopy(ts, cs, key)
				return nil
			}
			ix.codexBeat(ts, cs, key, mode) // a backlog read records every one: see catchUp
			if tokens {
				ix.chargeCodex(ts, cs, r.Usage)
			}
		case transcripts.RolloutEventMsg:
			if mode != readPoints && transcripts.RolloutUserLine(line) {
				var h transcripts.EventHeader
				if json.Unmarshal(entry.Payload, &h) == nil && h.UserPrompt() {
					// A person's prompt — not a spawned thread's brief, not copied fork history.
					if cs.Human != nil && *cs.Human && !ts.IsZero() && ts.UnixMilli() >= cs.Created {
						cs.Focus.rebase(ix.root(cs.Cwd))
						if mode != readFocus {
							ix.beat(ts, cs.Cwd, beatHuman)
						}
					}
					return nil
				}
			}
			var tc transcripts.TokenCount
			if json.Unmarshal(entry.Payload, &tc) != nil || tc.Type != transcripts.EventTokenCount {
				return nil
			}
			// Copied fork history is older than the thread: its owner recorded it.
			if cs.Owner == "" || ts.IsZero() || ts.UnixMilli() < cs.Created {
				return nil
			}
			// A null info, or an unchanged total, means no call happened: a
			// rate-limit refresh. It still carries a limit reading.
			refresh := tc.Info == nil || cs.Prev != nil && *cs.Prev == tc.Info.Total
			if mode == readTokens || mode == readPoints {
				ix.point(ts, cs, tc, refresh)
			}
			if refresh {
				return nil
			}
			total, last := tc.Info.Total, tc.Info.Last
			// Kept past receipts too: the next point tells a call from a refresh by it.
			cs.Prev = &total
			// Receipts are exact; once a thread writes them, token_count is bookkeeping.
			if cs.Receipts || mode == readPoints || !last.Valid() || last.Input+last.Output == 0 {
				return nil
			}
			key := identity("codex-count", cs.Owner, strconv.FormatInt(total.Input, 10), strconv.FormatInt(total.Cached, 10),
				strconv.FormatInt(total.CacheWrite, 10), strconv.FormatInt(total.Output, 10))
			if tokens && ix.seen(key, srcCodex, ts) {
				ix.codexCopy(ts, cs, key)
				return nil
			}
			cs.LastLegacy, cs.Staged = &last, ix.staging
			ix.codexBeat(ts, cs, key, mode) // a backlog read records every one: see catchUp
			if tokens {
				ix.chargeCodex(ts, cs, last)
			}
		}
		return nil
	}
}

// codexBeat records a response's AI minute under the roots its calls wrote
// into, else the focus, and closes the response.
func (ix *indexer) codexBeat(ts time.Time, cs *codexState, key int64, mode readMode) {
	ix.syncBase(&cs.Focus, cs.Cwd, mode)
	ix.aiBeat(ts, cs.Focus.roots(), key, mode)
	cs.Focus.Touched = nil
}

// codexCopy closes a response a token read found already seen: it adds no
// minute, but while history is re-read it is staged and found (copyFound).
func (ix *indexer) codexCopy(ts time.Time, cs *codexState, key int64) {
	if ix.staging {
		ix.syncBase(&cs.Focus, cs.Cwd, readTokens)
		ix.copyFound(ts, cs.Focus.roots(), key)
	}
	cs.Focus.Touched = nil
}

func sameUsage(a, b transcripts.CodexUsage) bool {
	return a.Input == b.Input && a.Cached == b.Cached && a.CacheWrite == b.CacheWrite && a.Output == b.Output
}

func (ix *indexer) chargeCodex(ts time.Time, cs *codexState, u transcripts.CodexUsage) {
	model := cs.Model
	if model == "" {
		model = UnknownCodexModel
	}
	// Cached and cache-written tokens are subsets of input; split them out.
	c := Counts{Input: u.Input - u.Cached - u.CacheWrite, CacheRead: u.Cached, CacheWrite5m: u.CacheWrite, Output: u.Output, Responses: 1}
	ix.add(ts, cs.Cwd, model, OpenAI, c, "codex:"+cs.Owner)
}

// limitPoint is one limit_points row: the account's rate-limit reading a
// token_count carried, and the call it followed (zero for a refresh).
type limitPoint struct {
	ts                  int64
	thread, plan, model string
	u                   transcripts.CodexUsage
	windows             string // JSON []LimitWindow
}

// point records the limit reading of a token_count. Rollouts older than the
// readings carry none. A copy of the event — an archived rollout, a re-read,
// the backlog meeting what a token read recorded — has the same identity, and
// the store keeps one.
func (ix *indexer) point(ts time.Time, cs *codexState, tc transcripts.TokenCount, refresh bool) {
	if tc.RateLimits == nil {
		return
	}
	windows := []LimitWindow{}
	for _, w := range []*transcripts.RateWindow{tc.RateLimits.Primary, tc.RateLimits.Secondary} {
		if w != nil {
			windows = append(windows, LimitWindow{Minutes: w.WindowMinutes, ResetsAt: w.ResetsAt, Pct: w.UsedPercent})
		}
	}
	if len(windows) == 0 {
		return
	}
	var total, last transcripts.CodexUsage
	if tc.Info != nil {
		total = tc.Info.Total
		// Counters the charge rejects are no call either.
		if !refresh && tc.Info.Last.Valid() {
			last = tc.Info.Last
		}
	}
	model := cs.Model
	if model == "" {
		model = UnknownCodexModel
	}
	raw, _ := json.Marshal(windows)
	key := identity("codex-limit", cs.Owner, strconv.FormatInt(ts.UnixMilli(), 10), strconv.FormatInt(total.Input, 10),
		strconv.FormatInt(total.Cached, 10), strconv.FormatInt(total.CacheWrite, 10), strconv.FormatInt(total.Output, 10), string(raw))
	ix.points[key] = limitPoint{ts: ts.UnixMilli(), thread: cs.Owner, plan: tc.RateLimits.PlanType, model: model, u: last, windows: string(raw)}
}

// seen reports whether an identity was already counted, and records it if not.
func (ix *indexer) seen(key int64, src int, ts time.Time) bool {
	if _, ok := ix.newSeen[key]; ok {
		ix.report.Duplicates++
		return true
	}
	var one int
	if err := ix.seenStmt.QueryRow(key).Scan(&one); err == nil {
		ix.report.Duplicates++
		return true
	}
	ix.newSeen[key] = seenRow{src: src, day: ts.Unix() / 86400}
	return false
}

func (ix *indexer) add(ts time.Time, cwd, model string, lane Lane, c Counts, session string) {
	root := ix.root(cwd)
	hour := ts.Unix() - ts.Unix()%3600
	bk := bucketKey{hour: hour, root: root, model: model}
	b := ix.buckets[bk]
	if b == nil {
		b = &bucketVal{lane: lane}
		ix.buckets[bk] = b
	}
	b.c.add(c)
	ms := ts.UnixMilli()
	sk := spanKey{session: session, hour: hour, root: root}
	if sp := ix.spans[sk]; sp == nil {
		ix.spans[sk] = &span{first: ms, last: ms}
	} else {
		sp.first, sp.last = min(sp.first, ms), max(sp.last, ms)
	}
	ix.report.Responses++
}

// root resolves a cwd to its repository once per pass; exact answers are
// kept forever, because a removed worktree can no longer be resolved.
func (ix *indexer) root(cwd string) string {
	if root, ok := ix.rootMemo[cwd]; ok {
		return root
	}
	root, exact := transcripts.GitRoot(cwd)
	ix.rootMemo[cwd] = root
	if exact {
		ix.newRoots[cwd] = root
	}
	return root
}

// beat records that a minute of cwd's project saw a prompt or an answer.
func (ix *indexer) beat(ts time.Time, cwd string, kind int) {
	if ts.IsZero() {
		return
	}
	ix.beats[beatKey{minute: ts.Unix() / 60, root: ix.root(cwd), kind: kind}] = struct{}{}
}

// backlog reads the beats owed by bytes passes read before beats existed —
// beats only, charging nothing — with whatever budget the token reads left,
// so it never delays a token. Newest files go first: the recent weeks a
// timesheet asks for fill before older history does.
func (ix *indexer) backlog(targets []target, known map[string]fileRow, budget int64, perCommit int) error {
	order := append([]target(nil), targets...)
	sort.SliceStable(order, func(i, j int) bool { return order[i].info.ModTime().After(order[j].info.ModTime()) })
	sinceCommit, lastCommit := 0, time.Now()
	for _, t := range order {
		row, ok := ix.read[t.path]
		if !ok {
			if row, ok = known[t.path]; !ok {
				continue // new and left unread by the budget: its first read records beats
			}
		}
		if row.beats == beatsDone {
			continue
		}
		lag, lost := lagOf(row.beats, row.cur), false
		if !lag.done() && ix.ctx.Err() == nil && (budget <= 0 || ix.report.ReadBytes < budget) {
			var err error
			if lag, lost, err = ix.catchUp(t, &row, lag, budget, readBeats); err != nil {
				return err
			}
		}
		if !lag.done() {
			ix.report.BeatsPendingBytes += lag.Until - lag.Cursor.Offset
		}
		if beats := lag.encode(); beats != row.beats {
			row.beats = beats
			if lost {
				// What it owed went with the old content, whose last write the
				// debt recorded when it opened (or lagOf gave an older build's).
				// Only a stored cursor that never saw a write leaves it 0;
				// lag.Cursor is the new content's, for a live transcript today.
				at := lag.Written
				if at == 0 {
					at = lag.Cursor.Modified
				}
				ix.lost[t.path] = row
				ix.lostSince = max(ix.lostSince, at/int64(time.Second))
			} else {
				ix.files[t.path] = row
				sinceCommit++
			}
		}
		if (sinceCommit > 0 || ix.dirty > 0) && (ix.dirty >= commitBytes || sinceCommit >= perCommit || time.Since(lastCommit) >= commitEvery) {
			if err := ix.flush(); err != nil {
				return err
			}
			sinceCommit, lastCommit = 0, time.Now()
		}
	}
	return nil
}

// pointsBacklog recovers the limit points of rollout bytes passes read before
// points existed — points only: no token charged, no beat, no seen identity —
// with whatever budget tokens and beats left, newest rollouts first, so the
// recent windows a caller asks for fill before older history. Its debt is its
// own (files.points), apart from the beats backlog and its coverage.
//
// A debt is fixed when it opens, at the cursor this pass started from: the
// token read records the points of everything after it. The backlog reads it
// with its own parse state from byte 0, so a refresh is told from a call just
// as a token read would.
func (ix *indexer) pointsBacklog(targets []target, known map[string]fileRow, budget int64, perCommit int) error {
	order := make([]target, 0, len(targets))
	for _, t := range targets {
		if t.codex {
			order = append(order, t)
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return order[i].info.ModTime().After(order[j].info.ModTime()) })
	sinceCommit, lastCommit := 0, time.Now()
	for _, t := range order {
		row, ok := known[t.path]
		if !ok || row.points == pointsDone {
			continue // new this pass: its token read records its points
		}
		lag := pointsOf(row.points, row.cur)
		if !lag.done() && ix.ctx.Err() == nil && (budget <= 0 || ix.report.ReadBytes < budget) {
			var err error
			// A file that shrank under its debt took the points it owed along;
			// nothing claims them, so the debt just ends.
			if lag, _, err = ix.catchUp(t, &row, lag, budget, readPoints); err != nil {
				return err
			}
		}
		if !lag.done() {
			ix.report.PointsPendingBytes += lag.Until - lag.Cursor.Offset
		}
		if points := lag.encode(); points != row.points {
			ix.debts[t.path] = points
			sinceCommit++
		}
		if (sinceCommit > 0 || ix.dirty > 0) && (ix.dirty >= commitBytes || sinceCommit >= perCommit || time.Since(lastCommit) >= commitEvery) {
			if err := ix.flush(); err != nil {
				return err
			}
			sinceCommit, lastCommit = 0, time.Now()
		}
	}
	return nil
}

// catchUp reads one file's backlog until it is paid, the budget runs out or
// the pass is stopped; mode names the backlog: beats, focus or points. A rollout's
// token state learns from the beats backlog whether a person drives the
// thread, so later token reads record that person's prompts.
//
// The backlog cannot follow the charge the way a token read does: every
// response in it was charged before beats existed, so seen holds all of them,
// and which copy was charged is not recorded. It applies every rule that
// needs no seen — a message's repeated content blocks count once, copied fork
// history and a receipt already charged through its token_count count
// nothing — and records the rest. A copy keeping its record's time and cwd
// lands on the original's minute; one that changed them adds its own.
//
// lost reports a backlog that ended short of Until because its file did.
func (ix *indexer) catchUp(t target, row *fileRow, lag beatsLag, budget int64, mode readMode) (_ beatsLag, lost bool, _ error) {
	var cs codexState
	if lag.State != nil {
		cs = *lag.State
	}
	parse := ix.claudeLine(&cs, mode)
	if t.codex {
		parse = ix.codexLine(&cs, mode)
	}
	for !lag.done() && ix.ctx.Err() == nil {
		if lag.Cursor.Unchanged(t.info) {
			// Read to its end and still short of Until: the file shrank under
			// its debt, and what it owed went with its old content.
			lag.Until, lost = lag.Cursor.Offset, true
			break
		}
		sweep := min(int64(sweepBytes), lag.Until-lag.Cursor.Offset)
		if budget > 0 {
			if sweep = min(sweep, budget-ix.report.ReadBytes); sweep <= 0 {
				break
			}
		}
		res, err := transcripts.Scan(t.path, lag.Cursor, lag.Cursor.PrefixSize > 0, transcripts.ScanOptions{Budget: sweep, SkipOversize: true}, parse)
		if err != nil {
			ix.report.FileErrors = append(ix.report.FileErrors, fmt.Sprintf("%s: %s: %v", t.path, mode, err))
			break
		}
		if res.Skipped {
			break // gone: what it owed is lost, and Index moves coverage past it
		}
		// A replaced file restarts at byte 0 of its new content (Scan already
		// did); what it owes is still bounded by Until.
		lag.Cursor = res.Cursor
		ix.report.ReadBytes += res.Read
		ix.dirty += res.Read
		if res.Read == 0 {
			// Only an unterminated record short of Until: shrunk the same way.
			lag.Until, lost = lag.Cursor.Offset, true
			break
		}
	}
	if lag.done() {
		ix.flushMsg(&cs.Focus, mode) // the debt ends here: its last message is whole
	}
	lag.State = &cs
	if t.codex {
		var ts codexState
		if mode == readBeats && cs.Human != nil && row.state != "" && json.Unmarshal([]byte(row.state), &ts) == nil && ts.Human == nil && ts.Owner == cs.Owner {
			ts.Human = cs.Human
			raw, _ := json.Marshal(ts)
			row.state = string(raw)
		}
	}
	return lag, lost, nil
}

// oldestClaude is the modification time (unix seconds) of the oldest Claude
// transcript listed; 0 when there is none.
func oldestClaude(targets []target) int64 {
	var oldest int64
	for _, t := range targets {
		if m := t.info.ModTime().Unix(); !t.codex && (oldest == 0 || m < oldest) {
			oldest = m
		}
	}
	return oldest
}

func (ix *indexer) flush() error {
	tx, err := ix.begin()
	if err != nil {
		return err
	}
	return ix.commit(tx)
}

func (ix *indexer) begin() (*sql.Tx, error) { return ix.s.db.Begin() }

// commit writes the aggregate, the identities and the cursors in ONE
// transaction: a crash either keeps all three or none, so a re-read after a
// crash re-counts nothing.
func (ix *indexer) commit(tx *sql.Tx) error {
	fail := func(err error) error {
		tx.Rollback()
		return err
	}
	for bk, b := range ix.buckets {
		project, err := ix.id(tx, "projects", "root", bk.root, ix.projects)
		if err != nil {
			return fail(err)
		}
		c := b.c
		if _, err := tx.Exec(`INSERT INTO buckets(hour, project, model, lane, input, output, cache_write_5m, cache_write_1h, cache_read, responses)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(hour, project, model) DO UPDATE SET
				input = input + excluded.input, output = output + excluded.output,
				cache_write_5m = cache_write_5m + excluded.cache_write_5m, cache_write_1h = cache_write_1h + excluded.cache_write_1h,
				cache_read = cache_read + excluded.cache_read, responses = responses + excluded.responses`,
			bk.hour, project, bk.model, string(b.lane), c.Input, c.Output, c.CacheWrite5m, c.CacheWrite1h, c.CacheRead, c.Responses); err != nil {
			return fail(err)
		}
	}
	for sk, sp := range ix.spans {
		project, err := ix.id(tx, "projects", "root", sk.root, ix.projects)
		if err != nil {
			return fail(err)
		}
		session, err := ix.id(tx, "sessions", "key", sk.session, ix.sessions)
		if err != nil {
			return fail(err)
		}
		if _, err := tx.Exec(`INSERT INTO session_hours(session, hour, project, first, last) VALUES(?, ?, ?, ?, ?)
			ON CONFLICT(session, hour, project) DO UPDATE SET first = min(first, excluded.first), last = max(last, excluded.last)`,
			session, sk.hour, project, sp.first, sp.last); err != nil {
			return fail(err)
		}
	}
	if len(ix.beats) > 0 {
		stmt, err := tx.Prepare("INSERT OR IGNORE INTO beats(minute, project, kind) VALUES(?, ?, ?)")
		if err != nil {
			return fail(err)
		}
		for bk := range ix.beats {
			project, err := ix.id(tx, "projects", "root", bk.root, ix.projects)
			if err == nil {
				_, err = stmt.Exec(bk.minute, project, bk.kind)
			}
			if err != nil {
				stmt.Close()
				return fail(err)
			}
		}
		stmt.Close()
	}
	for key, row := range ix.newSeen {
		if _, err := tx.Exec("INSERT OR IGNORE INTO seen(id, src, day) VALUES(?, ?, ?)", key, row.src, row.day); err != nil {
			return fail(err)
		}
	}
	var points int64
	if len(ix.points) > 0 {
		stmt, err := tx.Prepare(`INSERT OR IGNORE INTO limit_points(id, ts, thread, plan, model, input, cached, cache_write, output, reasoning, windows)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return fail(err)
		}
		for key, p := range ix.points {
			res, err := stmt.Exec(key, p.ts, p.thread, p.plan, p.model, p.u.Input, p.u.Cached, p.u.CacheWrite, p.u.Output, p.u.Reasoning, p.windows)
			var n int64
			if err == nil {
				n, err = res.RowsAffected()
			}
			if err != nil {
				stmt.Close()
				return fail(err)
			}
			points += n
		}
		stmt.Close()
	}
	// A row is inserted only by its first read, which records its points as
	// it goes and its minutes by the focus; after that only the points backlog
	// moves points, and only focusDebts move focus.
	for path, row := range ix.files {
		raw, err := json.Marshal(row.cur)
		if err != nil {
			return fail(err)
		}
		if _, err := tx.Exec(`INSERT INTO files(path, cursor, state, tail, beats, points, focus) VALUES(?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(path) DO UPDATE SET cursor = excluded.cursor, state = excluded.state, tail = excluded.tail, beats = excluded.beats`,
			path, string(raw), row.state, row.tail, row.beats, pointsDone, beatsDone); err != nil {
			return fail(err)
		}
	}
	for path, debt := range ix.debts {
		if _, err := tx.Exec("UPDATE files SET points = ? WHERE path = ?", debt, path); err != nil {
			return fail(err)
		}
	}
	for path, debt := range ix.focusDebts {
		if _, err := tx.Exec("UPDATE files SET focus = ? WHERE path = ?", debt, path); err != nil {
			return fail(err)
		}
	}
	if err := ix.commitStaged(tx); err != nil {
		return fail(err)
	}
	for cwd, root := range ix.newRoots {
		if _, err := tx.Exec("INSERT OR REPLACE INTO roots(cwd, root) VALUES(?, ?)", cwd, root); err != nil {
			return fail(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	ix.report.Points += points
	ix.reset()
	return nil
}

// id returns the integer id of a projects/sessions row, creating it.
func (ix *indexer) id(tx *sql.Tx, table, column, value string, cache map[string]int64) (int64, error) {
	if id, ok := cache[value]; ok {
		return id, nil
	}
	if _, err := tx.Exec("INSERT OR IGNORE INTO "+table+"("+column+") VALUES(?)", value); err != nil {
		return 0, err
	}
	var id int64
	if err := tx.QueryRow("SELECT id FROM "+table+" WHERE "+column+" = ?", value).Scan(&id); err != nil {
		return 0, err
	}
	cache[value] = id
	return id, nil
}

// LaneOf names a model's vendor; fallback covers names that say neither.
func LaneOf(model string, fallback Lane) Lane {
	m := strings.ToLower(model)
	switch {
	case strings.HasPrefix(m, "claude"):
		return Anthropic
	case strings.HasPrefix(m, "gpt-"), strings.HasPrefix(m, "codex"), openAIReasoning(m):
		return OpenAI
	}
	return fallback
}

func openAIReasoning(m string) bool {
	for _, p := range []string{"o1", "o3", "o4"} {
		if m == p || strings.HasPrefix(m, p+"-") {
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
