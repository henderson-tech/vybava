// Package tokentime answers "where did my AI tokens go" — by project, model
// and hour — for every Claude Code session and Codex thread on this machine.
//
// It indexes Claude transcripts (~/.claude/projects) and Codex rollouts
// (~/.codex) incrementally into permanent hour × project × model buckets in a
// local SQLite file. Buckets outlive their sources: Claude Code deletes
// transcripts after cleanupPeriodDays, and a lifetime total must never shrink.
// Every response is counted once — duplicate copies (forks, archived rollouts,
// a replaced file) are recognised by a message/response identity.
package tokentime

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// Counts are disjoint token components: their sum is the total. Cache writes
// are split by TTL because Anthropic prices them differently.
type Counts struct {
	Input        int64 `json:"input"`
	Output       int64 `json:"output"`
	CacheWrite5m int64 `json:"cacheWrite5m"`
	CacheWrite1h int64 `json:"cacheWrite1h"`
	CacheRead    int64 `json:"cacheRead"`
	Responses    int64 `json:"responses"`
}

// Total sums the token components.
func (c Counts) Total() int64 {
	return c.Input + c.Output + c.CacheWrite5m + c.CacheWrite1h + c.CacheRead
}

func (c *Counts) add(o Counts) {
	c.Input += o.Input
	c.Output += o.Output
	c.CacheWrite5m += o.CacheWrite5m
	c.CacheWrite1h += o.CacheWrite1h
	c.CacheRead += o.CacheRead
	c.Responses += o.Responses
}

// Lane is the model vendor.
type Lane string

const (
	Anthropic Lane = "anthropic"
	OpenAI    Lane = "openai"
)

const schema = `
PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS files(path TEXT PRIMARY KEY, cursor TEXT NOT NULL, state TEXT NOT NULL DEFAULT '', tail INTEGER NOT NULL DEFAULT 0, beats TEXT NOT NULL DEFAULT '');
CREATE TABLE IF NOT EXISTS projects(id INTEGER PRIMARY KEY, root TEXT NOT NULL UNIQUE);
CREATE TABLE IF NOT EXISTS roots(cwd TEXT PRIMARY KEY, root TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS buckets(
	hour INTEGER NOT NULL,
	project INTEGER NOT NULL,
	model TEXT NOT NULL,
	lane TEXT NOT NULL,
	input INTEGER NOT NULL DEFAULT 0,
	output INTEGER NOT NULL DEFAULT 0,
	cache_write_5m INTEGER NOT NULL DEFAULT 0,
	cache_write_1h INTEGER NOT NULL DEFAULT 0,
	cache_read INTEGER NOT NULL DEFAULT 0,
	responses INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY(hour, project, model)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS buckets_by_project ON buckets(project, hour);
CREATE TABLE IF NOT EXISTS sessions(id INTEGER PRIMARY KEY, key TEXT NOT NULL UNIQUE);
CREATE TABLE IF NOT EXISTS session_hours(
	session INTEGER NOT NULL,
	hour INTEGER NOT NULL,
	project INTEGER NOT NULL,
	first INTEGER NOT NULL,
	last INTEGER NOT NULL,
	PRIMARY KEY(session, hour, project)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS session_hours_by_hour ON session_hours(hour);
CREATE TABLE IF NOT EXISTS seen(id INTEGER PRIMARY KEY, src INTEGER NOT NULL, day INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS beats(
	minute INTEGER NOT NULL,
	project INTEGER NOT NULL,
	kind INTEGER NOT NULL,
	PRIMARY KEY(minute, project, kind)
) WITHOUT ROWID;
PRAGMA user_version=4;
`

// schemaVersion is the user_version the schema above ends on.
const schemaVersion = 4

// readableSchema is the oldest schema the rollup and status queries run on
// unchanged — schema 3 only added an index, schema 4 a table and a column
// they never read — so a store an index pass has not migrated yet is still
// served. A migration that changes a table they read raises it.
const readableSchema = 2

// projectSchema is the oldest schema the project verb reads: the first with
// buckets_by_project. beatsSchema is the first that records beats.
const (
	projectSchema = 3
	beatsSchema   = 4
)

// migrations bring an older schema up to date, keyed by the version they
// start from. The schema itself re-runs after them on every older store, so
// additive DDL it declares IF NOT EXISTS needs no entry — v2 → v3 is only
// buckets_by_project, which lets a project's reads (its lifetime sum and
// span, its range) seek instead of scanning the permanent buckets table.
// session_hours has no such index on purpose: the project verb's reads are
// already bounded by session_hours_by_hour and its primary key, and a
// project-leading one would win the rollup's DISTINCT project, session read
// over its hour range and turn it into a whole-table scan.
//
// Every migration adds one column, and adds it only when it is missing: the
// schema's version bump is a separate statement, so a pass killed between the
// two leaves the column behind in a store still at the old version, and
// ALTER TABLE ADD COLUMN is not idempotent.
var migrations = map[int]column{
	// v1 → v2: a file whose unread bytes are an unterminated tail.
	1: {"files", "tail", "INTEGER NOT NULL DEFAULT 0"},
	// v3 → v4: each file's beats backlog ('' = read before beats existed).
	3: {"files", "beats", "TEXT NOT NULL DEFAULT ''"},
}

type column struct{ table, name, decl string }

// addColumn adds c unless its table already has it.
func addColumn(db *sql.DB, c column) error {
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?", c.table, c.name).Scan(&n); err != nil || n > 0 {
		return err
	}
	_, err := db.Exec("ALTER TABLE " + c.table + " ADD COLUMN " + c.name + " " + c.decl)
	return err
}

// Seen-identity sources. Every identity is kept forever: a transcript or
// rollout can reappear at any age (restored from a backup, archived to a new
// path, re-read after its cursor was lost) and must never count twice.
const (
	srcClaude = 0
	srcCodex  = 1
)

// Beat kinds: a minute you typed a prompt in, a minute an agent answered in.
const (
	beatHuman = 0
	beatAI    = 1
)

// Store is the state directory: tokentime.db plus its lock and price override.
type Store struct {
	Dir string
	// db is nil until an index pass creates the database.
	db      *sql.DB
	version int
}

// DefaultStateDir is ~/.local/share/vybava/tokentime.
func DefaultStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "vybava", "tokentime"), nil
}

var (
	// ErrNoStore: the state directory holds no tokentime.db — nothing was indexed yet.
	ErrNoStore = errors.New("no tokentime store")
	// ErrStaleSchema: the store predates this binary; only an index pass migrates it.
	ErrStaleSchema = errors.New("the store's schema is older than this tokentime")
)

// Open opens the state directory's database for an index pass and reads, and
// writes nothing: no directory or database is created and no DDL runs. Only
// Index, holding the index lock, creates or migrates the store — so a rollup
// or status beside a running pass never runs a migration of its own and
// never waits on one. A missing database reads as ErrNoStore until a pass
// creates it.
func Open(dir string) (*Store, error) {
	path := filepath.Join(dir, "tokentime.db")
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return &Store{Dir: dir}, nil
	} else if err != nil {
		return nil, err
	}
	db, version, err := openDB(path, "rw")
	if err != nil {
		return nil, err
	}
	return &Store{Dir: dir, db: db, version: version}, nil
}

// prepare creates or migrates the database. Only Index calls it, holding the
// index lock, so no two processes ever run DDL at once. The version is read
// again under the lock: another pass may have created or migrated the store
// since Open.
func (s *Store) prepare() error {
	path := filepath.Join(s.Dir, "tokentime.db")
	if s.db == nil {
		db, _, err := openDB(path, "")
		if err != nil {
			return err
		}
		s.db = db
	}
	version, err := readVersion(s.db, path)
	if err != nil {
		return err
	}
	if version < schemaVersion {
		// A new store (version 0) gets every column from the schema itself.
		for from := max(version, 1); version > 0 && from < schemaVersion; from++ {
			if c, ok := migrations[from]; ok {
				if err := addColumn(s.db, c); err != nil {
					return fmt.Errorf("migrate %s from schema %d: %w", path, from, err)
				}
			}
		}
		if _, err := s.db.Exec(schema); err != nil {
			return fmt.Errorf("init %s: %w", path, err)
		}
	}
	s.version = schemaVersion
	return nil
}

// begin opens one read snapshot for a verb's queries. A store nothing was
// indexed into is ErrNoStore, one older than readableSchema ErrStaleSchema.
func (s *Store) begin() (*sql.Tx, error) {
	if s.db == nil || s.version == 0 {
		return nil, fmt.Errorf("%w in %s", ErrNoStore, s.Dir)
	}
	if s.version < readableSchema {
		return nil, fmt.Errorf("%w: %s is schema %d, this binary reads %d or later", ErrStaleSchema,
			filepath.Join(s.Dir, "tokentime.db"), s.version, readableSchema)
	}
	return s.db.Begin()
}

// OpenReadOnly opens an existing store for reading and never writes it: no
// state directory or database is created, no DDL or migration runs, the
// database file is opened mode=ro. It is not opened immutable — an index
// pass may be committing — so SQLite keeps its WAL coordination files
// (-wal, -shm) beside it, as for any WAL reader. A missing store is
// ErrNoStore, one an older binary wrote is ErrStaleSchema.
func OpenReadOnly(dir string) (*Store, error) {
	path := filepath.Join(dir, "tokentime.db")
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w in %s", ErrNoStore, dir)
	} else if err != nil {
		return nil, err
	}
	db, version, err := openDB(path, "ro")
	if err != nil {
		return nil, err
	}
	if version < projectSchema {
		db.Close()
		return nil, fmt.Errorf("%w: %s is schema %d, this binary reads %d or later", ErrStaleSchema, path, version, projectSchema)
	}
	return &Store{Dir: dir, db: db, version: version}, nil
}

// openDB opens path in the given SQLite mode ("" is read-write-create, "rw"
// never creates) and reads its schema version.
func openDB(path, mode string) (*sql.DB, int, error) {
	query := "_pragma=busy_timeout(10000)"
	if mode != "" {
		query = "mode=" + mode + "&" + query
	}
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: query}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, 0, err
	}
	db.SetMaxOpenConns(1)
	version, err := readVersion(db, path)
	if err != nil {
		db.Close()
		return nil, 0, err
	}
	return db, version, nil
}

// readVersion reads the schema version, refusing one a newer tokentime wrote.
func readVersion(db *sql.DB, path string) (int, error) {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return 0, err
	}
	if version > schemaVersion {
		return 0, fmt.Errorf("%s was written by a newer tokentime (schema %d)", path, version)
	}
	return version, nil
}

// Close releases the database.
func (s *Store) Close() error {
	if s.db == nil {
		return nil
	}
	return s.db.Close()
}

// querier is the store's *sql.DB or one read transaction on it: a verb that
// reads with several queries runs them all in one snapshot, so an index pass
// committing between them cannot make one answer disagree with itself.
type querier interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func meta(q querier, key string) (string, error) {
	var v string
	err := q.QueryRow("SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func setMeta(tx *sql.Tx, key, value string) error {
	_, err := tx.Exec("INSERT INTO meta(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value)
	return err
}

// identity hashes a response identity into the seen table's 64-bit key.
func identity(parts ...string) int64 {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return int64(binary.BigEndian.Uint64(h.Sum(nil)[:8]))
}
