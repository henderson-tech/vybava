package tokentime

import (
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
	// before beats existed, owed one more read (beats only).
	BeatsPendingBytes int64    `json:"beatsPendingBytes"`
	Responses         int64    `json:"responses"`
	Duplicates        int64    `json:"duplicates"`
	Resets            int      `json:"resets"`
	DecodeErrors      int64    `json:"decodeErrors"`
	FileErrors        []string `json:"fileErrors"`
	DurationMs        int64    `json:"durationMs"`
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
// everything up to where its token cursor stands; beats are idempotent, so an
// unreadable backlog is owed again from byte 0 rather than lost.
func lagOf(stored string, cur transcripts.Cursor) beatsLag {
	var l beatsLag
	if stored == "" || json.Unmarshal([]byte(stored), &l) != nil {
		return beatsLag{Until: cur.Offset}
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
}

type indexer struct {
	s       *Store
	ctx     context.Context
	now     time.Time
	report  IndexReport
	buckets map[bucketKey]*bucketVal
	spans   map[spanKey]*span
	beats   map[beatKey]struct{}
	newSeen map[int64]seenRow
	files   map[string]fileRow
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
			continue
		}
		if ok && row.tail && row.cur.Size == t.info.Size() && row.cur.Modified == t.info.ModTime().UnixNano() {
			ix.tail(t, row.cur) // nothing appended: the same unterminated tail
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
				if row.beats != beatsDone && !lagOf(row.beats, row.cur).done() {
					// Gone with beats unread: no day up to its last write is
					// complete any more.
					beatsSince = max(beatsSince, row.cur.Modified/int64(time.Second))
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
	rows, err := s.db.Query("SELECT path, cursor, state, tail, beats FROM files")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	known := map[string]fileRow{}
	for rows.Next() {
		var path, cursor, state, beats string
		var tail bool
		if err := rows.Scan(&path, &cursor, &state, &tail, &beats); err != nil {
			return nil, err
		}
		var cur transcripts.Cursor
		if err := json.Unmarshal([]byte(cursor), &cur); err != nil {
			continue // unreadable cursor: re-read the file; identities stop double counting
		}
		known[path] = fileRow{cur: cur, state: state, tail: tail, beats: beats}
	}
	return known, rows.Err()
}

func (s *Store) newIndexer() (*indexer, error) {
	ix := &indexer{
		s: s, rootMemo: map[string]string{}, projects: map[string]int64{}, sessions: map[string]int64{}, read: map[string]fileRow{}, lost: map[string]fileRow{},
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
	ix.newSeen = map[int64]seenRow{}
	ix.files = map[string]fileRow{}
	ix.newRoots = map[string]string{}
	ix.dirty = 0
}

// file reads one transcript or rollout until it is caught up, the pass budget
// runs out or the pass is stopped. A long file commits between sweeps.
func (ix *indexer) file(t target, row fileRow, known bool, budget int64) error {
	cur := row.cur
	var cs codexState
	if t.codex && row.state != "" {
		_ = json.Unmarshal([]byte(row.state), &cs)
	}
	parse := ix.claudeLine(true)
	if t.codex {
		parse = ix.codexLine(&cs, true)
	}
	var read, pending, unsaved int64
	opened, tail, fresh := false, false, !known
	save := func() {
		state := ""
		if t.codex {
			raw, _ := json.Marshal(cs)
			state = string(raw)
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
		}
		ix.files[t.path] = fileRow{cur: cur, state: state, tail: tail, beats: lag.encode()}
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
// with tokens, charges its response. Without tokens it is the backlog read —
// beats only, the seen identities neither read nor written.
func (ix *indexer) claudeLine(tokens bool) func([]byte, int64) error {
	lastID := ""
	return func(line []byte, _ int64) error {
		if transcripts.ClaudeHumanLine(line) {
			if rec, err := transcripts.DecodeClaude(line); err == nil && rec.HumanPrompt() {
				ix.beat(rec.Timestamp, rec.Cwd, beatHuman)
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
		if id == "" || id == lastID {
			return nil // repeats of one message sit next to each other, one per content block
		}
		lastID = id
		if !tokens {
			ix.beat(rec.Timestamp, rec.Cwd, beatAI) // the backlog: see catchUp
			return nil
		}
		if ix.seen(identity("claude", id), srcClaude, rec.Timestamp) {
			return nil // a copy: its minute is the charged original's
		}
		ix.beat(rec.Timestamp, rec.Cwd, beatAI)
		w5, w1 := u.CacheWrites()
		c := Counts{Input: u.InputTokens, Output: u.OutputTokens, CacheWrite5m: w5, CacheWrite1h: w1, CacheRead: u.CacheReadInputTokens, Responses: 1}
		session := "claude:" + rec.SessionID
		ix.add(rec.Timestamp, rec.Cwd, model, LaneOf(model, Anthropic), c, session)
		return nil
	}
}

// codexLine is claudeLine for a rollout, threading its parse state.
func (ix *indexer) codexLine(cs *codexState, tokens bool) func([]byte, int64) error {
	return func(line []byte, offset int64) error {
		if offset == 0 {
			*cs = codexState{} // first read, or the file was replaced
		}
		if !transcripts.RolloutUsageLine(line) && !transcripts.RolloutUserLine(line) {
			return nil
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
			}
		case transcripts.RolloutUsageRecord:
			var r transcripts.UsageRecord
			if json.Unmarshal(entry.Payload, &r) != nil || cs.Owner == "" || r.ThreadID != cs.Owner ||
				r.ResponseID == "" || !r.Usage.Valid() || ts.IsZero() || ts.UnixMilli() < cs.Created {
				return nil // copied fork history keeps its original owner's thread id
			}
			first := !cs.Receipts
			cs.Receipts = true
			key := identity("codex-receipt", cs.Owner, r.ResponseID)
			if first && cs.LastLegacy != nil && sameUsage(*cs.LastLegacy, r.Usage) {
				// The producer persisted this response's token_count first; it
				// is already charged — and beat. Remember the receipt, charge nothing.
				if tokens {
					ix.seen(key, srcCodex, ts)
				}
				return nil
			}
			if !tokens {
				ix.beat(ts, cs.Cwd, beatAI) // the backlog: see catchUp
				return nil
			}
			if ix.seen(key, srcCodex, ts) {
				return nil
			}
			ix.beat(ts, cs.Cwd, beatAI)
			ix.chargeCodex(ts, cs, r.Usage)
		case transcripts.RolloutEventMsg:
			if transcripts.RolloutUserLine(line) {
				var h transcripts.EventHeader
				if json.Unmarshal(entry.Payload, &h) == nil && h.UserPrompt() {
					// A person's prompt — not a spawned thread's brief, not copied fork history.
					if cs.Human != nil && *cs.Human && !ts.IsZero() && ts.UnixMilli() >= cs.Created {
						ix.beat(ts, cs.Cwd, beatHuman)
					}
					return nil
				}
			}
			var tc transcripts.TokenCount
			if json.Unmarshal(entry.Payload, &tc) != nil || tc.Type != transcripts.EventTokenCount || tc.Info == nil {
				return nil // a null info is a rate-limit refresh
			}
			// Receipts are exact; once a thread writes them, token_count is bookkeeping.
			if cs.Receipts || cs.Owner == "" || ts.IsZero() || ts.UnixMilli() < cs.Created {
				return nil
			}
			// An unchanged total means no call happened: a rate-limit refresh.
			if cs.Prev != nil && *cs.Prev == tc.Info.Total {
				return nil
			}
			total, last := tc.Info.Total, tc.Info.Last
			cs.Prev = &total
			if !last.Valid() || last.Input+last.Output == 0 {
				return nil
			}
			if !tokens {
				cs.LastLegacy = &last
				ix.beat(ts, cs.Cwd, beatAI) // the backlog: see catchUp
				return nil
			}
			key := identity("codex-count", cs.Owner, strconv.FormatInt(total.Input, 10), strconv.FormatInt(total.Cached, 10),
				strconv.FormatInt(total.CacheWrite, 10), strconv.FormatInt(total.Output, 10))
			if ix.seen(key, srcCodex, ts) {
				return nil
			}
			cs.LastLegacy = &last
			ix.beat(ts, cs.Cwd, beatAI)
			ix.chargeCodex(ts, cs, last)
		}
		return nil
	}
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
			if lag, lost, err = ix.catchUp(t, &row, lag, budget); err != nil {
				return err
			}
		}
		if !lag.done() {
			ix.report.BeatsPendingBytes += lag.Until - lag.Cursor.Offset
		}
		if beats := lag.encode(); beats != row.beats {
			row.beats = beats
			if lost {
				// What it owed went with the old content, bounded by the last
				// write the token read saw before this pass (the bound a vanished
				// file gets; Until tracks that cursor). lag.Cursor is the new
				// content's, which for a live transcript is today.
				at := known[t.path].cur.Modified
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

// catchUp reads one file's backlog until it is paid, the budget runs out or
// the pass is stopped. A rollout's token state learns from it whether a
// person drives the thread, so later token reads record that person's prompts.
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
func (ix *indexer) catchUp(t target, row *fileRow, lag beatsLag, budget int64) (_ beatsLag, lost bool, _ error) {
	var cs codexState
	if lag.State != nil {
		cs = *lag.State
	}
	parse := ix.claudeLine(false)
	if t.codex {
		parse = ix.codexLine(&cs, false)
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
			ix.report.FileErrors = append(ix.report.FileErrors, fmt.Sprintf("%s: beats: %v", t.path, err))
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
	if t.codex {
		lag.State = &cs
		var ts codexState
		if cs.Human != nil && row.state != "" && json.Unmarshal([]byte(row.state), &ts) == nil && ts.Human == nil && ts.Owner == cs.Owner {
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
	for path, row := range ix.files {
		raw, err := json.Marshal(row.cur)
		if err != nil {
			return fail(err)
		}
		if _, err := tx.Exec(`INSERT INTO files(path, cursor, state, tail, beats) VALUES(?, ?, ?, ?, ?)
			ON CONFLICT(path) DO UPDATE SET cursor = excluded.cursor, state = excluded.state, tail = excluded.tail, beats = excluded.beats`,
			path, string(raw), row.state, row.tail, row.beats); err != nil {
			return fail(err)
		}
	}
	for cwd, root := range ix.newRoots {
		if _, err := tx.Exec("INSERT OR REPLACE INTO roots(cwd, root) VALUES(?, ?)", cwd, root); err != nil {
			return fail(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
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
