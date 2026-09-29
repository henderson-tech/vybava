package uiloop

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// harnessFS is the TypeScript harness every repo vendors. `sync` writes it
// verbatim into <dir>/vendor; never edit a vendored copy.
//
//go:embed harness
var harnessFS embed.FS

// StampFile records which vybava wrote vendor/ and every file's sha256.
const StampFile = "STAMP.json"

// Stamp is vendor/STAMP.json.
type Stamp struct {
	Vybava string            `json:"vybava"`
	Files  map[string]string `json:"files"`
}

// FileState is one vendored file against this binary's harness.
type FileState struct {
	File string `json:"file"`
	// State: ok | missing | outdated (as synced, from another harness) |
	// edited (changed after sync) | extra (not part of the harness).
	State string `json:"state"`
}

// VendorReport is sync's and check's view of vendor/.
type VendorReport struct {
	Dir     string      `json:"dir"`
	Clean   bool        `json:"clean"`
	Synced  string      `json:"syncedBy,omitempty"`
	Binary  string      `json:"binary"`
	Files   []FileState `json:"files"`
	Written []string    `json:"written,omitempty"`
	Removed []string    `json:"removed,omitempty"`
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// harnessFiles is the embedded harness, name → bytes, sorted by name.
func (t *Tool) harnessFiles() (map[string][]byte, []string, error) {
	files := map[string][]byte{}
	var names []string
	err := fs.WalkDir(t.Harness, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(t.Harness, p)
		if err != nil {
			return err
		}
		files[p] = b
		names = append(names, p)
		return nil
	})
	sort.Strings(names)
	return files, names, err
}

func readStamp(dir string) (*Stamp, error) {
	b, err := os.ReadFile(filepath.Join(dir, StampFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Stamp
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", StampFile, err)
	}
	return &s, nil
}

// Vendor compares <dir>/vendor with the embedded harness without writing.
func (t *Tool) Vendor() (VendorReport, error) {
	dir := t.VendorDir()
	rep := VendorReport{Dir: t.Config.Dir + "/vendor", Binary: t.Version, Clean: true, Files: []FileState{}}
	files, names, err := t.harnessFiles()
	if err != nil {
		return rep, err
	}
	stamp, err := readStamp(dir)
	if err != nil {
		return rep, err
	}
	if stamp != nil {
		rep.Synced = stamp.Vybava
	}
	for _, name := range names {
		state := "ok"
		have, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			state = "missing"
		case err != nil:
			return rep, err
		case sum(have) == sum(files[name]):
		case stamp != nil && stamp.Files[name] == sum(have):
			state = "outdated"
		case stamp != nil && stamp.Files[name] != "":
			state = "edited"
		default:
			state = "outdated"
		}
		if state != "ok" {
			rep.Clean = false
		}
		rep.Files = append(rep.Files, FileState{File: name, State: state})
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return rep, err
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == StampFile {
			continue
		}
		if _, ok := files[e.Name()]; !ok {
			rep.Clean = false
			rep.Files = append(rep.Files, FileState{File: e.Name(), State: "extra"})
		}
	}
	return rep, nil
}

// Sync writes the embedded harness into <dir>/vendor, removes files the
// harness no longer has, and rewrites the stamp. Edited files are
// overwritten only with force.
func (t *Tool) Sync(force bool) (Result, error) {
	before, err := t.Vendor()
	if err != nil {
		return Result{}, err
	}
	var edited []string
	for _, f := range before.Files {
		if f.State == "edited" {
			edited = append(edited, f.File)
		}
	}
	if len(edited) > 0 && !force {
		return Result{Data: before}, diag(DiagVendorEdited, fmt.Sprintf("vendored files were edited in the repo: %v", edited),
			"move the change into Výbava (internal/uiloop/harness), then `vybava ui-loop sync --force`")
	}
	dir := t.VendorDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Result{}, err
	}
	files, names, err := t.harnessFiles()
	if err != nil {
		return Result{}, err
	}
	stamp := Stamp{Vybava: t.Version, Files: map[string]string{}}
	rep := VendorReport{Dir: before.Dir, Binary: t.Version, Synced: t.Version, Clean: true, Files: []FileState{}}
	for _, name := range names {
		stamp.Files[name] = sum(files[name])
		target := filepath.Join(dir, filepath.FromSlash(name))
		if have, err := os.ReadFile(target); err == nil && sum(have) == stamp.Files[name] {
			rep.Files = append(rep.Files, FileState{File: name, State: "ok"})
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return Result{}, err
		}
		if err := os.WriteFile(target, files[name], 0o644); err != nil {
			return Result{}, err
		}
		rep.Written = append(rep.Written, name)
		rep.Files = append(rep.Files, FileState{File: name, State: "ok"})
	}
	for _, f := range before.Files {
		if f.State == "extra" {
			if err := os.Remove(filepath.Join(dir, f.File)); err != nil {
				return Result{}, err
			}
			rep.Removed = append(rep.Removed, f.File)
		}
	}
	// An unchanged vendor keeps its stamp: a release that did not touch the
	// harness must not dirty every repo that syncs.
	if old, err := readStamp(dir); err == nil && old != nil && len(rep.Written) == 0 && len(rep.Removed) == 0 && sameFiles(old.Files, stamp.Files) {
		rep.Synced = old.Vybava
		return Result{Data: rep, Next: []string{"vybava ui-loop check --json"}}, nil
	}
	b, err := json.MarshalIndent(stamp, "", "  ")
	if err != nil {
		return Result{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, StampFile), append(b, '\n'), 0o644); err != nil {
		return Result{}, err
	}
	return Result{Data: rep, Next: []string{"vybava ui-loop check --json"}}, nil
}

func sameFiles(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
