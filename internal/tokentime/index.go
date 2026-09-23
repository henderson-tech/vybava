package tokentime

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
}

// IndexReport summarises one pass.
type IndexReport struct {
	Files        int      `json:"files"`
	Opened       int      `json:"opened"`
	ReadBytes    int64    `json:"readBytes"`
	PendingBytes int64    `json:"pendingBytes"`
	Responses    int64    `json:"responses"`
	Duplicates   int64    `json:"duplicates"`
	Resets       int      `json:"resets"`
	DecodeErrors int64    `json:"decodeErrors"`
	FileErrors   []string `json:"fileErrors"`
	DurationMs   int64    `json:"durationMs"`
}

// ErrBusy means another pass holds the index lock.
var ErrBusy = errors.New("another tokentime index pass is running")

// claudeSeenDays is how long a Claude message id is remembered: longer than
// any transcript can live (cleanupPeriodDays), so a fork or a replaced file
// can never re-count a message whose source still exists.
const claudeSeenDays = 60

// flushEvery bounds the uncommitted aggregate.
const flushEvery = 64 * 1024 * 1024

// sweepBytes bounds one Scan call inside a file.
const sweepBytes = 16 * transcripts.DefaultBudget

// UnknownCodexModel names Codex usage seen before any turn_context.
const UnknownCodexModel = "codex-unknown"

type fileRow struct {
	cur   transcripts.Cursor
	state string
}

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
}

type indexer struct {
	s        *Store
	report   IndexReport
	buckets  map[bucketKey]*bucketVal
	spans    map[spanKey]*span
	newSeen  map[int64]seenRow
	files    map[string]fileRow
	rootMemo map[string]string
	newRoots map[string]string
	projects map[string]int64
	sessions map[string]int64
	dirty    int64
	seenStmt *sql.Stmt
}

// Index reads everything written since the last pass into the buckets.
func (s *Store) Index(opts Options) (IndexReport, error) {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	started := time.Now()
	unlock, err := tryLock(filepath.Join(s.Dir, "index.lock"))
	if err != nil {
		return IndexReport{}, err
	}
	defer unlock()

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
	ix.report.FileErrors = []string{}
	for _, t := range targets {
		row, ok := known[t.path]
		if ok && row.cur.Unchanged(t.info) {
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
		ix.file(t, row, ok, remaining)
		if ix.dirty >= flushEvery {
			if err := ix.flush(); err != nil {
				return IndexReport{}, err
			}
		}
	}
	tx, err := ix.begin()
	if err != nil {
		return IndexReport{}, err
	}
	if walked {
		// Cursors of vanished files go; their buckets stay forever.
		present := make(map[string]bool, len(targets))
		for _, t := range targets {
			present[t.path] = true
		}
		for path := range known {
			if !present[path] {
				if _, err := tx.Exec("DELETE FROM files WHERE path = ?", path); err != nil {
					tx.Rollback()
					return IndexReport{}, err
				}
			}
		}
	}
	today := now().Unix() / 86400
	if _, err := tx.Exec("DELETE FROM seen WHERE src = ? AND day < ?", srcClaude, today-claudeSeenDays); err != nil {
		tx.Rollback()
		return IndexReport{}, err
	}
	if err := setMeta(tx, "pending_bytes", strconv.FormatInt(ix.report.PendingBytes, 10)); err != nil {
		tx.Rollback()
		return IndexReport{}, err
	}
	if err := setMeta(tx, "last_index_at", now().UTC().Format(time.RFC3339)); err != nil {
		tx.Rollback()
		return IndexReport{}, err
	}
	if err := ix.commit(tx); err != nil {
		return IndexReport{}, err
	}
	ix.report.DurationMs = time.Since(started).Milliseconds()
	return ix.report, nil
}

// discover lists every transcript and rollout. walked is false when a root
// could not be listed, so vanished-file cleanup must not run.
func discover(opts Options) ([]target, bool, error) {
	var targets []target
	walked := true
	claude, err := transcripts.WalkClaude(opts.ClaudeRoot)
	if err != nil {
		return nil, false, fmt.Errorf("list %s: %w", opts.ClaudeRoot, err)
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
	rows, err := s.db.Query("SELECT path, cursor, state FROM files")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	known := map[string]fileRow{}
	for rows.Next() {
		var path, cursor, state string
		if err := rows.Scan(&path, &cursor, &state); err != nil {
			return nil, err
		}
		var cur transcripts.Cursor
		if err := json.Unmarshal([]byte(cursor), &cur); err != nil {
			continue // unreadable cursor: re-read the file; identities stop double counting
		}
		known[path] = fileRow{cur: cur, state: state}
	}
	return known, rows.Err()
}

func (s *Store) newIndexer() (*indexer, error) {
	ix := &indexer{
		s: s, rootMemo: map[string]string{}, projects: map[string]int64{}, sessions: map[string]int64{},
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
	ix.newSeen = map[int64]seenRow{}
	ix.files = map[string]fileRow{}
	ix.newRoots = map[string]string{}
	ix.dirty = 0
}

// file reads one transcript or rollout until it is caught up or the pass
// budget runs out.
func (ix *indexer) file(t target, row fileRow, known bool, budget int64) {
	cur := row.cur
	var cs codexState
	if t.codex && row.state != "" {
		_ = json.Unmarshal([]byte(row.state), &cs)
	}
	parse := ix.claudeLine()
	if t.codex {
		parse = ix.codexLine(&cs)
	}
	var read, pending int64
	opened := false
	for {
		sweep := int64(sweepBytes)
		if budget > 0 {
			sweep = min(sweep, budget-read)
		}
		res, err := transcripts.Scan(t.path, cur, known, transcripts.ScanOptions{Budget: sweep, SkipOversize: true}, parse)
		if err != nil {
			ix.report.FileErrors = append(ix.report.FileErrors, fmt.Sprintf("%s: %v", t.path, err))
			ix.report.PendingBytes += max(0, t.info.Size()-cur.Offset)
			return
		}
		if res.Skipped {
			return
		}
		opened = true
		if res.Reset {
			ix.report.Resets++
		}
		cur, known = res.Cursor, true
		read += res.Read
		pending = res.Pending
		if pending == 0 || res.Read == 0 || (budget > 0 && read >= budget) {
			break
		}
	}
	if opened {
		ix.report.Opened++
	}
	ix.report.ReadBytes += read
	ix.report.PendingBytes += pending
	state := ""
	if t.codex {
		raw, _ := json.Marshal(cs)
		state = string(raw)
	}
	ix.files[t.path] = fileRow{cur: cur, state: state}
	ix.dirty += read
}

func (ix *indexer) claudeLine() func([]byte, int64) error {
	lastID := ""
	return func(line []byte, _ int64) error {
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
		if ix.seen(identity("claude", id), srcClaude, rec.Timestamp) {
			return nil
		}
		w5, w1 := u.CacheWrites()
		c := Counts{Input: u.InputTokens, Output: u.OutputTokens, CacheWrite5m: w5, CacheWrite1h: w1, CacheRead: u.CacheReadInputTokens, Responses: 1}
		session := "claude:" + rec.SessionID
		ix.add(rec.Timestamp, rec.Cwd, model, LaneOf(model, Anthropic), c, session)
		return nil
	}
}

func (ix *indexer) codexLine(cs *codexState) func([]byte, int64) error {
	return func(line []byte, offset int64) error {
		if offset == 0 {
			*cs = codexState{} // first read, or the file was replaced
		}
		if !transcripts.RolloutUsageLine(line) {
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
				// is already charged. Remember the receipt, charge nothing.
				ix.seen(key, srcCodex, ts)
				return nil
			}
			if ix.seen(key, srcCodex, ts) {
				return nil
			}
			ix.chargeCodex(ts, cs, r.Usage)
		case transcripts.RolloutEventMsg:
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
			key := identity("codex-count", cs.Owner, strconv.FormatInt(total.Input, 10), strconv.FormatInt(total.Cached, 10),
				strconv.FormatInt(total.CacheWrite, 10), strconv.FormatInt(total.Output, 10))
			if ix.seen(key, srcCodex, ts) {
				return nil
			}
			cs.LastLegacy = &last
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
		if _, err := tx.Exec(`INSERT INTO files(path, cursor, state) VALUES(?, ?, ?)
			ON CONFLICT(path) DO UPDATE SET cursor = excluded.cursor, state = excluded.state`, path, string(raw), row.state); err != nil {
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
