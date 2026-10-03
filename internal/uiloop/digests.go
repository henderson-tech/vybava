package uiloop

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"time"
)

// digestCacheVersion is <pass>/.cache/digests.json's v.
const digestCacheVersion = 1

// racyWindow is how long after its last change a file stays uncached: a
// filesystem that stamps whole seconds (two on FAT) gives a write landing in
// the same tick as the hash the old stamp, which only an unrecorded entry
// survives.
const racyWindow = 2 * time.Second

// fileStamp is what os.Stat says of a file: it changes on every write,
// rename-over or rsync, so a file whose stamp still matches its cache entry
// has the bytes that entry hashed. ctime and the inode change even when a
// writer restores size and mtime.
type fileStamp struct {
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtimeNs"`
	CtimeNs int64  `json:"ctimeNs"`
	Ino     uint64 `json:"ino"`
	Dev     uint64 `json:"dev"`
}

type cachedDigest struct {
	fileStamp
	SHA256 string `json:"sha256"`
}

type digestCacheFile struct {
	V     int                     `json:"v"`
	Files map[string]cachedDigest `json:"files"`
}

// digestCache is one reviewEvidence's view of <pass>/.cache/digests.json, the
// SHA256 the pass's evidence files had when last read, keyed relative to Root
// like the hashes, so a state call stats 2,000 shots instead of reading them
// again (fixit/5334). It is never evidence: no basis, digest, publish, rsync
// or done judgement reads it. A corrupt or other-version file is a warning
// and a rebuild, and so is a failed write.
type digestCache struct {
	file  string
	now   time.Time
	old   map[string]cachedDigest
	seen  map[string]cachedDigest
	stale bool
	diags []runxDiagnostic
}

func (t *Tool) openDigestCache(pass int) *digestCache {
	c := &digestCache{file: filepath.Join(t.passAbs(pass), ".cache", "digests.json"), now: t.Now(), old: map[string]cachedDigest{}, seen: map[string]cachedDigest{}}
	var f digestCacheFile
	found, err := readJSON(c.file, &f)
	switch {
	case !found && err == nil:
	case err != nil:
		c.problem("does not decode: %v", err)
	case f.V != digestCacheVersion:
		c.problem("is v%d, this vybava writes v%d", f.V, digestCacheVersion)
	default:
		c.old = f.Files
	}
	return c
}

func (c *digestCache) problem(format string, args ...any) {
	c.stale = true
	c.diags = append(c.diags, warn(DiagDigestCache, fmt.Sprintf("%s %s; every file it covers is hashed again", c.file, fmt.Sprintf(format, args...)),
		"nothing to do unless it recurs: the next read rewrites it (delete it to rebuild by hand)"))
}

// sum is file's SHA256 (rel is its key), from the cache while its stamp
// matches; found is false when the file is gone.
func (c *digestCache) sum(file, rel string) (sum string, found bool, err error) {
	stamp, stamped, err := statStamp(file)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if e, ok := c.old[rel]; stamped && ok && e.fileStamp == stamp {
		c.seen[rel] = e
		return e.SHA256, true, nil
	}
	body, err := os.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	sum = digest(string(body), 64)
	if stamped && c.now.Sub(time.Unix(0, stamp.CtimeNs)) >= racyWindow {
		c.seen[rel] = cachedDigest{stamp, sum}
	}
	return sum, true, nil
}

// save writes the entries this read proved, when they differ from the file,
// through a unique temp file and a rename: concurrent readers of one pass
// race only for whose entries stay, never into a torn file. It returns the
// warnings of the whole read.
func (c *digestCache) save() []runxDiagnostic {
	if !c.stale && maps.Equal(c.old, c.seen) {
		return c.diags
	}
	b, err := json.Marshal(digestCacheFile{V: digestCacheVersion, Files: c.seen})
	if err == nil {
		err = writeCacheFile(c.file, b)
	}
	if err != nil {
		c.diags = append(c.diags, warn(DiagDigestCache, fmt.Sprintf("%s was not written: %v; the next read hashes every file again", c.file, err),
			"make the pass directory writable"))
	}
	return c.diags
}

func writeCacheFile(file string, b []byte) error {
	// Mkdir, not MkdirAll: a pass deleted mid-read must not come back as a
	// directory holding only its cache.
	if err := os.Mkdir(filepath.Dir(file), 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(file), "digests-*.tmp")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), file)
	}
	if err != nil {
		return errors.Join(err, os.Remove(f.Name()))
	}
	return nil
}
