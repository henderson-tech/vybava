package lok

import (
	"bytes"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/henderson-tech/vybava/internal/shellword"
)

// ---------------------------------------------------------------------------
// Key renames for english-as-key catalogs: `lok mv <old> <new>` for one
// family, `lok sub <re> <rep> --keys` for a regex over every base key. The
// base key moves with every plural variant each locale holds (nothing is
// manufactured in a locale that lacked one), lands at the sorted slot like
// `lok add`, and every literal t('old') call site is rewritten in the same
// run - mobile t() is untyped, so a missed site compiles and silently
// renders English in the Czech UI. Path keys are refused (KEYS_UNSUPPORTED):
// code reaches them through generated types, and tsc is the rename tool.
// ---------------------------------------------------------------------------

// Rename is one key family moved in one catalog.
type Rename struct {
	Catalog string `json:"catalog"`
	From    string `json:"from"`
	To      string `json:"to"`
	// Locales lists, per locale, the variants moved: "base" and suffixes.
	Locales map[string][]string `json:"locales"`
	// Merged: To already existed with identical values; From was dropped.
	Merged bool `json:"merged,omitempty"`
	order  []string
}

func (r Rename) variants() string {
	var parts []string
	for _, code := range r.order {
		parts = append(parts, code+" "+strings.Join(r.Locales[code], " "))
	}
	return strings.Join(parts, " · ")
}

// keySub is how keys mode derives renames: rename maps a base key to its
// new text (ok=false: not renamed), worded rewrites an en variant carrying
// real wording (sub --keys: the same regex; mv: left alone). explicit: the
// catalogs were named, so a path catalog among them is refused, not skipped.
type keySub struct {
	explicit bool
	rename   func(base string) (string, bool)
	worded   func(string) string
	mv       string
}

// Mv renames one english-as-key family: `lok mv <old> <new>`. Without
// --catalog it moves the family in every english-as-key catalog holding it.
// Both keys decode \x{HHHH}, so an invisible or long-dash character is typed
// visibly; nothing else in them is special.
func (t *Tool) Mv(catalogID, oldKey, newKey string, o SubOptions) (SubResult, error) {
	o.Pattern, o.Replacement, o.Keys, o.Literal = oldKey, newKey, true, true
	res := newSubResult(o, "keys")
	from, err := decodeEscapes(oldKey, false)
	if err != nil {
		return res, &Diag{Code: DiagBadPattern, Detail: err.Error()}
	}
	to, err := decodeEscapes(newKey, false)
	if err != nil {
		return res, &Diag{Code: DiagBadReplacement, Detail: err.Error()}
	}
	if from == to {
		return res, &Diag{Code: DiagConfigInvalid, Detail: "old and new key are the same"}
	}
	var cats []*Catalog
	if catalogID != "" {
		if cats, err = t.loadCatalogs([]string{catalogID}); err != nil {
			return res, err
		}
	} else {
		all, err := t.loadCatalogs(nil)
		if err != nil {
			return res, err
		}
		for _, c := range all {
			if c.has(from) {
				cats = append(cats, c)
			}
		}
		if len(cats) == 0 {
			return res, keyMissing(quoteKey(from)+" is in no catalog", from, all, "get", "", nil)
		}
	}
	for _, c := range cats {
		if base, plural := c.Config.BaseKey(from); plural && c.Config.Style == StyleEnglishAsKey && c.has(base) {
			return res, &Diag{Code: DiagConfigInvalid, Detail: fmt.Sprintf("%q is a plural variant; mv moves the whole family - pass its base key", from), Fix: "lok mv " + shellword.Quote(base) + " <new base key>"}
		}
	}
	ks := keySub{explicit: true, mv: from, worded: func(s string) string { return s }, rename: func(base string) (string, bool) {
		return to, base == from
	}}
	return t.subKeys(res, &subPlan{}, cats, o, ks)
}

// FixRerunWithMerge is KEY_EXISTS's Fix when every collision is an
// identical twin; the CLI turns it into the exact command plus --merge.
const FixRerunWithMerge = "rerun the same command with --merge"

// renamePlan is the renames of one catalog and its check baseline.
type renamePlan struct {
	c       *Catalog
	renames []*Rename
	before  []Problem
}

// baseline is the check run from before the renames, re-keyed to where
// each key lives after them.
func (pl *renamePlan) baseline() map[string]bool {
	to := map[string]string{}
	for _, r := range pl.renames {
		to[r.From] = r.To
	}
	return problemSet(pl.before, func(k string) string {
		base, _ := pl.c.Config.BaseKey(k)
		if n, ok := to[base]; ok {
			return n + strings.TrimPrefix(k, base)
		}
		return k
	})
}

func (t *Tool) subKeys(res SubResult, p *subPlan, cats []*Catalog, o SubOptions, ks keySub) (SubResult, error) {
	res, err := t.renameKeys(res, p, cats, o, ks)
	return capRenameListing(res, o.Limit), err
}

// capRenameListing bounds the listed renames, call sites, leftovers and
// mirror literals at --limit, like values mode caps its changes: totals
// stay complete and Truncated says a list was cut.
func capRenameListing(res SubResult, limit int) SubResult {
	res.Total.CallSites, res.Total.Literals, res.Total.Mirrors = len(res.CallSites), len(res.Literals), len(res.Mirrors)
	res.Total.TestCallSites = 0
	for _, e := range res.CallSites {
		if e.Test {
			res.Total.TestCallSites++
		}
	}
	if limit < 0 {
		return res
	}
	var cut [4]bool
	res.Renames, cut[0] = capped(res.Renames, limit)
	res.CallSites, cut[1] = capped(res.CallSites, limit)
	res.Literals, cut[2] = capped(res.Literals, limit)
	res.Mirrors, cut[3] = capped(res.Mirrors, limit)
	res.Truncated = res.Truncated || cut[0] || cut[1] || cut[2] || cut[3]
	return res
}

func capped[T any](list []T, n int) ([]T, bool) {
	if len(list) <= n {
		return list, false
	}
	return list[:n], true
}

// renameKeys plans, validates and (with --write) applies the renames,
// listing every rename, call site and leftover; subKeys caps the lists.
func (t *Tool) renameKeys(res SubResult, p *subPlan, cats []*Catalog, o SubOptions, ks keySub) (SubResult, error) {
	if len(o.Locales) > 0 {
		return res, &Diag{Code: DiagConfigInvalid, Detail: "a key rename moves the key in every locale; --locale does not apply to --keys or mv", Fix: "drop --locale"}
	}
	var plans []*renamePlan
	byID := map[string]*renamePlan{}
	planFor := func(c *Catalog) *renamePlan {
		if pl, ok := byID[c.ID]; ok {
			return pl
		}
		pl := &renamePlan{c: c, before: checkCatalog(c)}
		byID[c.ID] = pl
		plans = append(plans, pl)
		return pl
	}
	for _, c := range cats {
		if c.Config.Style != StyleEnglishAsKey {
			if ks.explicit {
				return res, &Diag{Code: DiagKeysUnsupported, Detail: fmt.Sprintf("catalog %s uses path keys; a key rename is english-as-key only (code reaches path keys through generated types)", c.ID), Fix: "lok set '<new key>' --catalog=" + c.ID + " --tr <locale>=<value>, lok rm '<old key>' --catalog=" + c.ID + ", then typecheck the consuming app"}
			}
			continue
		}
		seen := map[string]bool{}
		for _, k := range c.Keys() {
			base, _ := c.Config.BaseKey(k)
			if seen[base] || !p.keyInScope(base) {
				continue
			}
			seen[base] = true
			if to, ok := ks.rename(base); ok && to != base {
				pl := planFor(c)
				pl.renames = append(pl.renames, &Rename{Catalog: c.ID, From: base, To: to, Locales: map[string][]string{}})
			}
		}
	}
	if o.WithMirrors {
		// Every mirror catalog holding the key moves with it: the source
		// literal changes once, and each catalog's parity rests on it.
		for _, pl := range append([]*renamePlan{}, plans...) {
			if pl.c.Config.Mirrors == nil {
				continue
			}
			for _, r := range pl.renames {
				for _, id := range t.CatalogIDs() {
					cfg := t.Config.Catalogs[id]
					if id == pl.c.ID || cfg.Mirrors == nil || cfg.Style != StyleEnglishAsKey {
						continue
					}
					other, ok := byID[id]
					if !ok {
						c, err := LoadCatalog(t.Root, id, cfg)
						if err != nil {
							return res, err
						}
						if !c.has(r.From) {
							continue
						}
						other = planFor(c)
					}
					if !other.c.has(r.From) || other.hasFrom(r.From) {
						continue
					}
					other.renames = append(other.renames, &Rename{Catalog: id, From: r.From, To: r.To, Locales: map[string][]string{}})
				}
			}
		}
	}
	if len(plans) == 0 && ks.mv != "" {
		return res, &Diag{Code: DiagKeyMissing, Detail: fmt.Sprintf("%q is no base key of an english-as-key catalog in scope", ks.mv), Fix: "lok grep " + shellword.Quote(ks.mv)}
	}
	if err := validateRenames(plans, o, ks); err != nil {
		return res, err
	}

	// Apply in memory: per locale, every old family out first (a chain
	// A->B, B->C must not collide), then every new one in at its sorted slot.
	for _, pl := range plans {
		c := pl.c
		sc := SubCatalog{Catalog: c.ID, Locales: map[string]int{}, Written: []string{}, cat: c}
		for _, code := range c.Config.Locales {
			moved := make([]map[string]string, len(pl.renames))
			for i, r := range pl.renames {
				moved[i] = c.movedFamily(code, r, ks)
				for _, sfx := range c.suffixOrder() {
					if _, ok := moved[i][sfx]; ok {
						c.Remove(code, r.From+sfx)
						label := sfx
						if label == "" {
							label = "base"
						}
						if len(r.Locales[code]) == 0 {
							r.order = append(r.order, code)
						}
						r.Locales[code] = append(r.Locales[code], label)
						res.Total.Values++
						if sc.Locales[code] == 0 {
							sc.order = append(sc.order, code)
						}
						sc.Locales[code]++
					}
				}
			}
			for i, r := range pl.renames {
				if r.Merged {
					continue
				}
				for _, sfx := range c.suffixOrder() {
					if v, ok := moved[i][sfx]; ok {
						if err := c.Put(code, r.To+sfx, v); err != nil {
							return res, err
						}
					}
				}
			}
		}
		for _, r := range pl.renames {
			res.Renames = append(res.Renames, *r)
		}
		res.Total.Renames += len(pl.renames)
		res.ByCatalog = append(res.ByCatalog, sc)
	}
	res.Total.Keys = res.Total.Renames

	files := &sourceEdits{byPath: map[string]*fileEdit{}}
	if err := t.renameCallSites(&res, plans, o, files); err != nil {
		return res, err
	}
	mirrorBlocked, err := t.renameMirrors(&res, plans, o, files)
	if err != nil {
		return res, err
	}
	if len(mirrorBlocked) > 0 {
		return res, &Diag{Code: DiagMirrorSource, Detail: summarize(mirrorBlocked, "source literal(s) mirror the key (the catalog's mirrors.roots)"), Fix: "rerun with --with-mirrors to rewrite them in the same run"}
	}
	var regressed []string
	for _, pl := range plans {
		regressed = append(regressed, regressions(pl.c, pl.baseline())...)
	}
	if len(regressed) > 0 {
		return res, &Diag{Code: DiagCheckRegressed, Detail: summarize(regressed, "new check problem(s)")}
	}
	if len(res.Literals) > 0 {
		lits := make([]string, len(res.Literals))
		for i, l := range res.Literals {
			lits[i] = fmt.Sprintf("%s:%d %q", l.File, l.Line, l.Text)
		}
		d := &Diag{Code: DiagCallSitesUnresolved, Detail: summarize(lits, "quoted old key(s) left in source outside t() calls (lookup tables, t(cond ? 'a' : 'b'), i18nKey props, test assertions)"), Fix: "rewrite them by hand, or pass --allow-literals once they are known to be safe"}
		if o.Write && !o.AllowLiterals {
			return res, d
		}
		if !o.AllowLiterals {
			d.Detail = strings.TrimSuffix(d.Detail, "; nothing was written") + "; --write will refuse until they are resolved"
			res.Warnings = append(res.Warnings, d)
		}
	}
	if !o.Write {
		return res, nil
	}
	if o.Expect >= 0 && res.Total.Renames != o.Expect {
		return res, &Diag{Code: DiagSubDrift, Detail: fmt.Sprintf("--expect %d, but %d key famil(ies) would be renamed now; nothing was written", o.Expect, res.Total.Renames), Fix: "rerun without --write and review the new preview"}
	}
	return t.commitSub(res, files.list())
}

func (pl *renamePlan) hasFrom(from string) bool {
	for _, r := range pl.renames {
		if r.From == from {
			return true
		}
	}
	return false
}

// suffixOrder is the base key followed by the plural suffixes.
func (c *Catalog) suffixOrder() []string {
	return append([]string{""}, c.Config.PluralSuffixes()...)
}

// movedFamily is what a rename writes in one locale: suffix -> new value
// for every variant of r.From the locale holds. Values are kept, except in
// en, where a derived value (== its key) follows the key and a base key
// that is not exempt IS its key; real en wording goes through worded.
func (c *Catalog) movedFamily(locale string, r *Rename, ks keySub) map[string]string {
	out := map[string]string{}
	for _, sfx := range c.suffixOrder() {
		v, ok := c.lookupSegs(locale, []string{r.From + sfx})
		if !ok {
			continue
		}
		if locale == "en" {
			switch {
			case v == r.From+sfx:
				v = r.To + sfx
			case sfx == "" && !c.Config.Exempted(r.From):
				v = r.To
			default:
				v = ks.worded(v)
			}
		}
		out[sfx] = v
	}
	return out
}

// enWording is every en variant of r.From that carries real wording (the
// values movedFamily passes through ks.worded): suffix -> current value.
func (c *Catalog) enWording(r *Rename) map[string]string {
	out := map[string]string{}
	for _, sfx := range c.suffixOrder() {
		v, ok := c.lookupSegs("en", []string{r.From + sfx})
		if !ok || v == r.From+sfx || (sfx == "" && !c.Config.Exempted(r.From)) {
			continue
		}
		out[sfx] = v
	}
	return out
}

// validateRenames refuses before anything moves: a new key that is empty or
// a plural variant, a placeholder set that changes (call sites pass values
// by name), two keys landing on one, and a new key that already exists
// (unless --merge and every locale's family is identical).
func validateRenames(plans []*renamePlan, o SubOptions, ks keySub) error {
	var emptied, suffixed, placeholdersBroken, clashes []string
	twinsOnly := true // every collision is an identical twin: --merge settles them all
	for _, pl := range plans {
		c := pl.c
		froms := map[string]bool{}
		for _, r := range pl.renames {
			froms[r.From] = true
		}
		tos := map[string]string{}
		for _, r := range pl.renames {
			where := fmt.Sprintf("%s %q -> %q", c.ID, r.From, r.To)
			if strings.TrimSpace(r.To) == "" {
				emptied = append(emptied, where)
				continue
			}
			if _, plural := c.Config.BaseKey(r.To); plural {
				suffixed = append(suffixed, where)
			}
			if !sameTokenSet(r.From, r.To) {
				placeholdersBroken = append(placeholdersBroken, fmt.Sprintf("%s %v -> %v", where, placeholderTokens(r.From), placeholderTokens(r.To)))
			}
			// Worded en variants take the regex too: the same value
			// invariants as values mode hold for them.
			for sfx, v := range c.enWording(r) {
				w := ks.worded(v)
				at := fmt.Sprintf("%s en %q", c.ID, r.From+sfx)
				if strings.TrimSpace(w) == "" && strings.TrimSpace(v) != "" {
					emptied = append(emptied, at)
				}
				if !samePlaceholders(v, w) {
					placeholdersBroken = append(placeholdersBroken, fmt.Sprintf("%s %v -> %v", at, placeholderTokens(v), placeholderTokens(w)))
				}
			}
			if prev, dup := tos[r.To]; dup {
				clashes = append(clashes, fmt.Sprintf("%s: %q and %q both become %q", c.ID, prev, r.From, r.To))
				twinsOnly = false
			}
			tos[r.To] = r.From
			if froms[r.To] || !c.has(r.To) {
				continue
			}
			diff := c.familyDiff(r, ks)
			switch {
			case len(diff) == 0 && o.Merge:
				r.Merged = true
			case len(diff) == 0:
				clashes = append(clashes, fmt.Sprintf("%s: %q already exists with identical values (pass --merge to drop the old family and repoint its call sites)", c.ID, r.To))
			default:
				clashes = append(clashes, fmt.Sprintf("%s: %q already exists and differs in %s", c.ID, r.To, strings.Join(diff, ", ")))
				twinsOnly = false
			}
		}
	}
	switch {
	case len(emptied) > 0:
		return &Diag{Code: DiagValueEmptied, Detail: summarize(emptied, "key(s) would become empty")}
	case len(suffixed) > 0:
		return &Diag{Code: DiagBadReplacement, Detail: summarize(suffixed, "new key(s) end in a plural suffix (a family is renamed through its base key)")}
	case len(placeholdersBroken) > 0:
		return &Diag{Code: DiagPlaceholderChanged, Detail: summarize(placeholdersBroken, "rename(s) change the placeholder set, which call sites pass by name")}
	case len(clashes) > 0:
		fix := "lok get '<new key>' --json, align the values with lok set, then rerun with --merge"
		if twinsOnly {
			fix = FixRerunWithMerge
		}
		return &Diag{Code: DiagKeyExists, Detail: summarize(clashes, "rename collision(s)"), Fix: fix}
	}
	return nil
}

func sameTokenSet(a, b string) bool {
	set := func(s string) string {
		seen := map[string]bool{}
		var out []string
		for _, t := range placeholderTokens(s) {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
		return strings.Join(out, "\x00")
	}
	return set(a) == set(b)
}

// familyDiff lists the locales where the family r would move differs from
// the family already under r.To (variants held and values).
func (c *Catalog) familyDiff(r *Rename, ks keySub) []string {
	var diff []string
	for _, code := range c.Config.Locales {
		moved := c.movedFamily(code, r, ks)
		existing := map[string]string{}
		for _, sfx := range c.suffixOrder() {
			if v, ok := c.lookupSegs(code, []string{r.To + sfx}); ok {
				existing[sfx] = v
			}
		}
		same := len(moved) == len(existing)
		for sfx, v := range moved {
			if ev, ok := existing[sfx]; !ok || ev != v {
				same = false
			}
		}
		if !same {
			diff = append(diff, code)
		}
	}
	return diff
}

// sourceEdits accumulates in-memory rewrites of source files, so two
// passes over one file (two catalogs, call sites then mirrors) compose.
type sourceEdits struct {
	byPath map[string]*fileEdit
	order  []string
}

func (s *sourceEdits) data(f sourceFile) []byte {
	if e, ok := s.byPath[f.path]; ok {
		return e.data
	}
	return f.data
}

func (s *sourceEdits) set(f sourceFile, data []byte) {
	if e, ok := s.byPath[f.path]; ok {
		e.data = data
		return
	}
	s.byPath[f.path] = &fileEdit{path: f.path, orig: f.data, data: data}
	s.order = append(s.order, f.path)
}

func (s *sourceEdits) list() []fileEdit {
	var out []fileEdit
	for _, p := range s.order {
		if e := s.byPath[p]; !bytes.Equal(e.orig, e.data) {
			out = append(out, *e)
		}
	}
	return out
}

// splice replaces the spans (sorted by start, disjoint) with their text.
type span struct {
	start, end int
	text       string
}

func splice(data []byte, spans []span) []byte {
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	var b bytes.Buffer
	at := 0
	for _, s := range spans {
		if s.start < at {
			continue // overlapping hit: the earlier one already covers it
		}
		b.Write(data[at:s.start])
		b.WriteString(s.text)
		at = s.end
	}
	b.Write(data[at:])
	return b.Bytes()
}

// quotedForms is every way source spells the text as a whole literal.
func quotedForms(s string) map[byte]string {
	out := map[byte]string{'\'': "'" + escapeLiteral(s, '\'') + "'", '"': `"` + escapeLiteral(s, '"') + `"`}
	if !strings.ContainsAny(s, "`\\\n") && !strings.Contains(s, "${") {
		out['`'] = "`" + s + "`"
	}
	return out
}

func requote(s string, quote byte) string {
	if quote == '`' {
		return strings.NewReplacer(`\`, `\\`, "`", "\\`", "${", `\${`).Replace(s)
	}
	return escapeLiteral(s, quote)
}

// literalHits finds every whole quoted literal equal to one of the texts in
// the comment-blanked source; spans cover the literal body (quotes kept).
func literalHits(blank []byte, texts []string) []span {
	var out []span
	for _, from := range texts {
		for q, needle := range quotedForms(from) {
			for off := 0; ; {
				i := bytes.Index(blank[off:], []byte(needle))
				if i < 0 {
					break
				}
				start := off + i
				out = append(out, span{start: start + 1, end: start + len(needle) - 1, text: string(q) + from})
				off = start + len(needle)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

// renameCallSites rewrites every literal translation call of an old key
// under the catalog's scan roots (tests included, reported as such), then
// lists every quoted old key still left there.
func (t *Tool) renameCallSites(res *SubResult, plans []*renamePlan, o SubOptions, files *sourceEdits) error {
	for _, pl := range plans {
		c := pl.c
		if len(pl.renames) == 0 {
			continue
		}
		if c.Config.Scan == nil || o.NoSource {
			if c.Config.Mirrors != nil && c.Config.Scan == nil {
				continue // its source is the mirror roots
			}
			if !o.NoSource {
				return &Diag{Code: DiagNoScan, Detail: fmt.Sprintf("catalog %s has no scan roots, so lok cannot find the call sites of a renamed key", c.ID), Fix: "add scan: { roots: [...] } to the catalog in vybava.config.ts, or rerun with --no-source when a typecheck names every stale call site"}
			}
			res.Warnings = append(res.Warnings, &Diag{Code: DiagNoScan, Detail: fmt.Sprintf("--no-source: call sites of %s were not rewritten; typecheck the consuming app, a stale key is an error only where t() is typed", c.ID)})
			continue
		}
		// Call sites pass the base key; a direct variant call (`t('X_one')`)
		// is rewritten and swept for too.
		fromTo := map[string]string{}
		var froms []string
		for _, r := range pl.renames {
			for _, sfx := range c.suffixOrder() {
				fromTo[r.From+sfx] = r.To + sfx
				froms = append(froms, r.From+sfx)
			}
		}
		sort.Strings(froms)
		re := c.callRegex()
		err := t.walkSources(c, true, func(f sourceFile) error {
			data := files.data(f)
			goSource := filepath.Ext(f.path) == ".go"
			var spans []span
			rewritten := map[int]bool{}
			for _, s := range callSites(re, data, goSource) {
				to, ok := fromTo[s.key]
				if !ok {
					continue
				}
				spans = append(spans, span{start: s.start, end: s.end, text: requote(to, s.quote)})
				rewritten[s.start] = true
				res.CallSites = append(res.CallSites, SourceEdit{File: f.rel, Line: lineAt(data, s.start), From: s.key, To: to, Test: f.test})
			}
			// Leftovers are swept in the text before the rewrite, minus the
			// call sites it rewrites: sweeping after would flag a new key
			// that is another rename's old key (a chain `ax`->`axx`,
			// `axx`->`axxxx`). requote never adds a newline, so lines hold.
			for _, h := range literalHits(blankComments(data, goSource), froms) {
				if !rewritten[h.start] {
					res.Literals = append(res.Literals, SourceLine{File: f.rel, Line: lineAt(data, h.start), Text: h.text[1:]})
				}
			}
			if len(spans) > 0 {
				files.set(f, splice(data, spans))
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// renameMirrors finds the source literals a mirror catalog's keys copy
// (non-test files under mirrors.roots) and, with --with-mirrors, rewrites
// them; test files holding the old text are listed as specs to update.
// Without --with-mirrors it returns the literals that block the rename.
func (t *Tool) renameMirrors(res *SubResult, plans []*renamePlan, o SubOptions, files *sourceEdits) ([]string, error) {
	var blocked []string
	seen := map[string]bool{}
	for _, pl := range plans {
		m := pl.c.Config.Mirrors
		if m == nil || len(pl.renames) == 0 {
			continue
		}
		if m.BundledInStoreApp {
			res.Warnings = append(res.Warnings, &Diag{Code: DiagMirrorSource, Detail: fmt.Sprintf("%s ships inside a store app binary: store-live apps keep the old key, so once the source text changes they show the generic fallback for it. Retire in three steps (list both texts, switch the source, drop the old key one release later); lok cannot see the store version and never decides this for you", pl.c.ID)})
		}
		fromTo := map[string]string{}
		var froms []string
		for _, r := range pl.renames {
			fromTo[r.From] = r.To
			froms = append(froms, r.From)
		}
		sort.Strings(froms)
		err := t.walkTree(m.Roots, defaultExtensions, true, func(f sourceFile) error {
			data := files.data(f)
			if f.test {
				for _, from := range froms {
					if i := bytes.Index(data, []byte(from)); i >= 0 {
						if k := fmt.Sprintf("spec\x00%s\x00%d", f.rel, lineAt(data, i)); !seen[k] {
							seen[k] = true
							res.MirrorSpecs = append(res.MirrorSpecs, SourceLine{File: f.rel, Line: lineAt(data, i), Text: from})
						}
					}
				}
				return nil
			}
			hits := literalHits(blankComments(data, filepath.Ext(f.path) == ".go"), froms)
			if len(hits) == 0 {
				return nil
			}
			var spans []span
			for _, h := range hits {
				from := h.text[1:]
				line := lineAt(data, h.start)
				if k := fmt.Sprintf("src\x00%s\x00%d\x00%s", f.rel, line, from); !seen[k] {
					seen[k] = true
					res.Mirrors = append(res.Mirrors, SourceEdit{File: f.rel, Line: line, From: from, To: fromTo[from]})
					blocked = append(blocked, fmt.Sprintf("%s:%d", f.rel, line))
				}
				spans = append(spans, span{start: h.start, end: h.end, text: requote(fromTo[from], h.text[0])})
			}
			if o.WithMirrors {
				files.set(f, splice(data, spans))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if o.WithMirrors {
		return nil, nil
	}
	return blocked, nil
}
