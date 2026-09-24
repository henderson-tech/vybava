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

// isTestSource reports test code by file name: Go `_test.go`, JS/TS with a
// `.test.` / `.spec.` segment anywhere (`a.test.ts`, `a.spec.gen.ts`);
// `__tests__` and `testdata` dirs are skipped by the walk. Test keys are
// synthetic by definition — a test asserts copy, it never defines a catalog
// key — so a test file is neither extracted nor counted as usage.
func isTestSource(name string) bool {
	return strings.HasSuffix(name, "_test.go") || strings.Contains(name, ".test.") || strings.Contains(name, ".spec.")
}

// blankComments overwrites every `//` and `/* */` comment with spaces
// (newlines kept), so an example call in a doc comment is never extracted.
// A small lexer: it steps over '…' and "…" literals, JS regex literals
// (classes included) and template text, and descends into `${…}`, which is
// code. Where it cannot tell, it fails toward scanning too much, never toward
// blanking code: '…', "…" and a regex end at a newline, a suspect JS `/*`
// left open on its line is not a comment (blockEnd), and a `//` right after
// `:` is a URL in JSX text. A backslash in code escapes the
// next byte (only an unrecognised regex holds one). Go has no regex or
// template literals and its raw strings take no escapes.
func blankComments(src []byte, goSource bool) []byte {
	out := bytes.Clone(src)
	var interp []int // brace depth inside each open `${`, innermost last
	last := -1       // the last significant code byte: regex or division?
	template := func(j int) int {
		end, open := templateEnd(src, j)
		if open {
			interp = append(interp, 0)
		}
		return end
	}
	for i := 0; i < len(src); i++ {
		end := -1
		switch c := src[i]; {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			continue
		case c == '/' && i+1 < len(src) && src[i+1] == '/' && (i == 0 || src[i-1] != ':'):
			if end = bytes.IndexByte(src[i:], '\n'); end < 0 {
				end = len(src)
			} else {
				end += i
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*': // before the regex case: no regex starts with `*`
			suspect := !goSource && last >= 0 && bytes.IndexByte(src[last:i], '\n') < 0 &&
				(src[last] == '[' || !regexAllowed(src, last))
			end = blockEnd(src, i, suspect)
		case c == '/' && !goSource && regexAllowed(src, last):
			i = regexEnd(src, i)
		case c == '`' && !goSource:
			i = template(i + 1)
		case c == '\'' || c == '"' || c == '`':
			i = literalEnd(src, i, c == '`')
		case c == '{' && len(interp) > 0:
			interp[len(interp)-1]++
		case c == '}' && len(interp) > 0:
			if top := len(interp) - 1; interp[top] > 0 {
				interp[top]--
			} else {
				interp = interp[:top]
				i = template(i + 1)
			}
		case c == '\\':
			i++
		}
		if end < 0 {
			last = i
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

// blockEnd returns the end of the block comment opened at src[i]. In code a
// `/*` always opens one (Go has no regex; in JS no regex starts with `*`), so
// the only doubt is a `/*` the lexer reached while not really in code: a
// regex class it failed to recognise (`) /[/*]/`) or JSX text (`src/*`).
// That is suspect — JS, with `[` or a value before it on its line — and a
// suspect `/*` not closed on its own line returns -1 and stays code, since
// misread as a comment it would blank real code up to the next `*/`.
func blockEnd(src []byte, i int, suspect bool) int {
	end := bytes.Index(src[i+2:], []byte("*/"))
	if nl := bytes.IndexByte(src[i:], '\n'); suspect && (end < 0 || nl >= 0 && end+2 > nl) {
		return -1
	}
	if end < 0 {
		return len(src)
	}
	return i + 2 + end + 2
}

// regexAllowed reports whether a `/` after the code byte src[last] opens a
// regex literal rather than dividing: after an operator or opening
// punctuation, an expression keyword, or at the start of the file.
func regexAllowed(src []byte, last int) bool {
	if last < 0 || strings.IndexByte("(,=:[!&|?{};+-%^~>", src[last]) >= 0 {
		return true
	}
	start := last + 1
	for start > 0 && isIdentByte(src[start-1]) {
		start--
	}
	switch string(src[start : last+1]) {
	case "return", "typeof", "case", "do", "else", "in", "of", "new", "delete", "void", "throw", "instanceof", "yield", "await":
		return true
	}
	return false
}

func isIdentByte(c byte) bool {
	return c == '_' || c == '$' || c >= 0x80 || c >= '0' && c <= '9' || c|0x20 >= 'a' && c|0x20 <= 'z'
}

// regexEnd returns the index of the `/` closing the regex literal opened at
// src[i] — one inside a [class] does not — or of the byte before the newline
// a misread stops at.
func regexEnd(src []byte, i int) int {
	class := false
	for j := i + 1; j < len(src); j++ {
		switch src[j] {
		case '\\':
			if j+1 < len(src) && src[j+1] != '\n' {
				j++
			}
		case '[':
			class = true
		case ']':
			class = false
		case '/':
			if !class {
				return j
			}
		case '\n':
			return j - 1
		}
	}
	return len(src)
}

// templateEnd scans template text from src[j] to its closing backtick, or to
// the `{` of a `${` interpolation (open), whose code the caller lexes.
func templateEnd(src []byte, j int) (int, bool) {
	for ; j < len(src); j++ {
		switch src[j] {
		case '\\':
			j++
		case '`':
			return j, false
		case '$':
			if j+1 < len(src) && src[j+1] == '{' {
				return j + 1, true
			}
		}
	}
	return len(src), false
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
