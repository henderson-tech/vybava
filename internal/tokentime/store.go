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
CREATE TABLE IF NOT EXISTS files(path TEXT PRIMARY KEY, cursor TEXT NOT NULL, state TEXT NOT NULL DEFAULT '', tail INTEGER NOT NULL DEFAULT 0);
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
PRAGMA user_version=2;
`

// migrations bring an older schema up to date, keyed by the version they start from.
var migrations = map[int]string{
	// v1 → v2: a file whose unread bytes are an unterminated tail.
	1: "ALTER TABLE files ADD COLUMN tail INTEGER NOT NULL DEFAULT 0",
}

// Seen-identity sources. Claude message ids are pruned once no transcript can
// still hold a copy; Codex response ids are kept, because an old thread can be
// archived — moved to a new path — at any time.
const (
	srcClaude = 0
	srcCodex  = 1
)

// Store is the state directory: tokentime.db plus its lock and price override.
type Store struct {
	Dir string
	db  *sql.DB
}

// DefaultStateDir is ~/.local/share/vybava/tokentime.
func DefaultStateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "vybava", "tokentime"), nil
}

// Open creates or opens the state directory's database.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "tokentime.db")
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=busy_timeout(10000)"}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version > 2 {
		db.Close()
		return nil, fmt.Errorf("%s was written by a newer tokentime (schema %d)", path, version)
	}
	if m, ok := migrations[version]; ok {
		if _, err := db.Exec(m); err != nil {
			db.Close()
			return nil, fmt.Errorf("migrate %s from schema %d: %w", path, version, err)
		}
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init %s: %w", path, err)
	}
	return &Store{Dir: dir, db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) meta(key string) (string, error) {
	var v string
	err := s.db.QueryRow("SELECT value FROM meta WHERE key = ?", key).Scan(&v)
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
