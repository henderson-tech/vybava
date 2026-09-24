package tokentime

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
)

// Status is what `tokentime status` reports.
type Status struct {
	StateDir     string `json:"stateDir"`
	ClaudeRoot   string `json:"claudeRoot"`
	CodexDir     string `json:"codexDir"`
	LastIndexAt  string `json:"lastIndexAt"`
	PendingBytes int64  `json:"pendingBytes"`
	Files        int64  `json:"files"`
	Projects     int64  `json:"projects"`
	Sessions     int64  `json:"sessions"`
	Buckets      int64  `json:"buckets"`
	Responses    int64  `json:"responses"`
	DBBytes      int64  `json:"dbBytes"`
}

// Status reads the index bookkeeping without touching any transcript. A store
// nothing was indexed into yet is all zeros, never created to say so.
func (s *Store) Status() (Status, error) {
	st := Status{StateDir: s.Dir}
	tx, err := s.begin()
	if errors.Is(err, ErrNoStore) {
		return st, nil
	} else if err != nil {
		return st, err
	}
	defer tx.Rollback()
	if st.LastIndexAt, err = meta(tx, "last_index_at"); err != nil {
		return st, err
	}
	pending, err := meta(tx, "pending_bytes")
	if err != nil {
		return st, err
	}
	st.PendingBytes, _ = strconv.ParseInt(pending, 10, 64)
	for _, q := range []struct {
		sql string
		dst *int64
	}{
		{"SELECT COUNT(*) FROM files", &st.Files},
		{"SELECT COUNT(*) FROM projects", &st.Projects},
		{"SELECT COUNT(*) FROM sessions", &st.Sessions},
		{"SELECT COUNT(*) FROM buckets", &st.Buckets},
		{"SELECT COALESCE(SUM(responses), 0) FROM buckets", &st.Responses},
	} {
		if err := tx.QueryRow(q.sql).Scan(q.dst); err != nil {
			return st, err
		}
	}
	for _, name := range []string{"tokentime.db", "tokentime.db-wal"} {
		if info, err := os.Stat(filepath.Join(s.Dir, name)); err == nil {
			st.DBBytes += info.Size()
		}
	}
	return st, nil
}
