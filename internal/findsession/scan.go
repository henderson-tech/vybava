package findsession

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/henderson-tech/vybava/internal/transcripts"
)

// sessionFile is one main-session transcript: <root>/<project>/<id>.jsonl.
// Subagent transcripts are never listed — only a main session reopens.
type sessionFile struct {
	Path    string
	ID      string
	ModTime time.Time
}

// uuid is the only file name listed: the id reaches the reopen line, so a
// name a shell would split or expand is never a session.
var uuid = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// listSessions lists every main-session transcript under root, newest first.
// since > 0 keeps only those written within it.
func listSessions(root string, since time.Duration) ([]sessionFile, error) {
	if _, err := os.Stat(root); err != nil {
		return nil, err
	}
	walked, _, err := transcripts.WalkClaude(root)
	if err != nil {
		return nil, err
	}
	cutoff := time.Time{}
	if since > 0 {
		cutoff = time.Now().Add(-since)
	}
	var files []sessionFile
	for _, f := range walked {
		id := strings.TrimSuffix(filepath.Base(f.Path), ".jsonl")
		if f.Kind != transcripts.ClaudeSession || !uuid.MatchString(id) || f.Info.ModTime().Before(cutoff) {
			continue
		}
		files = append(files, sessionFile{Path: f.Path, ID: id, ModTime: f.Info.ModTime()})
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
	overlap := 0
	for _, n := range needles {
		overlap = max(overlap, len(n.text)-1)
	}
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
			buf := make([]byte, chunkSize+overlap)
			for file := range jobs {
				found, err := scanFile(file.Path, needles, need, buf)
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

// chunkSize bounds a scan worker's memory: a file streams through one
// fixed buffer, so scan memory never grows with the largest transcript.
var chunkSize = 8 << 20

// scanFile counts the needles a file holds, streaming it through buf in
// chunks that overlap by the longest needle less one byte, so a needle
// across a chunk boundary still matches. In the last chunk it gives up once
// need is out of reach — a typical transcript is one chunk, rejected after
// len-need+1 passes.
func scanFile(path string, needles []needle, need int, buf []byte) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	overlap := len(buf) - chunkSize
	seen := make([]bool, len(needles))
	found, carry := 0, 0
	for {
		n, err := io.ReadFull(f, buf[carry:])
		last := err == io.EOF || err == io.ErrUnexpectedEOF
		if err != nil && !last {
			return found, err
		}
		window := buf[:carry+n]
		unseen := len(needles) - found
		for i, needle := range needles {
			if seen[i] {
				continue
			}
			if last && found+unseen < need {
				return found, nil
			}
			unseen--
			if needle.in(window) {
				seen[i] = true
				found++
			}
		}
		if last || found == len(needles) {
			return found, nil
		}
		carry = min(overlap, len(window))
		copy(buf, window[len(window)-carry:])
	}
}
