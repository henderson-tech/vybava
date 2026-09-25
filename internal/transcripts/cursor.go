// Package transcripts reads the append-only JSONL logs agent CLIs write —
// Claude Code transcripts and Codex rollouts — incrementally and read-only.
//
// The cursor is the load-bearing part: a sweep never consumes a partially
// written last record, never reads more than its budget (plus the record it is
// in), and notices a file that was replaced underneath it (a 256-byte prefix
// digest) rather than trusting a byte offset into different content.
package transcripts

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"time"
)

// Cursor is one file's read position. The JSON shape is persisted by
// internal/operator, so field names and tags are a compatibility contract.
type Cursor struct {
	Offset     int64     `json:"offset"`
	Size       int64     `json:"size,omitempty"`
	Modified   int64     `json:"modified,omitempty"`
	VerifiedAt time.Time `json:"verified_at,omitempty"`
	// The prefix detects replacement, including replacement by a larger file.
	Prefix     string `json:"prefix"`
	PrefixSize int    `json:"prefix_size"`
}

// Unchanged reports whether a completed file is byte-for-byte where the
// cursor left it by size and modification time — no need to open it.
func (c Cursor) Unchanged(info os.FileInfo) bool {
	return c.Offset == info.Size() && c.Size == info.Size() && c.Modified == info.ModTime().UnixNano()
}

const (
	// DefaultBudget bounds one sweep of one file.
	DefaultBudget = 4 * 1024 * 1024
	// DefaultRecordLimit bounds one record held in memory.
	DefaultRecordLimit = 16 * 1024 * 1024
)

// ErrRecordTooLarge is returned for a record past RecordLimit unless
// ScanOptions.SkipOversize is set.
var ErrRecordTooLarge = errors.New("session record exceeds 16 MiB")

// ScanOptions tune one sweep. The zero value reads 4 MiB, holds records up to
// 16 MiB and never reopens an unchanged completed file.
type ScanOptions struct {
	// Baseline starts a file never seen before at its last complete record
	// instead of byte 0: only what is written from now on is observed.
	Baseline bool
	// Budget bounds the bytes consumed per sweep; the record that crosses it
	// is still finished. Zero means DefaultBudget.
	Budget int64
	// RecordLimit bounds a record held in memory. Zero means DefaultRecordLimit.
	RecordLimit int
	// SkipOversize steps over a record past RecordLimit (without holding it)
	// instead of failing the sweep. Transcript bodies that cannot carry usage
	// evidence — huge tool outputs, base instructions — are safe to skip.
	SkipOversize bool
	// RecheckAfter reopens an unchanged completed file for a prefix check once
	// this long has passed since the last verification, so a restored
	// timestamp cannot hide a replacement forever. Zero never reopens it.
	RecheckAfter time.Duration
	// Now overrides the clock stamped into Cursor.VerifiedAt.
	Now func() time.Time
}

// ScanResult reports one sweep.
type ScanResult struct {
	Cursor Cursor
	// Skipped: the file vanished, is not a regular file, or is unchanged. The
	// caller must keep its previous cursor.
	Skipped bool
	// Reset: the prefix changed or the file shrank, so the sweep restarted
	// from byte 0 of what is now a different file.
	Reset bool
	// Read is the number of bytes consumed as complete records.
	Read int64
	// Pending is what is left after the sweep: size − offset.
	Pending int64
	// Oversize counts the records SkipOversize stepped over unread.
	Oversize int
}

// Scan reads the complete records after cur and hands each to fn with its
// starting byte offset. known says whether cur came from an earlier sweep.
// An error from fn aborts the sweep; the caller keeps its previous cursor.
func Scan(path string, cur Cursor, known bool, opts ScanOptions, fn func(line []byte, offset int64) error) (ScanResult, error) {
	budget := opts.Budget
	if budget <= 0 {
		budget = DefaultBudget
	}
	limit := opts.RecordLimit
	if limit <= 0 {
		limit = DefaultRecordLimit
	}
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return ScanResult{Skipped: true}, nil // removed after the directory listing
	}
	if err != nil {
		return ScanResult{}, err
	}
	if !info.Mode().IsRegular() {
		return ScanResult{Skipped: true}, nil
	}
	// Skip opening unchanged completed files. With RecheckAfter set, their
	// prefix is still re-verified periodically.
	if !opts.Baseline && known && cur.Unchanged(info) &&
		(opts.RecheckAfter <= 0 || now().Sub(cur.VerifiedAt) < opts.RecheckAfter) {
		return ScanResult{Cursor: cur, Skipped: true}, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return ScanResult{Skipped: true}, nil // removed between stat and open
	}
	if err != nil {
		return ScanResult{}, err
	}
	defer f.Close()
	var res ScanResult
	if opts.Baseline {
		cur.Offset = info.Size()
		// Baseline only through the last newline; an incomplete first record
		// must be consumed when its writer finishes it.
		if cur.Offset > 0 {
			lastByte := make([]byte, 1)
			if _, err := f.ReadAt(lastByte, cur.Offset-1); err != nil {
				return ScanResult{}, err
			}
			if lastByte[0] != '\n' {
				start := max(int64(0), cur.Offset-DefaultBudget)
				tail := make([]byte, cur.Offset-start)
				if _, err := f.ReadAt(tail, start); err != nil {
					return ScanResult{}, err
				}
				last := bytes.LastIndexByte(tail, '\n')
				if last < 0 && start > 0 {
					return ScanResult{}, errors.New("initial partial session record exceeds 4 MiB")
				}
				cur.Offset = start + int64(last+1)
			}
		}
		known = false
	}
	if known && cur.PrefixSize > 0 {
		prefix := make([]byte, cur.PrefixSize)
		n, err := f.ReadAt(prefix, 0)
		if err != nil && !errors.Is(err, io.EOF) {
			return ScanResult{}, err
		}
		if n != cur.PrefixSize || Digest(prefix[:n]) != cur.Prefix {
			cur, res.Reset = Cursor{}, true
		}
	}
	if cur.Offset > info.Size() {
		cur, res.Reset = Cursor{}, true
	}
	if cur.PrefixSize == 0 && info.Size() > 0 {
		cur.PrefixSize = int(min(info.Size(), 256))
		prefix := make([]byte, cur.PrefixSize)
		if _, err := f.ReadAt(prefix, 0); err != nil {
			return ScanResult{}, err
		}
		cur.Prefix = Digest(prefix)
	}
	if _, err := f.Seek(cur.Offset, io.SeekStart); err != nil {
		return ScanResult{}, err
	}
	r := bufio.NewReaderSize(f, 256*1024)
	var buf []byte
	for res.Read < budget {
		line, n, over, err := readRecord(r, buf[:0], limit)
		// A partial oversize record fails like a complete one: holding it
		// until its writer finishes would breach the limit anyway.
		if over && !opts.SkipOversize {
			return ScanResult{}, ErrRecordTooLarge
		}
		if errors.Is(err, io.EOF) {
			break // an unfinished last record waits for its writer
		}
		if err != nil {
			return ScanResult{}, err
		}
		if over {
			res.Oversize++
		} else {
			buf = line
			// line is reused by the next record; fn must copy what it keeps.
			if err := fn(line, cur.Offset); err != nil {
				return ScanResult{}, err
			}
		}
		cur.Offset += n
		res.Read += n
	}
	cur.Size, cur.Modified, cur.VerifiedAt = info.Size(), info.ModTime().UnixNano(), now().UTC()
	res.Cursor = cur
	res.Pending = info.Size() - cur.Offset
	return res, nil
}

// readRecord reads through the next newline. A record longer than limit is
// consumed without being held (over=true, line=nil); n always counts every
// byte consumed, the newline included. io.EOF means no newline followed.
func readRecord(r *bufio.Reader, buf []byte, limit int) (line []byte, n int64, over bool, err error) {
	for {
		chunk, err := r.ReadSlice('\n')
		n += int64(len(chunk))
		if !over {
			if len(buf)+len(chunk) > limit {
				over, buf = true, nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			return nil, n, over, err
		}
		return buf, n, over, nil
	}
}

// Digest is the prefix fingerprint stored in Cursor.Prefix.
func Digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
