package lok

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ScanResult is what `lok scan` reports for one catalog. Missing keys are
// literal t('…') calls absent from the catalog — always a real defect, safe
// to add. Orphans are catalog keys never seen verbatim in source — a strong
// hint, never proof (keys held in lookup tables, API messages passed through
// t(), template strings), so they are listed, never deleted.
type ScanResult struct {
	Catalog      string   `json:"catalog"`
	FilesScanned int      `json:"filesScanned"`
	Calls        int      `json:"calls"`
	Missing      []string `json:"missing"`
	Added        []string `json:"added"`
	Orphans      []string `json:"orphans"`
	OrphanTotal  int      `json:"orphanTotal"`
	Written      []string `json:"written"`
}

var defaultExtensions = []string{".ts", ".tsx", ".js", ".jsx"}

// callRegex matches `<call>(` followed by a single- or double-quoted literal,
// across newlines and leading whitespace. Backtick templates are dynamic and
// deliberately not matched.
func callRegex(call string) *regexp.Regexp {
	return regexp.MustCompile(`(?s)(?:^|[^A-Za-z0-9_$.])` + regexp.QuoteMeta(call) + `\(\s*(?:'((?:[^'\\]|\\.)*)'|"((?:[^"\\]|\\.)*)")`)
}

func unescape(s string) string {
	return strings.NewReplacer(`\'`, `'`, `\"`, `"`, `\\`, `\`, `\n`, "\n").Replace(s)
}

// Scan runs the extractor for one english-as-key catalog. With write, the
// missing keys are added to every locale that can be derived (en = key);
// required locales without a derivable value stay absent and surface in
// `lok missing`, which is the intended follow-up for the translator.
func (t *Tool) Scan(catalogID string, write bool, orphanLimit int) (ScanResult, error) {
	c, err := t.CatalogFor(catalogID, "")
	if err != nil {
		return ScanResult{}, err
	}
	if c.Config.Scan == nil {
		return ScanResult{}, &Diag{Code: DiagConfigInvalid, Detail: fmt.Sprintf("catalog %s has no scan config", c.ID), Fix: "add scan: { roots: [...] } to the catalog in vybava.config.ts"}
	}
	call := c.Config.Scan.Call
	if call == "" {
		call = "t"
	}
	exts := c.Config.Scan.Extensions
	if len(exts) == 0 {
		exts = defaultExtensions
	}
	re := callRegex(call)
	res := ScanResult{Catalog: c.ID, Missing: []string{}, Added: []string{}, Orphans: []string{}, Written: []string{}}
	seen := map[string]bool{}
	var corpus strings.Builder
	for _, root := range c.Config.Scan.Roots {
		abs := filepath.Join(t.Root, root)
		err := filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				switch d.Name() {
				case "node_modules", ".git", "ios", "android", ".next", "dist", "build", ".expo":
					return filepath.SkipDir
				}
				return nil
			}
			if !contains(exts, filepath.Ext(p)) {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			res.FilesScanned++
			corpus.Write(data)
			corpus.WriteByte('\n')
			for _, m := range re.FindAllSubmatch(data, -1) {
				lit := m[1]
				if len(lit) == 0 {
					lit = m[2]
				}
				key := unescape(string(lit))
				if key == "" {
					continue
				}
				res.Calls++
				seen[key] = true
			}
			return nil
		})
		if err != nil {
			return res, err
		}
	}
	for key := range seen {
		if !c.has(key) {
			res.Missing = append(res.Missing, key)
		}
	}
	sort.Strings(res.Missing)
	src := corpus.String()
	bases := map[string]bool{}
	for _, k := range c.Keys() {
		base, _ := c.Config.BaseKey(k)
		if bases[base] {
			continue
		}
		bases[base] = true
		if !strings.Contains(src, base) {
			res.OrphanTotal++
			if len(res.Orphans) < orphanLimit {
				res.Orphans = append(res.Orphans, base)
			}
		}
	}
	if write && len(res.Missing) > 0 {
		for _, key := range res.Missing {
			if contains(c.Config.Locales, "en") {
				if err := c.Put("en", key, key); err != nil {
					return res, &Diag{Code: DiagConfigInvalid, Detail: err.Error()}
				}
			}
			res.Added = append(res.Added, key)
		}
		var locales []string
		if contains(c.Config.Locales, "en") {
			locales = []string{"en"}
		}
		wr, err := t.commit(c, "", locales)
		if err != nil {
			return res, err
		}
		res.Written = wr.Written
	}
	return res, nil
}
