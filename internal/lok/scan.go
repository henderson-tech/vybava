package lok

import (
	"bytes"
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

// callRegex matches any configured call followed by a single- or
// double-quoted literal, across newlines and leading whitespace. A bare
// entry (`t`) matches `t(` only when no identifier character or dot precedes
// it, so `foo.t(` is not a hit; a method entry (`*.T`) matches `.T(` on any
// receiver — identifier, call result or index — so `l.T(`, `FromContext(ctx).N(`
// and `xs[i].T(` are, while a bare `T(` is not. Backtick templates are
// dynamic and deliberately not matched.
func callRegex(calls Calls) *regexp.Regexp {
	var bare, method []string
	for _, call := range calls {
		if name, ok := strings.CutPrefix(call, "*."); ok {
			method = append(method, regexp.QuoteMeta(name))
		} else {
			bare = append(bare, regexp.QuoteMeta(call))
		}
	}
	var alts []string
	if len(bare) > 0 {
		alts = append(alts, `(?:^|[^A-Za-z0-9_$.])(?:`+strings.Join(bare, "|")+`)`)
	}
	if len(method) > 0 {
		alts = append(alts, `[A-Za-z0-9_$)\]]\.(?:`+strings.Join(method, "|")+`)`)
	}
	return regexp.MustCompile(`(?s)(?:` + strings.Join(alts, "|") + `)\(\s*(?:'((?:[^'\\]|\\.)*)'|"((?:[^"\\]|\\.)*)")`)
}

func unescape(s string) string {
	return strings.NewReplacer(`\'`, `'`, `\"`, `"`, `\\`, `\`, `\n`, "\n").Replace(s)
}

// isTestSource reports test code by file name: Go `_test.go`, JS/TS
// `*.test.*` / `*.spec.*` (`__tests__` and `testdata` dirs are skipped by the
// walk). Test keys are synthetic by definition — a test asserts copy, it
// never defines a catalog key — so a test file is neither extracted nor
// counted as usage.
func isTestSource(name string) bool {
	if strings.HasSuffix(name, "_test.go") {
		return true
	}
	stem := strings.TrimSuffix(name, filepath.Ext(name))
	return strings.HasSuffix(stem, ".test") || strings.HasSuffix(stem, ".spec")
}

// blankComments overwrites every `//` and `/* */` comment with spaces
// (newlines kept), so an example call in a doc comment is never extracted.
// It steps over '…', "…" and `…` literals so a `//` inside one — a URL —
// stays code. '…' and "…" end at a newline, bounding a misread (a JS regex
// literal holding a quote) to its own line; a backslash outside a literal
// escapes the next byte, which only a regex literal (`/\/*/`) does; a `//`
// right after `:` is a URL in JSX text, never a comment. Go raw strings
// take no escapes.
func blankComments(src []byte, goSource bool) []byte {
	out := bytes.Clone(src)
	for i := 0; i < len(src); i++ {
		end := -1
		switch c := src[i]; {
		case c == '\\':
			i++
		case c == '\'' || c == '"' || c == '`':
			i = literalEnd(src, i, goSource && c == '`')
		case c == '/' && i+1 < len(src) && src[i+1] == '/' && (i == 0 || src[i-1] != ':'):
			if end = bytes.IndexByte(src[i:], '\n'); end < 0 {
				end = len(src)
			} else {
				end += i
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			if end = bytes.Index(src[i+2:], []byte("*/")); end < 0 {
				end = len(src)
			} else {
				end += i + 4
			}
		}
		if end < 0 {
			continue
		}
		for j := i; j < end; j++ {
			if out[j] != '\n' {
				out[j] = ' '
			}
		}
		i = end - 1
	}
	return out
}

// literalEnd returns the index of the byte closing the literal opened at
// src[i]: its quote, or — for '…' and "…" — the newline an unterminated one
// stops at.
func literalEnd(src []byte, i int, raw bool) int {
	q := src[i]
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			if !raw {
				j++
			}
		case q:
			return j
		case '\n':
			if q != '`' {
				return j
			}
		}
	}
	return len(src)
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
	calls := c.Config.Scan.Call
	if len(calls) == 0 {
		calls = Calls{"t"}
	}
	exts := c.Config.Scan.Extensions
	if len(exts) == 0 {
		exts = defaultExtensions
	}
	re := callRegex(calls)
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
				case "node_modules", ".git", "ios", "android", ".next", "dist", "build", ".expo", "__tests__", "testdata":
					return filepath.SkipDir
				}
				return nil
			}
			if !contains(exts, filepath.Ext(p)) || isTestSource(d.Name()) {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil {
				return nil
			}
			res.FilesScanned++
			corpus.Write(data)
			corpus.WriteByte('\n')
			for _, m := range re.FindAllSubmatch(blankComments(data, filepath.Ext(p) == ".go"), -1) {
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
