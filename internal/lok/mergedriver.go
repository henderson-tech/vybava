package lok

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/henderson-tech/vybava/internal/mergeassist/gitmerge"
)

// DiagMergeClash — a catalog key both sides changed differently; rerun with --prefer.
const DiagMergeClash = "MERGE_CLASH"

// CatalogAt resolves a repository-relative path to its declared catalog.
func (t *Tool) CatalogAt(rel string) (string, CatalogConfig, bool) {
	for _, id := range t.CatalogIDs() {
		cfg := t.Config.Catalogs[id]
		for _, code := range cfg.Locales {
			if strings.ReplaceAll(cfg.Files, "{locale}", code) == rel {
				return id, cfg, true
			}
		}
	}
	return "", CatalogConfig{}, false
}

// MergeDriver is `lok merge-driver %O %A %B %P`: git's contract is to leave
// the result in ours and report a conflict by exit status. Only a declared
// catalog in lok's canonical format is merged by key; anything else gets
// git's own text merge, so registering the driver never loses behaviour.
func MergeDriver(dir, base, ours, theirs, rel string, stderr io.Writer) (conflict bool, err error) {
	// clash forces a conflict: git's line merge happily keeps both sides' copy
	// of a key added at different lines — duplicate keys, the last silently wins.
	textMerge := func(reason string, journal, clash bool) (bool, error) {
		fmt.Fprintf(stderr, "lok merge-driver: %s: %s; text merge\n", rel, reason)
		conflict, err := gitmerge.TextMerge(dir, base, ours, theirs, stderr)
		conflict = conflict || clash
		if err != nil || !journal {
			return conflict, err
		}
		e := gitmerge.Event{Path: rel, Class: gitmerge.ClassCatalog, Outcome: gitmerge.OutcomeResolved, Detail: reason}
		if conflict {
			e.Outcome = gitmerge.OutcomeConflict
		}
		record(dir, e, stderr)
		return conflict, nil
	}
	t, err := Open(dir)
	if err != nil {
		return textMerge(err.Error(), false, false)
	}
	_, cfg, ok := t.CatalogAt(rel)
	if !ok {
		return textMerge("not a declared catalog", false, false)
	}
	var sides [3][]byte
	for i, p := range []string{base, ours, theirs} {
		if sides[i], err = os.ReadFile(p); err != nil {
			return false, err
		}
	}
	data, clashes, err := MergeCatalog(sides[0], sides[1], sides[2], PreferNone)
	if err != nil {
		return textMerge(err.Error(), true, errors.Is(err, ErrDuplicateKey))
	}
	if len(clashes) > 0 {
		for i, c := range clashes {
			if i == 20 {
				fmt.Fprintf(stderr, "  … %d more\n", len(clashes)-i)
				break
			}
			fmt.Fprintf(stderr, "  clash %s\n", c)
		}
		return textMerge(fmt.Sprintf("%d clashing keys; settle with `lok merge %s --prefer ours|theirs`", len(clashes), rel), true, true)
	}
	if err := os.WriteFile(ours, data, 0o644); err != nil {
		return false, err
	}
	e := gitmerge.Event{Path: rel, Class: gitmerge.ClassCatalog, Outcome: gitmerge.OutcomeResolved}
	if !bytes.Equal(data, sides[2]) {
		e.Regen = cfg.AfterWrite // theirs' generated types no longer match
	}
	record(dir, e, stderr)
	return false, nil
}

func (c Clash) String() string {
	show := func(v string) string {
		if v == "" {
			return "(absent)"
		}
		return v
	}
	return fmt.Sprintf("%q: base=%s ours=%s theirs=%s", c.Key, show(c.Base), show(c.Ours), show(c.Theirs))
}

// record journals a driver outcome; a journal failure never fails the merge
// itself, but it is said out loud.
func record(dir string, e gitmerge.Event, stderr io.Writer) {
	if err := gitmerge.Record(dir, e); err != nil {
		fmt.Fprintf(stderr, "lok merge-driver: journal: %v (merge-assist status will not list %s)\n", err, e.Path)
	}
}

// MergeResult is `lok merge`'s receipt.
type MergeResult struct {
	Path    string  `json:"path"`
	Catalog string  `json:"catalog"`
	Prefer  Prefer  `json:"prefer,omitempty"`
	Clashes []Clash `json:"clashes"`
	Regen   string  `json:"regen,omitempty"`
}

// MergeIndexed re-merges an unmerged catalog from the index stages (1 base,
// 2 ours, 3 theirs), writes and stages it — the way out of a driver clash,
// and of a catalog conflict left by a merge that ran without the driver.
func (t *Tool) MergeIndexed(rel string, prefer Prefer) (MergeResult, error) {
	id, cfg, ok := t.CatalogAt(rel)
	if !ok {
		return MergeResult{}, &Diag{Code: DiagCatalogUnknown, Detail: rel + " is not a declared catalog file", Fix: "lok catalogs"}
	}
	var sides [3][]byte
	for i := range sides {
		out, err := t.git("show", fmt.Sprintf(":%d:%s", i+1, rel))
		if err != nil {
			if i == 0 {
				continue // no common ancestor: both sides added the file
			}
			return MergeResult{}, &Diag{Code: DiagMergeClash, Detail: fmt.Sprintf("%s has no stage %d (not unmerged, or deleted on one side)", rel, i+1), Fix: "git status -- " + shellQuote(rel)}
		}
		sides[i] = out
	}
	data, clashes, err := MergeCatalog(sides[0], sides[1], sides[2], prefer)
	if err != nil {
		return MergeResult{}, &Diag{Code: DiagConfigInvalid, Detail: fmt.Sprintf("%s: %v", rel, err)}
	}
	res := MergeResult{Path: rel, Catalog: id, Prefer: prefer, Clashes: clashes}
	if res.Clashes == nil {
		res.Clashes = []Clash{}
	}
	if data == nil {
		lines := make([]string, len(clashes))
		for i, c := range clashes {
			lines[i] = c.String()
		}
		return res, &Diag{Code: DiagMergeClash, Detail: strings.Join(lines, "; "), Fix: "lok merge " + shellQuote(rel) + " --prefer ours|theirs, then lok set the keys that need a mix"}
	}
	if err := os.WriteFile(filepath.Join(t.Root, filepath.FromSlash(rel)), data, 0o644); err != nil {
		return res, err
	}
	if _, err := t.git("add", "--", rel); err != nil {
		return res, err
	}
	if !bytes.Equal(data, sides[2]) {
		res.Regen = cfg.AfterWrite
	}
	if err := gitmerge.Record(t.Root, gitmerge.Event{Path: rel, Class: gitmerge.ClassCatalog, Outcome: gitmerge.OutcomeResolved, Detail: "lok merge", Regen: res.Regen}); err != nil {
		return res, fmt.Errorf("journal: %w", err)
	}
	return res, nil
}

func (t *Tool) git(args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = t.Root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out, fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	return out, err
}
