package lok

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/henderson-tech/vybava/internal/shellword"
)

// ---------------------------------------------------------------------------
// `lok sub` - a regex rewrite over catalog values (and, with --keys, over
// english-as-key keys; see rename.go). A dry run by default: the preview IS
// the review, and `--write --expect <n>` applies exactly the count that was
// reviewed. Every invariant is checked in memory, over every catalog, before
// a single byte is written; any refusal writes nothing anywhere.
// ---------------------------------------------------------------------------

// SubOptions is one `lok sub` (or `lok mv`) invocation.
type SubOptions struct {
	Pattern     string
	Replacement string
	Catalogs    []string // empty: every catalog
	Locales     []string // empty: every locale (values mode only)
	Key         string   // RE2 over the canonical key (the base key in keys mode)
	ExcludeKey  string
	Literal     bool
	IgnoreCase  bool
	Write       bool
	Expect      int // < 0: unset
	Limit       int
	Refs        bool

	Keys          bool
	Merge         bool
	WithMirrors   bool
	NoSource      bool
	AllowLiterals bool
}

// SubTotal is complete even when the listed changes are capped.
type SubTotal struct {
	Values      int `json:"values"`
	Keys        int `json:"keys"`
	Occurrences int `json:"occurrences"`
	Skipped     int `json:"skipped"`
	Renames     int `json:"renames"`
	// Key renames: what the listed callSites / literals / mirrors add up to
	// before --limit cuts the lists.
	CallSites     int `json:"callSites"`
	TestCallSites int `json:"testCallSites"`
	Literals      int `json:"literals"`
	Mirrors       int `json:"mirrors"`
}

// SubChange is one rewritten value.
type SubChange struct {
	Catalog     string `json:"catalog"`
	Key         string `json:"key"`
	Locale      string `json:"locale"`
	Before      string `json:"before"`
	After       string `json:"after"`
	Occurrences int    `json:"occurrences"`
}

// SubSkip is a value the pattern matches but values mode may not rewrite.
type SubSkip struct {
	Catalog string `json:"catalog"`
	Key     string `json:"key"`
	Locale  string `json:"locale"`
	Reason  string `json:"reason"`
}

// SubCatalog is one catalog's share of a run: changed values per locale,
// the files a write landed and its afterWrite outcome.
type SubCatalog struct {
	Catalog    string         `json:"catalog"`
	Locales    map[string]int `json:"locales"`
	Written    []string       `json:"written"`
	AfterWrite *AfterWrite    `json:"afterWrite"`
	order      []string
	cat        *Catalog
}

// SourceEdit is one literal rewritten (or, in a dry run, to be rewritten)
// in a source file.
type SourceEdit struct {
	File string `json:"file"`
	Line int    `json:"line"`
	From string `json:"from"`
	To   string `json:"to"`
	Test bool   `json:"test,omitempty"`
}

// SourceLine is one place in a source file that holds a text verbatim.
type SourceLine struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// SubResult is the receipt of `lok sub` / `lok mv`, dry run or write.
type SubResult struct {
	Pattern       string       `json:"pattern"`
	Replacement   string       `json:"replacement"`
	Mode          string       `json:"mode"`
	Write         bool         `json:"write"`
	Total         SubTotal     `json:"total"`
	ByCatalog     []SubCatalog `json:"byCatalog"`
	Changes       []SubChange  `json:"changes"`
	Skipped       []SubSkip    `json:"skipped"`
	Renames       []Rename     `json:"renames"`
	CallSites     []SourceEdit `json:"callSites"`
	Literals      []SourceLine `json:"literals"`
	Mirrors       []SourceEdit `json:"mirrors"`
	MirrorSpecs   []SourceLine `json:"mirrorSpecs"`
	Refs          []SourceLine `json:"refs"`
	RefsTotal     int          `json:"refsTotal"`
	StillMatching int          `json:"stillMatching"`
	Truncated     bool         `json:"truncated"`
	// Warnings are advisory diagnostics (severity warning) the envelope carries.
	Warnings []*Diag `json:"-"`
}

func newSubResult(o SubOptions, mode string) SubResult {
	return SubResult{Pattern: o.Pattern, Replacement: o.Replacement, Mode: mode, ByCatalog: []SubCatalog{}, Changes: []SubChange{}, Skipped: []SubSkip{},
		Renames: []Rename{}, CallSites: []SourceEdit{}, Literals: []SourceLine{}, Mirrors: []SourceEdit{}, MirrorSpecs: []SourceLine{}, Refs: []SourceLine{}}
}

// subPlan is the compiled pattern, replacement and key filters.
type subPlan struct {
	re        *regexp.Regexp
	repl      string
	literal   bool
	keyRe     *regexp.Regexp
	excludeRe *regexp.Regexp
}

func (p *subPlan) apply(s string) string {
	if p.literal {
		return p.re.ReplaceAllLiteralString(s, p.repl)
	}
	return p.re.ReplaceAllString(s, p.repl)
}

func (p *subPlan) keyInScope(key string) bool {
	return (p.keyRe == nil || p.keyRe.MatchString(key)) && (p.excludeRe == nil || !p.excludeRe.MatchString(key))
}

// compileSub validates the pattern, the replacement template and the key
// filters. -F quotes the pattern and takes the replacement literally; both
// decode \x{HHHH} and \\ so an invisible character is typed visibly.
func compileSub(o SubOptions) (*subPlan, []*Diag, error) {
	pattern := o.Pattern
	if pattern == "" {
		return nil, nil, &Diag{Code: DiagBadPattern, Detail: "empty pattern"}
	}
	p := &subPlan{literal: o.Literal}
	if o.Literal {
		dec, err := decodeEscapes(pattern, false)
		if err != nil {
			return nil, nil, &Diag{Code: DiagBadPattern, Detail: err.Error()}
		}
		pattern = regexp.QuoteMeta(dec)
	}
	if o.IgnoreCase {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, nil, &Diag{Code: DiagBadPattern, Detail: err.Error(), Fix: "RE2 has no lookaround or backreferences; -F matches the text literally"}
	}
	p.re = re
	repl, err := decodeEscapes(o.Replacement, !o.Literal)
	if err != nil {
		return nil, nil, &Diag{Code: DiagBadReplacement, Detail: err.Error()}
	}
	p.repl = repl
	var warns []*Diag
	if !o.Literal {
		if err := validateTemplate(re, repl); err != nil {
			return nil, nil, err
		}
		if re.NumSubexp() > 0 && !strings.Contains(repl, "$") {
			warns = append(warns, &Diag{Code: DiagBadReplacement, Detail: "the pattern has groups but the replacement no `$`: was $1 eaten by the shell? single-quote the replacement"})
		}
	}
	for _, f := range []struct {
		src string
		dst **regexp.Regexp
	}{{o.Key, &p.keyRe}, {o.ExcludeKey, &p.excludeRe}} {
		if f.src == "" {
			continue
		}
		kre, err := regexp.Compile(f.src)
		if err != nil {
			return nil, nil, &Diag{Code: DiagBadPattern, Detail: "key filter: " + err.Error()}
		}
		*f.dst = kre
	}
	return p, warns, nil
}

// decodeEscapes turns \x{HHHH} into its rune and \\ into one backslash;
// every other byte is kept. In a template a decoded `$` stays literal.
func decodeEscapes(s string, template bool) (string, error) {
	if !strings.Contains(s, `\`) {
		return s, nil
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		switch s[i+1] {
		case '\\':
			b.WriteByte('\\')
			i++
		case 'x':
			if i+2 >= len(s) || s[i+2] != '{' {
				b.WriteByte(s[i])
				continue
			}
			end := strings.IndexByte(s[i+3:], '}')
			if end < 0 {
				return "", fmt.Errorf(`unterminated \x{ in %q`, s)
			}
			hex := s[i+3 : i+3+end]
			n, err := strconv.ParseUint(hex, 16, 32)
			if err != nil || hex == "" || !utf8.ValidRune(rune(n)) {
				return "", fmt.Errorf(`bad \x{%s} in %q: want 1 to 6 hex digits naming a Unicode code point`, hex, s)
			}
			if template && n == '$' {
				b.WriteString("$$")
			} else {
				b.WriteRune(rune(n))
			}
			i += 3 + end
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String(), nil
}

// validateTemplate refuses a `$name` / `${name}` that names no group of re.
// Go expands an unknown name to "" without a word, and reads `$1a` as the
// group named "1a" - so `$1a` silently deletes the match.
func validateTemplate(re *regexp.Regexp, tmpl string) error {
	names := map[string]bool{}
	for _, n := range re.SubexpNames() {
		if n != "" {
			names[n] = true
		}
	}
	for i := 0; i < len(tmpl); i++ {
		if tmpl[i] != '$' {
			continue
		}
		if i+1 < len(tmpl) && tmpl[i+1] == '$' {
			i++
			continue
		}
		name, braced, width := templateName(tmpl[i+1:])
		if name == "" {
			continue // Go writes a lone `$` literally
		}
		num, numeric := groupNumber(name)
		if numeric && num <= re.NumSubexp() || !numeric && names[name] {
			i += width
			continue
		}
		d := &Diag{Code: DiagBadReplacement, Detail: fmt.Sprintf("the replacement's $%s names no group of the pattern (it has %d numbered group(s)%s); Go would expand it to nothing", strings.Trim(tmpl[i+1:i+1+width], "{}"), re.NumSubexp(), namedList(names))}
		if digits := leadingDigits(name); !braced && digits != "" && digits != name {
			d.Detail = fmt.Sprintf("the replacement's $%s reads as the group named %q, not group %s followed by %q", name, name, digits, name[len(digits):])
			d.Fix = fmt.Sprintf("write ${%s}%s", digits, name[len(digits):])
		}
		return d
	}
	return nil
}

// templateName mirrors regexp.Expand's parser: `{name}` or the longest run
// of letters, digits and underscores; width counts the bytes consumed.
func templateName(s string) (name string, braced bool, width int) {
	if strings.HasPrefix(s, "{") {
		braced = true
		s = s[1:]
	}
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' {
			break
		}
		i += size
	}
	if i == 0 || braced && (i >= len(s) || s[i] != '}') {
		return "", false, 0
	}
	if braced {
		return s[:i], true, i + 2
	}
	return s[:i], false, i
}

func groupNumber(name string) (int, bool) {
	if name == "" || len(name) > 1 && name[0] == '0' {
		return 0, false
	}
	n := 0
	for _, c := range []byte(name) {
		if c < '0' || c > '9' || n >= 1e8 {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

func leadingDigits(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}

func namedList(names map[string]bool) string {
	if len(names) == 0 {
		return ""
	}
	var out []string
	for n := range names {
		out = append(out, n)
	}
	sort.Strings(out)
	return " and named " + strings.Join(out, ", ")
}

var reSingleBrace = regexp.MustCompile(`\{(\w+)\}`)

// placeholderTokens is the multiset (sorted) of a value's placeholders:
// i18next `{{x}}` and the web's single-brace `{x}` (apps/web interpolate).
func placeholderTokens(s string) []string {
	var out []string
	for _, m := range rePlaceholder.FindAllStringSubmatch(s, -1) {
		out = append(out, "{{"+m[1]+"}}")
	}
	for _, m := range reSingleBrace.FindAllStringSubmatch(rePlaceholder.ReplaceAllString(s, " "), -1) {
		out = append(out, "{"+m[1]+"}")
	}
	sort.Strings(out)
	return out
}

func samePlaceholders(a, b string) bool {
	return strings.Join(placeholderTokens(a), "\x00") == strings.Join(placeholderTokens(b), "\x00")
}

// Sub runs `lok sub`: values mode, or key renames with o.Keys.
func (t *Tool) Sub(o SubOptions) (SubResult, error) {
	mode := "values"
	if o.Keys {
		mode = "keys"
	}
	res := newSubResult(o, mode)
	if o.Write && o.Expect < 0 {
		return res, &Diag{Code: DiagExpectRequired, Detail: "sub --write needs --expect <n>, the count the dry run showed; nothing was written", Fix: "rerun without --write: its next line is the exact --write --expect <n> command"}
	}
	plan, warns, err := compileSub(o)
	res.Warnings = warns
	if err != nil {
		return res, err
	}
	cats, err := t.loadCatalogs(o.Catalogs)
	if err != nil {
		return res, err
	}
	if o.Keys {
		ks := keySub{explicit: len(o.Catalogs) > 0, rename: func(base string) (string, bool) { return plan.apply(base), true }, worded: plan.apply}
		return t.subKeys(res, plan, cats, o, ks)
	}
	return t.subValues(res, plan, cats, o)
}

// loadCatalogs loads the named catalogs (all when ids is empty).
func (t *Tool) loadCatalogs(ids []string) ([]*Catalog, error) {
	if len(ids) == 0 {
		ids = t.CatalogIDs()
	}
	var out []*Catalog
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		cfg, ok := t.Config.Catalogs[id]
		if !ok {
			return nil, &Diag{Code: DiagCatalogUnknown, Detail: fmt.Sprintf("no catalog %q (have %s)", id, strings.Join(t.CatalogIDs(), ", ")), Fix: "lok catalogs"}
		}
		c, err := LoadCatalog(t.Root, id, cfg)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// problemSet indexes a check run by kind, key and locale (details carry
// values, which a rewrite changes), errors only: a warning never blocks.
// rekey maps a key to where it lives after the run (a rename moves an old
// problem with its key; it is not a new one); nil keeps keys as they are.
func problemSet(ps []Problem, rekey func(string) string) map[string]bool {
	out := map[string]bool{}
	for _, p := range ps {
		if p.Severity == "" {
			k := p.Key
			if rekey != nil {
				k = rekey(k)
			}
			out[p.Kind+"\x00"+k+"\x00"+p.Locale] = true
		}
	}
	return out
}

// regressions lists the problems c has now that before did not.
func regressions(c *Catalog, before map[string]bool) []string {
	var out []string
	for _, p := range checkCatalog(c) {
		if p.Severity == "" && !before[p.Kind+"\x00"+p.Key+"\x00"+p.Locale] {
			out = append(out, fmt.Sprintf("%s %s %s %s: %s", c.ID, p.Locale, p.Kind, quoteKey(p.Key), p.Detail))
		}
	}
	return out
}

func (t *Tool) subValues(res SubResult, p *subPlan, cats []*Catalog, o SubOptions) (SubResult, error) {
	for _, l := range o.Locales {
		found := false
		for _, c := range cats {
			found = found || contains(c.Config.Locales, l)
		}
		if !found {
			return res, &Diag{Code: DiagLocaleUnknown, Detail: fmt.Sprintf("no catalog in scope ships locale %q", l), Fix: "lok catalogs"}
		}
	}
	var placeholderBreaks, emptied, regressed []string
	keys := map[string]bool{}
	olds := map[string]bool{}
	var changed []*Catalog
	for _, c := range cats {
		before := problemSet(checkCatalog(c), nil)
		sc := SubCatalog{Catalog: c.ID, Locales: map[string]int{}, Written: []string{}, cat: c}
		for _, code := range c.Config.Locales {
			if len(o.Locales) > 0 && !contains(o.Locales, code) {
				continue
			}
			for _, l := range c.Locales[code].Object.LeafPaths() {
				key := FormatKey(c.Config.Style, l.Path)
				if !p.keyInScope(key) {
					continue
				}
				if c.Config.Style == StyleEnglishAsKey && code == "en" && !c.Config.wordable(key) {
					if p.re.MatchString(l.Value) {
						res.Total.Skipped++
						if len(res.Skipped) < o.Limit {
							res.Skipped = append(res.Skipped, SubSkip{Catalog: c.ID, Key: key, Locale: code, Reason: "en-is-key"})
						}
					}
					continue
				}
				after := p.apply(l.Value)
				if after == l.Value {
					continue
				}
				where := fmt.Sprintf("%s %s %s", c.ID, code, shellword.Quote(key))
				if !samePlaceholders(l.Value, after) {
					placeholderBreaks = append(placeholderBreaks, fmt.Sprintf("%s %v -> %v", where, placeholderTokens(l.Value), placeholderTokens(after)))
				}
				if strings.TrimSpace(l.Value) != "" && strings.TrimSpace(after) == "" {
					emptied = append(emptied, where)
				}
				if err := c.putSegs(code, l.Path, after); err != nil {
					return res, err
				}
				occ := len(p.re.FindAllStringIndex(l.Value, -1))
				res.Total.Values++
				res.Total.Occurrences += occ
				keys[c.ID+"\x00"+key] = true
				olds[l.Value] = true
				if sc.Locales[code] == 0 {
					sc.order = append(sc.order, code)
				}
				sc.Locales[code]++
				if p.re.MatchString(after) {
					res.StillMatching++
				}
				if len(res.Changes) < o.Limit {
					res.Changes = append(res.Changes, SubChange{Catalog: c.ID, Key: key, Locale: code, Before: l.Value, After: after, Occurrences: occ})
				}
			}
		}
		if len(sc.order) > 0 {
			res.ByCatalog = append(res.ByCatalog, sc)
			changed = append(changed, c)
			regressed = append(regressed, regressions(c, before)...)
		}
	}
	res.Total.Keys = len(keys)
	res.Truncated = res.Total.Values > len(res.Changes)
	if o.Refs && len(olds) > 0 {
		if err := t.subRefs(&res, changed, olds, o.Limit); err != nil {
			return res, err
		}
	}
	switch {
	case len(placeholderBreaks) > 0:
		return res, &Diag{Code: DiagPlaceholderChanged, Detail: summarize(placeholderBreaks, "value(s) would change their placeholders"), Fix: "keep {{x}} / {x} out of the match (narrow the pattern, or --exclude-key)"}
	case len(emptied) > 0:
		return res, &Diag{Code: DiagValueEmptied, Detail: summarize(emptied, "value(s) would become empty"), Fix: "narrow the pattern, or lok rm the key instead"}
	case len(regressed) > 0:
		return res, &Diag{Code: DiagCheckRegressed, Detail: summarize(regressed, "new check problem(s)"), Fix: "narrow the pattern (--key / --exclude-key)"}
	}
	if !o.Write {
		return res, nil
	}
	if o.Expect >= 0 && res.Total.Values != o.Expect {
		return res, &Diag{Code: DiagSubDrift, Detail: fmt.Sprintf("--expect %d, but %d value(s) would change now; nothing was written", o.Expect, res.Total.Values), Fix: "rerun without --write and review the new preview"}
	}
	return t.commitSub(res, nil)
}

func summarize(items []string, what string) string {
	head := items
	if len(head) > 3 {
		head = head[:3]
	}
	s := fmt.Sprintf("%d %s: %s", len(items), what, strings.Join(head, "; "))
	if len(items) > 3 {
		s += fmt.Sprintf("; %d more", len(items)-3)
	}
	return s + "; nothing was written"
}

// commitSub lands a validated run: every catalog and source file is checked
// fresh first (a stale one refuses before any byte lands), then each
// catalog is saved (temp + rename, all its locales or none), then the
// source files, then each written catalog's afterWrite runs once. Across
// catalogs this is best effort; the receipt names what landed.
func (t *Tool) commitSub(res SubResult, files []fileEdit) (SubResult, error) {
	for _, sc := range res.ByCatalog {
		if err := sc.cat.verifyFreshAll(); err != nil {
			return res, err
		}
	}
	for _, f := range files {
		if err := fresh(f.path, f.orig); err != nil {
			return res, err
		}
	}
	var landed []string
	fail := func(err error) (SubResult, error) {
		msg := err.Error()
		code := runxInfra
		var d *Diag
		if errors.As(err, &d) {
			msg, code = d.Detail, d.Code
		}
		if len(landed) > 0 {
			msg += "; already written: " + strings.Join(landed, ", ")
		}
		return res, &Diag{Code: code, Detail: msg}
	}
	written := map[string][]string{}
	for i := range res.ByCatalog {
		sc := &res.ByCatalog[i]
		w, err := sc.cat.Save()
		if err != nil {
			return fail(err)
		}
		written[sc.Catalog] = w
		sc.Written = rel(t.Root, w)
		if len(w) > 0 {
			landed = append(landed, sc.Catalog)
		}
	}
	if len(files) > 0 {
		if err := writeAtomically(files); err != nil {
			return fail(err)
		}
	}
	res.Write = true
	var firstErr error
	for i := range res.ByCatalog {
		sc := &res.ByCatalog[i]
		aw, err := t.afterWrite(sc.cat, written[sc.Catalog])
		sc.AfterWrite = aw
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return res, firstErr
}

// runxInfra mirrors runx.DiagInfraError without importing the envelope here.
const runxInfra = "INFRA_ERROR"

// subRefs lists test sources holding an old value verbatim (memory #214:
// a copy change breaks the tests that assert the old copy). Roots: each
// catalog's scan roots (the catalog file's app directory when it has none)
// plus e2e/ and appium/; there every file counts as a test.
func (t *Tool) subRefs(res *SubResult, cats []*Catalog, olds map[string]bool, limit int) error {
	roots := []string{}
	exts := append([]string{}, defaultExtensions...)
	add := func(r string) {
		if r != "" && !contains(roots, r) {
			roots = append(roots, r)
		}
	}
	for _, c := range cats {
		if c.Config.Scan != nil {
			for _, r := range c.Config.Scan.Roots {
				add(r)
			}
			for _, e := range c.scanExtensions() {
				if !contains(exts, e) {
					exts = append(exts, e)
				}
			}
			continue
		}
		add(filepath.ToSlash(filepath.Dir(filepath.Dir(c.Config.Files))))
	}
	for _, r := range []string{"e2e", "appium"} {
		if st, err := os.Stat(filepath.Join(t.Root, r)); err == nil && st.IsDir() {
			add(r)
		}
	}
	values := make([]string, 0, len(olds))
	for v := range olds {
		if strings.TrimSpace(v) != "" {
			values = append(values, v)
		}
	}
	sort.Strings(values)
	seen := map[string]bool{}
	return t.walkTree(roots, exts, true, func(f sourceFile) error {
		if seen[f.rel] {
			return nil
		}
		seen[f.rel] = true
		if !f.test && !strings.HasPrefix(f.rel, "e2e/") && !strings.HasPrefix(f.rel, "appium/") {
			return nil
		}
		for _, v := range values {
			if i := bytes.Index(f.data, []byte(v)); i >= 0 {
				res.RefsTotal++
				if len(res.Refs) < limit {
					res.Refs = append(res.Refs, SourceLine{File: f.rel, Line: lineAt(f.data, i), Text: v})
				}
			}
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Human rendering
// ---------------------------------------------------------------------------

// Visible renders the invisible and look-alike runes a review must see as
// \x{...}: NBSP, non-breaking hyphen, narrow NBSP, zero-width space, the
// long dashes, every other space separator and format character.
func Visible(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == ' ':
			b.WriteByte(' ')
		case r == 0x2011 || r == 0x2013 || r == 0x2014 || unicode.Is(unicode.Zs, r) || unicode.Is(unicode.Cf, r) || unicode.IsControl(r):
			fmt.Fprintf(&b, `\x{%04X}`, r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// diffContext is one change as a -/+ pair: the changed span with up to 30
// runes of context on each side.
func diffContext(before, after string) (string, string) {
	a, b := []rune(before), []rune(after)
	p := 0
	for p < len(a) && p < len(b) && a[p] == b[p] {
		p++
	}
	s := 0
	for s < len(a)-p && s < len(b)-p && a[len(a)-1-s] == b[len(b)-1-s] {
		s++
	}
	start := p - 30
	if start < 0 {
		start = 0
	}
	cut := func(r []rune) string {
		end := len(r) - s + 30
		if end > len(r) {
			end = len(r)
		}
		out := Visible(string(r[start:end]))
		if start > 0 {
			out = "..." + out
		}
		if end < len(r) {
			out += "..."
		}
		return out
	}
	return cut(a), cut(b)
}

// Text renders the run for a human: one block per listed change, then the
// summary line. JSON output keeps every string raw.
func (r SubResult) Text() string {
	var b strings.Builder
	state := "dry run"
	if r.Write {
		state = "written"
	}
	if r.Mode == "keys" {
		for _, rn := range r.Renames {
			verb := "rename"
			if rn.Merged {
				verb = "merge into"
			}
			fmt.Fprintf(&b, "%s  %s\n  - %s\n  + %s  (%s)\n", rn.Catalog, verb, Visible(rn.From), Visible(rn.To), rn.variants())
		}
		for _, e := range r.CallSites {
			tag := ""
			if e.Test {
				tag = "  (test)"
			}
			fmt.Fprintf(&b, "call site  %s:%d%s\n", e.File, e.Line, tag)
		}
		for _, e := range r.Mirrors {
			fmt.Fprintf(&b, "mirror     %s:%d  %s\n", e.File, e.Line, Visible(e.From))
		}
		for _, l := range r.MirrorSpecs {
			fmt.Fprintf(&b, "spec       %s:%d asserts the old text\n", l.File, l.Line)
		}
		for _, l := range r.Literals {
			fmt.Fprintf(&b, "leftover   %s:%d  %s\n", l.File, l.Line, Visible(l.Text))
		}
		fmt.Fprintf(&b, "%d renames%s · %d entries moved · %d call sites (%d in tests) · %d mirror literals · %d leftover literals · %s\n",
			r.Total.Renames, r.catalogCounts(), r.Total.Values, r.Total.CallSites, r.Total.TestCallSites, r.Total.Mirrors, r.Total.Literals, state)
		return b.String()
	}
	for _, c := range r.Changes {
		minus, plus := diffContext(c.Before, c.After)
		fmt.Fprintf(&b, "%s  %s  %s\n  - %s\n  + %s\n", c.Catalog, c.Locale, c.Key, minus, plus)
	}
	if r.Truncated {
		fmt.Fprintf(&b, "(%d of %d changes listed; --limit raises the cap)\n", len(r.Changes), r.Total.Values)
	}
	for _, s := range r.Skipped {
		fmt.Fprintf(&b, "skipped  %s  %s  %s  (%s: rename the key with --keys)\n", s.Catalog, s.Locale, s.Key, s.Reason)
	}
	for _, l := range r.Refs {
		fmt.Fprintf(&b, "ref  %s:%d  %s\n", l.File, l.Line, Visible(l.Text))
	}
	fmt.Fprintf(&b, "%d values in %d keys%s · %d occurrences · %d skipped", r.Total.Values, r.Total.Keys, r.catalogCounts(), r.Total.Occurrences, r.Total.Skipped)
	if r.StillMatching > 0 {
		fmt.Fprintf(&b, " · %d still matching", r.StillMatching)
	}
	fmt.Fprintf(&b, " · %s\n", state)
	return b.String()
}

func (r SubResult) catalogCounts() string {
	var parts []string
	for _, sc := range r.ByCatalog {
		s := sc.Catalog
		for _, code := range sc.order {
			s += fmt.Sprintf(" %s %d", code, sc.Locales[code])
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return ""
	}
	return " · " + strings.Join(parts, " · ")
}
