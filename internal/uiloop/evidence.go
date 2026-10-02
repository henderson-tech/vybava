package uiloop

// Pass evidence is read from files, not reconstructed from agent summaries.
import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
)

type captureEvidence struct {
	HeadSHA string `json:"headSha"`
}

func (t *Tool) git(args ...string) (CmdOut, error) {
	return RealExec(context.Background(), Cmd{Dir: t.Root, Args: append([]string{"git"}, args...)})
}

// isRevision accepts a full SHA-1 or SHA-256 object name, never a ref.
func isRevision(s string) bool {
	_, err := hex.DecodeString(s)
	return (len(s) == 40 || len(s) == 64) && err == nil
}

func (t *Tool) sourceUnchanged(head string) (bool, error) {
	if !isRevision(head) {
		return false, nil
	}
	out, err := t.git("diff", "--name-only", head, "--")
	if err != nil {
		return false, err
	}
	if out.Code != 0 {
		return false, nil
	}
	files := strings.Split(strings.TrimSpace(out.Stdout), "\n")
	out, err = t.git("ls-files", "--others", "--exclude-standard")
	if err != nil {
		return false, err
	}
	if out.Code != 0 {
		return false, nil
	}
	files = append(files, strings.Split(strings.TrimSpace(out.Stdout), "\n")...)
	for _, file := range files {
		if file != "" && file != t.Config.Out && !strings.HasPrefix(file, strings.TrimRight(t.Config.Out, "/")+"/") && file != ".vitrinka" && !strings.HasPrefix(file, ".vitrinka/") {
			return false, nil
		}
	}
	return true, nil
}

func (t *Tool) backlogEvidenceCurrent(pass int, file string, knownBasis ...string) (bool, error) {
	var marker captureEvidence
	found, err := readJSON(filepath.Join(t.passAbs(pass), "capture.json"), &marker)
	if err != nil {
		return false, err
	}
	if !found {
		return true, nil
	}
	basis, err := t.cachedReviewBasis(pass, knownBasis)
	if err != nil {
		return false, err
	}
	var receipt struct {
		Basis string `json:"basis"`
	}
	if _, err := readJSON(filepath.Join(filepath.Dir(file), "basis.json"), &receipt); err != nil {
		return false, err
	}
	return receipt.Basis == basis, nil
}

// Persist BEFORE the interruptible runner. A resume never replaces the original revision.
func (t *Tool) captureProvenance(pass int, resume bool) error {
	file := filepath.Join(t.passAbs(pass), "capture.json")
	var marker captureEvidence
	found, err := readJSON(file, &marker)
	if err != nil {
		return err
	}
	if resume && found {
		clean, err := t.sourceUnchanged(marker.HeadSHA)
		if err != nil {
			return err
		}
		if !clean {
			return diag(DiagSelectionInvalid, "application source differs from the captured revision", "capture a new pass without --resume")
		}
		return nil
	}
	if resume {
		return nil
	} // Legacy CLI passes remain usable; workflows require provenance.
	head, err := t.git("rev-parse", "--verify", "HEAD")
	if err != nil {
		return err
	}
	if head.Code != 0 {
		return nil
	} // Captures outside git have no verified source revision.
	marker.HeadSHA = strings.TrimSpace(head.Stdout)
	clean, err := t.sourceUnchanged(marker.HeadSHA)
	if err != nil {
		return err
	}
	if !clean {
		return diag(DiagSelectionInvalid, "application source has uncommitted changes", "commit the application changes before capture")
	}
	return writeJSON(file, marker)
}

// Content basis: compact JSON of sorted [relative filename, SHA256(bytes)]
// pairs over the pass's shots, the manifest (*.ts under Config.Dir, vendor/
// skipped) and the spec. A pass's evidence is immutable: with provenance its
// manifest is read at the captured revision, so a rig repair committed after
// capture never stales the reviews, backlog and checkpoints of that pass. The
// spec is the owner's live rule set and stays a working-tree read.
func (t *Tool) reviewBasis(pass int) (string, error) {
	basis, _, err := t.reviewEvidence(pass)
	return basis, err
}

func (t *Tool) cachedReviewBasis(pass int, known []string) (string, error) {
	if len(known) > 0 {
		return known[0], nil
	}
	return t.reviewBasis(pass)
}

func (t *Tool) reviewEvidence(pass int) (string, map[string]string, error) {
	entries := [][2]string{}
	hashes := map[string]string{}
	add := func(file string) error {
		body, err := os.ReadFile(file)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(t.Root, file)
		if err != nil {
			return err
		}
		hashes[filepath.ToSlash(rel)] = digest(string(body), 64)
		entries = append(entries, [2]string{filepath.ToSlash(rel), hashes[filepath.ToSlash(rel)]})
		return nil
	}
	dirs := []string{filepath.Join(t.passAbs(pass), "shots")}
	var marker captureEvidence
	strict, err := readJSON(filepath.Join(t.passAbs(pass), "capture.json"), &marker)
	if err != nil {
		return "", nil, err
	}
	committed := false
	if strict && isRevision(marker.HeadSHA) {
		var manifest map[string]string
		if manifest, committed, err = t.committedManifest(marker.HeadSHA); err != nil {
			return "", nil, err
		}
		for rel, hash := range manifest {
			hashes[rel] = hash
			entries = append(entries, [2]string{rel, hash})
		}
	}
	if !committed {
		dirs = append(dirs, t.abs(t.Config.Dir))
	}
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(file string, d fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "vendor" {
					return filepath.SkipDir
				}
				return nil
			}
			ext := filepath.Ext(file)
			if (strings.HasPrefix(file, filepath.Join(t.passAbs(pass), "shots")+string(filepath.Separator)) && (ext == ".json" || ext == ".png")) || ext == ".ts" {
				return add(file)
			}
			return nil
		})
		if err != nil {
			return "", nil, err
		}
	}
	if t.Config.Spec != "" {
		if err := add(t.abs(t.Config.Spec)); err != nil {
			return "", nil, err
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i][0] < entries[j][0] })
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(entries); err != nil {
		return "", nil, err
	}
	return digest(strings.TrimSuffix(b.String(), "\n"), 64), hashes, nil
}

// committedManifest hashes the manifest's blobs at head, keyed like the
// working-tree walk (paths relative to Root). ok is false only when git cannot
// list head here (a commit this clone lacks, a Config.Dir outside the repo):
// the caller then reads the working tree as a legacy pass does.
func (t *Tool) committedManifest(head string) (hashes map[string]string, ok bool, err error) {
	dir, err := filepath.Rel(t.Root, t.abs(t.Config.Dir))
	if err != nil {
		return nil, false, err
	}
	dir = filepath.ToSlash(dir)
	tree, err := t.git("ls-tree", "-r", "-z", head, "--", dir)
	if err != nil || tree.Code != 0 {
		return nil, false, err
	}
	var files, oids []string
	for _, row := range strings.Split(tree.Stdout, "\x00") {
		meta, file, _ := strings.Cut(row, "\t")
		f := strings.Fields(meta) // mode, type, object
		if len(f) != 3 || f[1] != "blob" || path.Ext(file) != ".ts" ||
			slices.Contains(strings.Split(path.Dir(strings.TrimPrefix(file, dir+"/")), "/"), "vendor") {
			continue
		}
		files, oids = append(files, file), append(oids, f[2])
	}
	hashes = map[string]string{}
	if len(files) == 0 {
		return hashes, true, nil
	}
	out, err := RealExec(context.Background(), Cmd{Dir: t.Root, Args: []string{"git", "cat-file", "--batch"}, Stdin: strings.NewReader(strings.Join(oids, "\n") + "\n")})
	if err != nil {
		return nil, false, err
	}
	if out.Code != 0 {
		return nil, false, fmt.Errorf("git cat-file --batch at %s: exit %d: %s", head, out.Code, strings.TrimSpace(out.Stderr))
	}
	rest := out.Stdout
	for i, file := range files {
		// Each record is "<object> <type> <size>\n<size bytes>\n".
		header, body, _ := strings.Cut(rest, "\n")
		f := strings.Fields(header)
		size := -1
		if len(f) == 3 && f[0] == oids[i] {
			if n, err := strconv.Atoi(f[2]); err == nil {
				size = n
			}
		}
		if size < 0 || len(body) <= size {
			return nil, false, fmt.Errorf("git cat-file --batch at %s: %s: unexpected record %q", head, file, header)
		}
		hashes[file] = digest(body[:size], 64)
		rest = body[size+1:]
	}
	return hashes, true, nil
}

func (t *Tool) scoreBasis(pass int, basis string) (string, error) {
	parts := []string{basis}
	for _, file := range []string{"review/backlog.json", "publish/index.json", "publish/boards.json", "scoreboard.json", "scoreboard.md"} {
		b, err := os.ReadFile(filepath.Join(t.passAbs(pass), file))
		if errors.Is(err, fs.ErrNotExist) {
			parts = append(parts, "")
			continue
		}
		if err != nil {
			return "", err
		}
		parts = append(parts, digest(string(b), 64))
	}
	return digest(strings.Join(parts, "\n"), 64), nil
}

func (t *Tool) validCheckpoint(c Checkpoint, basis string) (bool, error) {
	if c.Basis != basis || !isRevision(c.Commit) {
		return false, nil
	}
	out, err := t.git("merge-base", "--is-ancestor", c.Commit, "HEAD")
	if err != nil {
		return false, err
	}
	if out.Code != 0 {
		return false, nil
	}
	if c.Status != "done" {
		return t.sourceUnchanged(c.Commit)
	}
	if len(c.FileDigests) == 0 {
		return false, nil
	}
	for file, hash := range c.FileDigests {
		rel, ok := repoRel(t.Root, file)
		if !ok {
			return false, nil
		}
		b, err := os.ReadFile(t.abs(rel))
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if digest(string(b), 64) != hash {
			return false, nil
		}
	}
	return true, nil
}
