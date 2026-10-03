package findsession

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// sessionFile is one main-session transcript: <root>/<project>/<id>.jsonl.
// Subagent transcripts are never listed — only a main session reopens.
type sessionFile struct {
	Path    string
	ID      string
	ModTime time.Time
}

// listSessions lists every main-session transcript under root, newest first.
// since > 0 keeps only those written within it.
func listSessions(root string, since time.Duration) ([]sessionFile, error) {
	projects, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	cutoff := time.Time{}
	if since > 0 {
		cutoff = time.Now().Add(-since)
	}
	var files []sessionFile
	for _, project := range projects {
		if !project.IsDir() {
			continue
		}
		dir := filepath.Join(root, project.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // a project dir removed or unreadable mid-listing holds nothing to find
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".jsonl") {
				continue
			}
			info, err := entry.Info()
			if err != nil || info.ModTime().Before(cutoff) {
				continue
			}
			files = append(files, sessionFile{
				Path:    filepath.Join(dir, name),
				ID:      strings.TrimSuffix(name, ".jsonl"),
				ModTime: info.ModTime(),
			})
		}
	}
	sortNewest(files)
	return files, nil
}

func sortNewest(files []sessionFile) {
	sort.Slice(files, func(i, j int) bool { return files[i].ModTime.After(files[j].ModTime) })
}

// scanned is a transcript that held at least need of the needles.
type scanned struct {
	File  sessionFile
	Found int
}

// scan reads every file and keeps those holding at least need of the
// needles, in parallel. Files that vanish mid-scan are skipped; any other
// read failure is returned.
func scan(files []sessionFile, needles []needle, need int) ([]scanned, error) {
	jobs := make(chan sessionFile)
	var (
		mu      sync.Mutex
		hits    []scanned
		failure error
		wg      sync.WaitGroup
	)
	for range min(runtime.GOMAXPROCS(0), 12) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf []byte
			for file := range jobs {
				var found int
				var err error
				buf, err = readInto(buf, file.Path)
				if err == nil {
					found = countNeedles(buf, needles, need)
				}
				mu.Lock()
				switch {
				case err != nil && !errors.Is(err, os.ErrNotExist):
					failure = errors.Join(failure, err)
				case found >= need:
					hits = append(hits, scanned{File: file, Found: found})
				}
				mu.Unlock()
			}
		}()
	}
	for _, file := range files {
		jobs <- file
	}
	close(jobs)
	wg.Wait()
	return hits, failure
}

// readInto reads a whole file into buf, growing it only when a file is
// larger than any before: one buffer per worker, no per-file allocation.
func readInto(buf []byte, path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return buf[:0], err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return buf[:0], err
	}
	if size := int(info.Size()); cap(buf) < size {
		buf = make([]byte, size)
	}
	n, err := io.ReadFull(f, buf[:info.Size()])
	if err == io.ErrUnexpectedEOF {
		err = nil // truncated mid-read: search what is there
	}
	return buf[:n], err
}

// countNeedles counts the needles data holds, giving up once need is out of
// reach — most files are rejected after len-need+1 passes.
func countNeedles(data []byte, needles []needle, need int) int {
	found := 0
	for i, needle := range needles {
		if found+len(needles)-i < need {
			break
		}
		if needle.in(data) {
			found++
		}
	}
	return found
}
