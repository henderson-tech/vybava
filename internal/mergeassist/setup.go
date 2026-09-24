package mergeassist

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/henderson-tech/vybava/internal/mergeassist/gitmerge"
)

// The config is the source of truth: `setup` renders the attribute block into
// the clone's info/attributes and registers the drivers in its git config —
// both local, both shared by every worktree, nothing tracked. A committed
// .gitattributes would not do: git reads attributes from the checked-out
// branch, so a branch predating the block would merge main without drivers.
// `merge-assist merge` runs setup itself; `setup --check` reports drift.
const (
	blockBegin = "# >>> vybava merge-assist: generated from vybava.config.ts by `merge-assist setup`; do not edit"
	blockEnd   = "# <<< vybava merge-assist"

	driverLok       = "lok"
	driverGenerated = "vybava-generated"
)

// drivers are the git config entries setup owns.
var drivers = [][2]string{
	{"merge." + driverLok + ".name", "vybava lok: merge locale catalogs by key"},
	{"merge." + driverLok + ".driver", "vybava lok merge-driver %O %A %B %P"},
	{"merge." + driverGenerated + ".name", "vybava merge-assist: take theirs, regenerate after the merge"},
	{"merge." + driverGenerated + ".driver", "vybava merge-assist driver %O %A %B %P"},
}

// SetupResult reports what setup changed (or, with check, what drifted).
type SetupResult struct {
	Attributes []string `json:"attributes"`
	Changed    []string `json:"changed"`
}

// Block renders the managed attribute lines.
func (t *Tool) Block() []string {
	var lines []string
	if t.Lok != nil {
		for _, id := range t.Lok.CatalogIDs() {
			cfg := t.Lok.Config.Catalogs[id]
			for _, code := range cfg.Locales {
				lines = append(lines, attrPath(strings.ReplaceAll(cfg.Files, "{locale}", code))+" merge="+driverLok)
			}
		}
	}
	for _, g := range t.Config.Generated {
		for _, p := range g.Paths {
			lines = append(lines, attrPath(p)+" merge="+driverGenerated)
		}
	}
	sort.Strings(lines)
	return lines
}

// attrPath anchors a repository path; gitattributes matches a slash-free
// pattern at any depth, which a config path never means.
func attrPath(p string) string {
	if strings.HasPrefix(p, "**/") || strings.HasPrefix(p, "/") {
		return p
	}
	return "/" + p
}

// Setup writes the block and registers the drivers; check only reports.
func (t *Tool) Setup(check bool) (SetupResult, error) {
	res := SetupResult{Attributes: t.Block(), Changed: []string{}}
	common, err := t.git("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return res, err
	}
	path := filepath.Join(strings.TrimSpace(common), "info", "attributes")
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return res, err
	}
	next := spliceBlock(string(old), res.Attributes)
	if next != string(old) {
		res.Changed = append(res.Changed, path)
		if !check {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return res, err
			}
			if err := os.WriteFile(path, []byte(next), 0o644); err != nil {
				return res, err
			}
		}
	}
	for _, kv := range drivers {
		cur, _ := t.git("config", "--local", "--get", kv[0]) // unset exits 1
		if strings.TrimSpace(cur) == kv[1] {
			continue
		}
		res.Changed = append(res.Changed, "git config "+kv[0])
		if !check {
			if _, err := t.git("config", "--local", kv[0], kv[1]); err != nil {
				return res, err
			}
		}
	}
	if check && len(res.Changed) > 0 {
		return res, &Diag{Code: DiagSetupDrift, Detail: strings.Join(res.Changed, ", ") + " out of date with vybava.config.ts", Fix: "merge-assist setup"}
	}
	return res, nil
}

// spliceBlock replaces the managed block, or appends it; an empty block
// removes it.
func spliceBlock(content string, lines []string) string {
	var block string
	if len(lines) > 0 {
		block = blockBegin + "\n" + strings.Join(lines, "\n") + "\n" + blockEnd + "\n"
	}
	if i := strings.Index(content, blockBegin); i >= 0 {
		if j := strings.Index(content[i:], blockEnd); j >= 0 {
			end := i + j + len(blockEnd)
			if end < len(content) && content[end] == '\n' {
				end++
			}
			return content[:i] + block + content[end:]
		}
	}
	if block == "" {
		return content
	}
	if content != "" && !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	if content != "" {
		content += "\n"
	}
	return content + block
}

// Driver is `merge-assist driver %O %A %B %P` for generated paths: theirs
// wins outright and the group's regen is journaled for after the merge.
// A path no group owns gets git's text merge.
func Driver(dir, base, ours, theirs, rel string, stderr io.Writer) (conflict bool, err error) {
	t, err := Open(dir)
	var g Generated
	ok := false
	if err == nil {
		g, ok = t.GeneratedFor(rel)
	}
	if !ok {
		reason := "no merge.generated group owns it"
		if err != nil {
			reason = err.Error()
		}
		fmt.Fprintf(stderr, "merge-assist driver: %s: %s; text merge\n", rel, reason)
		return gitmerge.TextMerge(dir, base, ours, theirs, stderr)
	}
	data, err := os.ReadFile(theirs)
	if err != nil {
		return false, err
	}
	if err := os.WriteFile(ours, data, 0o644); err != nil {
		return false, err
	}
	if err := gitmerge.Record(dir, gitmerge.Event{Path: rel, Class: gitmerge.ClassGenerated, Outcome: gitmerge.OutcomeResolved, Regen: g.Regen}); err != nil {
		fmt.Fprintf(stderr, "merge-assist driver: journal: %v; run `%s` after the merge\n", err, g.Regen)
	}
	return false, nil
}
