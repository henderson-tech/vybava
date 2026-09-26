package lok

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/henderson-tech/vybava/internal/vconfig"
)

// Closed diagnostic codes. Each fires for one reason and names its fix.
const (
	// DiagConfigMissing — no vybava.config.* above cwd, or no `lok` section.
	DiagConfigMissing = "CONFIG_MISSING"
	// DiagConfigInvalid — the lok section fails Validate().
	DiagConfigInvalid = "CONFIG_INVALID"
	// DiagCatalogAmbiguous — a key or file matches several catalogs; pass --catalog.
	DiagCatalogAmbiguous = "CATALOG_AMBIGUOUS"
	// DiagCatalogUnknown — --catalog names no configured catalog.
	DiagCatalogUnknown = "CATALOG_UNKNOWN"
	// DiagKeyMissing — the key exists in no locale of the catalog.
	DiagKeyMissing = "KEY_MISSING"
	// DiagKeyExists — add on a key that already exists; use set.
	DiagKeyExists = "KEY_EXISTS"
	// DiagLocaleRequired — a write omitted a required locale's value.
	DiagLocaleRequired = "LOCALE_REQUIRED"
	// DiagLocaleUnknown — a --tr names a locale the catalog does not ship.
	DiagLocaleUnknown = "LOCALE_UNKNOWN"
	// DiagCheckFailed — `check` found parity or invariant violations.
	DiagCheckFailed = "CHECK_FAILED"
	// DiagAfterWriteFailed — the catalog's afterWrite command exited non-zero.
	DiagAfterWriteFailed = "AFTER_WRITE_FAILED"
	// DiagCatalogChanged - a file changed on disk between load and save; nothing was written.
	DiagCatalogChanged = "CATALOG_CHANGED"

	// sub / mv (values and key renames).

	// DiagBadPattern - the sub pattern (or --key / --exclude-key) is not valid RE2.
	DiagBadPattern = "BAD_PATTERN"
	// DiagBadReplacement - the replacement names a group the pattern lacks ($1a), or a bad \x{...}.
	DiagBadReplacement = "BAD_REPLACEMENT"
	// DiagPlaceholderChanged - a rewrite adds, drops or renames a {{x}} / {x} placeholder.
	DiagPlaceholderChanged = "PLACEHOLDER_CHANGED"
	// DiagValueEmptied - a rewrite leaves a non-empty value empty or whitespace-only.
	DiagValueEmptied = "VALUE_EMPTIED"
	// DiagCheckRegressed - the rewritten catalog fails a `check` rule it passed before.
	DiagCheckRegressed = "CHECK_REGRESSED"
	// DiagSubDrift - --expect names a different count than the run would change.
	DiagSubDrift = "SUB_DRIFT"
	// DiagCallSitesUnresolved - a quoted old key is left in source after the call-site rewrite.
	DiagCallSitesUnresolved = "CALL_SITES_UNRESOLVED"
	// DiagNoScan - a key rename in a catalog with neither scan nor mirrors cannot find its call sites.
	DiagNoScan = "NO_SCAN"
	// DiagMirrorSource - the key is a literal in the catalog's mirror roots; rename it with --with-mirrors.
	DiagMirrorSource = "MIRROR_SOURCE"
	// DiagKeysUnsupported - key renames are english-as-key only (path keys are typed through generated code).
	DiagKeysUnsupported = "KEYS_UNSUPPORTED"
)

// Warnings reuse the code of the refusal they are the soft form of:
// BAD_REPLACEMENT (groups but no `$` - eaten by the shell?), MIRROR_SOURCE
// (the catalog ships in a store app), NO_SCAN (--no-source skipped call
// sites), CALL_SITES_UNRESOLVED (a dry run's leftover literals).

// Diag is one diagnostic; Fix is the exact next command when one exists.
type Diag struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

func (d *Diag) Error() string { return d.Code + ": " + d.Detail }

// Tool is one repo's lok configuration, ready to run verbs.
type Tool struct {
	Root   string
	Config Config
}

// Open loads the lok section for cwd.
func Open(cwd string) (*Tool, error) {
	cfg, err := vconfig.Load(cwd)
	if err != nil {
		if errors.Is(err, vconfig.ErrNotFound) {
			return nil, &Diag{Code: DiagConfigMissing, Detail: err.Error(), Fix: "vybava config init"}
		}
		return nil, &Diag{Code: DiagConfigInvalid, Detail: err.Error()}
	}
	var lc Config
	if err := cfg.Section("lok", &lc); err != nil {
		if errors.Is(err, vconfig.ErrNoSection) {
			return nil, &Diag{Code: DiagConfigMissing, Detail: cfg.Path + " has no lok section", Fix: "add lok: { catalogs: { … } } to " + cfg.Path}
		}
		return nil, &Diag{Code: DiagConfigInvalid, Detail: err.Error()}
	}
	if err := lc.Validate(); err != nil {
		return nil, &Diag{Code: DiagConfigInvalid, Detail: err.Error()}
	}
	return &Tool{Root: cfg.Root, Config: lc}, nil
}

// CatalogIDs in stable order.
func (t *Tool) CatalogIDs() []string {
	ids := make([]string, 0, len(t.Config.Catalogs))
	for id := range t.Config.Catalogs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// CatalogFor resolves --catalog, or the single catalog holding key.
func (t *Tool) CatalogFor(id, key string) (*Catalog, error) {
	if id != "" {
		cfg, ok := t.Config.Catalogs[id]
		if !ok {
			return nil, &Diag{Code: DiagCatalogUnknown, Detail: fmt.Sprintf("no catalog %q (have %s)", id, strings.Join(t.CatalogIDs(), ", ")), Fix: "lok catalogs"}
		}
		return LoadCatalog(t.Root, id, cfg)
	}
	if len(t.Config.Catalogs) == 1 {
		for id, cfg := range t.Config.Catalogs {
			return LoadCatalog(t.Root, id, cfg)
		}
	}
	var hits, all []*Catalog
	for _, cid := range t.CatalogIDs() {
		c, err := LoadCatalog(t.Root, cid, t.Config.Catalogs[cid])
		if err != nil {
			return nil, err
		}
		all = append(all, c)
		if key != "" && c.has(key) {
			hits = append(hits, c)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		if key == "" {
			return nil, ambiguous("several catalogs are configured", t.CatalogIDs())
		}
		d := keyMissing(quoteKey(key)+" is in no catalog", key, all, "get", "")
		if _, err := SplitKey(StylePath, key); err != nil {
			d.Detail += " (" + err.(*Diag).Detail + ")"
		}
		return nil, d
	default:
		return nil, ambiguous(fmt.Sprintf("%q exists in %s", key, strings.Join(catalogIDs(hits), " and ")), catalogIDs(hits))
	}
}

// catalogForNew resolves where a key that does not exist yet should land:
// the single catalog already holding it (so Add can say KEY_EXISTS), else
// the path catalog whose deepest existing parent path is the longest, else
// the single english-as-key catalog when the key reads as a sentence. Ties
// are CATALOG_AMBIGUOUS naming only the tied candidates.
func (t *Tool) catalogForNew(id, key string) (*Catalog, error) {
	if id != "" || len(t.Config.Catalogs) == 1 {
		return t.CatalogFor(id, "")
	}
	var all, exists, parents []*Catalog
	depth := 0
	for _, cid := range t.CatalogIDs() {
		c, err := LoadCatalog(t.Root, cid, t.Config.Catalogs[cid])
		if err != nil {
			return nil, err
		}
		all = append(all, c)
		if c.has(key) {
			exists = append(exists, c)
		}
		if d := c.parentDepth(key); d > 0 && d >= depth {
			if d > depth {
				parents, depth = nil, d
			}
			parents = append(parents, c)
		}
	}
	if len(exists) == 1 {
		return exists[0], nil
	}
	if len(exists) > 1 {
		return nil, ambiguous(fmt.Sprintf("%q exists in %s", key, strings.Join(catalogIDs(exists), " and ")), catalogIDs(exists))
	}
	if len(parents) == 1 {
		return parents[0], nil
	}
	if len(parents) > 1 {
		segs, _ := SplitKey(StylePath, key) // parentDepth > 0 only for a key that parsed
		parent := FormatKey(StylePath, segs[:depth])
		return nil, ambiguous(fmt.Sprintf("parent %q exists in %s", parent, strings.Join(catalogIDs(parents), " and ")), catalogIDs(parents))
	}
	if strings.Contains(key, " ") {
		var english []*Catalog
		for _, c := range all {
			if c.Config.Style == StyleEnglishAsKey {
				english = append(english, c)
			}
		}
		if len(english) == 1 {
			return english[0], nil
		}
		if len(english) > 1 {
			return nil, ambiguous(fmt.Sprintf("%q reads as an english-as-key sentence; %d catalogs use that style", key, len(english)), catalogIDs(english))
		}
	}
	return nil, ambiguous(fmt.Sprintf("no catalog holds %q or any parent path of it", key), t.CatalogIDs())
}

// parentDepth counts how many leading segments of a path-style key already
// resolve to a container in some locale; 0 for flat catalogs and unknown paths.
func (c *Catalog) parentDepth(key string) int {
	if c.Config.Style != StylePath {
		return 0
	}
	segs, err := SplitKey(StylePath, key)
	if err != nil {
		return 0
	}
	best := 0
	for _, loc := range c.Locales {
		var cur any = loc.Object
		n := 0
		for _, seg := range segs[:len(segs)-1] {
			next, ok := child(cur, seg)
			if !ok {
				break
			}
			if _, leaf := next.(string); leaf {
				break
			}
			cur = next
			n++
		}
		if n > best {
			best = n
		}
	}
	return best
}

func ambiguous(detail string, candidates []string) *Diag {
	return &Diag{Code: DiagCatalogAmbiguous, Detail: detail, Fix: "pass --catalog=<" + strings.Join(candidates, "|") + ">"}
}

func catalogIDs(cs []*Catalog) []string {
	ids := make([]string, 0, len(cs))
	for _, c := range cs {
		ids = append(ids, c.ID)
	}
	return ids
}

func (c *Catalog) has(key string) bool {
	for _, code := range c.Config.Locales {
		if _, ok := c.Lookup(code, key); ok {
			return true
		}
		for _, s := range c.Config.PluralSuffixes() {
			if _, ok := c.Lookup(code, key+s); ok {
				return true
			}
		}
	}
	return false
}

// keyMissing is KEY_MISSING for key. When exactly one path leaf across cats
// matches it loosely (a shell-eaten `\.`, an old unescaped spelling), the
// Fix is that leaf's exact escaped command: a diagnostic, never a resolver.
func keyMissing(detail, key string, cats []*Catalog, verb, tail string) *Diag {
	d := &Diag{Code: DiagKeyMissing, Detail: detail, Fix: "lok grep " + shellQuote(key)}
	var hits []string
	hitCatalog := ""
	for _, c := range cats {
		for _, k := range c.resolveLoose(key) {
			hits = append(hits, k)
			hitCatalog = c.ID
		}
	}
	if len(hits) == 1 {
		d.Detail += fmt.Sprintf(`; did you mean %s? A dot inside a path segment is escaped as \. - single-quote the key so the shell keeps the backslash`, shellQuote(hits[0]))
		d.Fix = "lok " + verb + " " + shellQuote(hits[0]) + " --catalog=" + hitCatalog + tail
	}
	return d
}

// validKey refuses a key the catalog's grammar cannot parse (a bad escape).
func (c *Catalog) validKey(key string) error {
	_, err := c.splitPath(key)
	return err
}

// CatalogInfo is one row of `lok catalogs`.
type CatalogInfo struct {
	ID       string         `json:"id"`
	Style    Style          `json:"style"`
	Files    string         `json:"files"`
	Locales  []string       `json:"locales"`
	Required []string       `json:"required"`
	Keys     int            `json:"keys"`
	Missing  map[string]int `json:"missing"`
}

// Catalogs describes every catalog with key counts and gaps per locale.
func (t *Tool) Catalogs() ([]CatalogInfo, error) {
	var out []CatalogInfo
	for _, id := range t.CatalogIDs() {
		c, err := LoadCatalog(t.Root, id, t.Config.Catalogs[id])
		if err != nil {
			return nil, err
		}
		info := CatalogInfo{ID: id, Style: c.Config.Style, Files: c.Config.Files, Locales: c.Config.Locales, Required: c.Config.RequiredLocales(), Missing: map[string]int{}}
		keys := c.Keys()
		info.Keys = len(keys)
		for _, code := range c.Config.Locales {
			for _, k := range keys {
				if _, ok := c.Lookup(code, k); !ok && c.expectedIn(code, k) {
					info.Missing[code]++
				}
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// wordable reports whether an english-as-key catalog lets the key carry en
// wording that differs from the key: plural variants and exempt keys.
func (c CatalogConfig) wordable(key string) bool {
	_, plural := c.BaseKey(key)
	return plural || c.Exempted(key)
}

// unwordedPlural is a plural variant whose en value is still the derived
// literal key ("{{count}} item_one") — legal, but a translator should see it.
func (c *Catalog) unwordedPlural(key string) bool {
	if c.Config.Style != StyleEnglishAsKey || !contains(c.Config.Locales, "en") || c.Config.Exempted(key) {
		return false
	}
	if _, plural := c.Config.BaseKey(key); !plural {
		return false
	}
	v, ok := c.Lookup("en", key)
	return ok && v == key
}

// expectedIn: plural variants are locale-specific (Czech has _few/_many,
// English does not), so a plural key missing from another locale is not a gap.
func (c *Catalog) expectedIn(locale, key string) bool {
	if _, plural := c.Config.BaseKey(key); plural {
		return false
	}
	return true
}

// Values is a key's value per locale ("" and absent=false where missing).
type Values struct {
	Catalog string            `json:"catalog"`
	Key     string            `json:"key"`
	Values  map[string]string `json:"values"`
	Missing []string          `json:"missing"`
}

func (t *Tool) values(c *Catalog, key string) Values {
	v := Values{Catalog: c.ID, Key: key, Values: map[string]string{}}
	for _, code := range c.Config.Locales {
		found := false
		if s, ok := c.Lookup(code, key); ok {
			v.Values[code], found = s, true
		}
		for _, sfx := range c.Config.PluralSuffixes() {
			if s, ok := c.Lookup(code, key+sfx); ok {
				v.Values[code+sfx], found = s, true
			}
		}
		if !found && c.expectedIn(code, key) {
			v.Missing = append(v.Missing, code)
		}
	}
	if v.Missing == nil {
		v.Missing = []string{}
	}
	return v
}

// Get returns one key across locales.
func (t *Tool) Get(catalogID, key string) (Values, error) {
	c, err := t.CatalogFor(catalogID, key)
	if err != nil {
		return Values{}, err
	}
	if err := c.validKey(key); err != nil {
		return Values{}, err
	}
	if !c.has(key) {
		return Values{}, keyMissing(quoteKey(key)+" is not in catalog "+c.ID, key, []*Catalog{c}, "get", "")
	}
	return t.values(c, key), nil
}

// Hit is one grep match.
type Hit struct {
	Catalog string `json:"catalog"`
	Key     string `json:"key"`
	Locale  string `json:"locale"`
	Value   string `json:"value"`
}

// GrepResult caps output so a broad pattern never floods the context.
type GrepResult struct {
	Hits      []Hit `json:"hits"`
	Total     int   `json:"total"`
	Truncated bool  `json:"truncated"`
}

// Grep matches a regex (case-insensitive) against keys and values.
func (t *Tool) Grep(catalogID, pattern string, locales []string, limit int) (GrepResult, error) {
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		return GrepResult{}, &Diag{Code: DiagConfigInvalid, Detail: "bad pattern: " + err.Error()}
	}
	ids := t.CatalogIDs()
	if catalogID != "" {
		if _, ok := t.Config.Catalogs[catalogID]; !ok {
			return GrepResult{}, &Diag{Code: DiagCatalogUnknown, Detail: "no catalog " + catalogID, Fix: "lok catalogs"}
		}
		ids = []string{catalogID}
	}
	res := GrepResult{Hits: []Hit{}}
	for _, id := range ids {
		c, err := LoadCatalog(t.Root, id, t.Config.Catalogs[id])
		if err != nil {
			return res, err
		}
		for _, code := range c.Config.Locales {
			if len(locales) > 0 && !contains(locales, code) {
				continue
			}
			for _, l := range c.Locales[code].Object.LeafPaths() {
				key := FormatKey(c.Config.Style, l.Path)
				// Discovery favours recall: `bankid.user` finds the escaped
				// `bankid\.user` segment through its raw spelling too.
				if re.MatchString(key) || re.MatchString(strings.Join(l.Path, ".")) || re.MatchString(l.Value) {
					res.Total++
					if len(res.Hits) < limit {
						res.Hits = append(res.Hits, Hit{Catalog: id, Key: key, Locale: code, Value: l.Value})
					}
				}
			}
		}
	}
	res.Truncated = res.Total > len(res.Hits)
	return res, nil
}

// WriteResult is the receipt of a write: the catalog it landed in, the
// locales and files touched, and the afterWrite outcome.
type WriteResult struct {
	Catalog    string      `json:"catalog"`
	Key        string      `json:"key"`
	Locales    []string    `json:"locales"`
	Written    []string    `json:"written"`
	AfterWrite *AfterWrite `json:"afterWrite,omitempty"`
}

// AfterWrite reports the catalog's afterWrite command and whether it passed.
type AfterWrite struct {
	Cmd string `json:"cmd"`
	OK  bool   `json:"ok"`
}

// Add inserts a new key with per-locale values. english-as-key catalogs
// derive `en` from the key; every required locale needs a value. Without
// --catalog the destination is inferred from the key (see catalogForNew).
func (t *Tool) Add(catalogID, key string, tr map[string]string) (WriteResult, error) {
	c, err := t.catalogForNew(catalogID, key)
	if err != nil {
		return WriteResult{}, err
	}
	if c.has(key) {
		return WriteResult{}, &Diag{Code: DiagKeyExists, Detail: fmt.Sprintf("%q already exists in %s", key, c.ID), Fix: "lok set " + shellQuote(key) + " --tr <locale>=<value>"}
	}
	return t.write(c, key, tr, true)
}

// Set updates an existing key in the given locales (others untouched).
func (t *Tool) Set(catalogID, key string, tr map[string]string) (WriteResult, error) {
	c, err := t.CatalogFor(catalogID, key)
	if err != nil {
		return WriteResult{}, err
	}
	if err := c.validKey(key); err != nil {
		return WriteResult{}, err
	}
	if !c.has(key) {
		d := keyMissing(quoteKey(key)+" is not in "+c.ID, key, []*Catalog{c}, "set", " --tr <locale>=<value>")
		if d.Fix == "lok grep "+shellQuote(key) {
			d.Fix = "lok add " + shellQuote(key) + " --tr <locale>=<value>"
		}
		return WriteResult{}, d
	}
	return t.write(c, key, tr, false)
}

func (t *Tool) write(c *Catalog, key string, tr map[string]string, requireAll bool) (WriteResult, error) {
	tr = cloneMap(tr)
	if c.Config.Style == StyleEnglishAsKey && contains(c.Config.Locales, "en") {
		// A plural variant (`{{count}} item_one`) or an exempt key may carry
		// real en wording; a base key never does.
		if v, ok := tr["en"]; ok && v != key && !c.Config.wordable(key) {
			return WriteResult{}, &Diag{Code: DiagConfigInvalid, Detail: "english-as-key: the en value IS the key; do not pass --tr en=… (plural variants and exempt keys may be worded)"}
		}
		if _, ok := tr["en"]; !ok {
			tr["en"] = key
		}
	}
	for code := range tr {
		if !contains(c.Config.Locales, code) {
			return WriteResult{}, &Diag{Code: DiagLocaleUnknown, Detail: fmt.Sprintf("catalog %s has no locale %q (have %s)", c.ID, code, strings.Join(c.Config.Locales, ", ")), Fix: "lok catalogs"}
		}
	}
	if requireAll {
		var missing []string
		for _, code := range c.Config.RequiredLocales() {
			if _, ok := tr[code]; !ok {
				missing = append(missing, code)
			}
		}
		if len(missing) > 0 {
			var flags []string
			for _, m := range missing {
				flags = append(flags, "--tr "+m+"=<value>")
			}
			return WriteResult{}, &Diag{Code: DiagLocaleRequired, Detail: fmt.Sprintf("%s requires %s", c.ID, strings.Join(missing, ", ")), Fix: "lok add " + shellQuote(key) + " " + strings.Join(flags, " ")}
		}
	}
	if len(tr) == 0 {
		return WriteResult{}, &Diag{Code: DiagLocaleRequired, Detail: "nothing to write", Fix: "pass --tr <locale>=<value>"}
	}
	var locales []string
	for _, code := range c.Config.Locales {
		val, ok := tr[code]
		if !ok {
			continue
		}
		if err := c.Put(code, key, val); err != nil {
			var d *Diag
			if errors.As(err, &d) {
				return WriteResult{}, d
			}
			return WriteResult{}, &Diag{Code: DiagConfigInvalid, Detail: err.Error()}
		}
		locales = append(locales, code)
	}
	return t.commit(c, key, locales)
}

// Rm deletes a key and its plural variants from every locale, or only from
// the given locales (`--locale en` drops an inert en `_few` variant without
// touching the Czech plurals).
func (t *Tool) Rm(catalogID, key string, only []string) (WriteResult, error) {
	c, err := t.CatalogFor(catalogID, key)
	if err != nil {
		return WriteResult{}, err
	}
	if err := c.validKey(key); err != nil {
		return WriteResult{}, err
	}
	for _, code := range only {
		if !contains(c.Config.Locales, code) {
			return WriteResult{}, &Diag{Code: DiagLocaleUnknown, Detail: fmt.Sprintf("catalog %s has no locale %q (have %s)", c.ID, code, strings.Join(c.Config.Locales, ", ")), Fix: "lok catalogs"}
		}
	}
	var locales []string
	for _, code := range c.Config.Locales {
		if len(only) > 0 && !contains(only, code) {
			continue
		}
		removed := c.Remove(code, key)
		for _, s := range c.Config.PluralSuffixes() {
			if c.Remove(code, key+s) {
				removed = true
			}
		}
		if removed {
			locales = append(locales, code)
		}
	}
	if len(locales) == 0 {
		where := c.ID
		if len(only) > 0 {
			where += " " + strings.Join(only, ",")
		}
		return WriteResult{}, keyMissing(quoteKey(key)+" is not in "+where, key, []*Catalog{c}, "rm", "")
	}
	return t.commit(c, key, locales)
}

// commit saves the catalog and builds the receipt; locales are the ones the
// verb actually changed, not every file Save happened to rewrite.
func (t *Tool) commit(c *Catalog, key string, locales []string) (WriteResult, error) {
	written, err := c.Save()
	if err != nil {
		return WriteResult{}, err
	}
	if locales == nil {
		locales = []string{}
	}
	res := WriteResult{Catalog: c.ID, Key: key, Locales: locales, Written: rel(t.Root, written)}
	aw, err := t.afterWrite(c, written)
	res.AfterWrite = aw
	return res, err
}

// afterWrite runs the catalog's afterWrite command once, from the repo
// root, when a save wrote anything; nil when there is nothing to run.
func (t *Tool) afterWrite(c *Catalog, written []string) (*AfterWrite, error) {
	if c.Config.AfterWrite == "" || len(written) == 0 {
		return nil, nil
	}
	aw := &AfterWrite{Cmd: c.Config.AfterWrite}
	cmd := exec.Command("sh", "-c", c.Config.AfterWrite)
	cmd.Dir = t.Root
	if out, err := cmd.CombinedOutput(); err != nil {
		return aw, &Diag{Code: DiagAfterWriteFailed, Detail: fmt.Sprintf("%s: %s", c.Config.AfterWrite, lastLines(string(out), 5)), Fix: "(cd " + t.Root + " && " + c.Config.AfterWrite + ")"}
	}
	aw.OK = true
	return aw, nil
}

// Gap is one missing translation.
type Gap struct {
	Catalog string `json:"catalog"`
	Key     string `json:"key"`
	Locale  string `json:"locale"`
	Source  string `json:"source,omitempty"`
	Warning string `json:"warning,omitempty"`
}

// Missing lists keys absent from a locale that should carry them.
func (t *Tool) Missing(catalogID string, locales []string, requiredOnly bool, limit int) ([]Gap, int, error) {
	ids := t.CatalogIDs()
	if catalogID != "" {
		ids = []string{catalogID}
	}
	var gaps []Gap
	total := 0
	for _, id := range ids {
		cfg, ok := t.Config.Catalogs[id]
		if !ok {
			return nil, 0, &Diag{Code: DiagCatalogUnknown, Detail: "no catalog " + id, Fix: "lok catalogs"}
		}
		c, err := LoadCatalog(t.Root, id, cfg)
		if err != nil {
			return nil, 0, err
		}
		targets := c.Config.Locales
		if requiredOnly {
			targets = c.Config.RequiredLocales()
		}
		for _, k := range c.Keys() {
			for _, code := range targets {
				if len(locales) > 0 && !contains(locales, code) {
					continue
				}
				if _, ok := c.Lookup(code, k); ok || !c.expectedIn(code, k) {
					continue
				}
				total++
				if len(gaps) < limit {
					gaps = append(gaps, Gap{Catalog: id, Key: k, Locale: code, Source: t.sourceValue(c, k)})
				}
			}
			if c.unwordedPlural(k) && (len(locales) == 0 || contains(locales, "en")) {
				total++
				if len(gaps) < limit {
					gaps = append(gaps, Gap{Catalog: id, Key: k, Locale: "en", Source: k, Warning: "en plural variant not worded — pass --tr en=<wording>"})
				}
			}
		}
	}
	if gaps == nil {
		gaps = []Gap{}
	}
	return gaps, total, nil
}

func (t *Tool) sourceValue(c *Catalog, key string) string {
	if c.Config.Style == StyleEnglishAsKey {
		return key
	}
	for _, code := range c.Config.Locales {
		if v, ok := c.Lookup(code, key); ok {
			return v
		}
	}
	return ""
}

// Problem is one `check` finding.
type Problem struct {
	Catalog string `json:"catalog"`
	Kind    string `json:"kind"`
	Key     string `json:"key,omitempty"`
	Locale  string `json:"locale,omitempty"`
	Detail  string `json:"detail"`
	// Severity is "warning" for advisory findings that never fail `check`; empty means error.
	Severity string `json:"severity,omitempty"`
}

var rePlaceholder = regexp.MustCompile(`\{\{\s*([A-Za-z0-9_.]+)\s*\}\}`)

// Check enforces: every locale file exists; required locales carry every
// key; english-as-key en[key]==key; placeholders agree across locales.
func (t *Tool) Check(catalogID string) ([]Problem, error) {
	ids := t.CatalogIDs()
	if catalogID != "" {
		ids = []string{catalogID}
	}
	problems := []Problem{}
	for _, id := range ids {
		cfg, ok := t.Config.Catalogs[id]
		if !ok {
			return nil, &Diag{Code: DiagCatalogUnknown, Detail: "no catalog " + id, Fix: "lok catalogs"}
		}
		c, err := LoadCatalog(t.Root, id, cfg)
		if err != nil {
			return nil, err
		}
		problems = append(problems, checkCatalog(c)...)
	}
	return problems, nil
}

// checkCatalog runs the `check` rules over one in-memory catalog, so `sub`
// can compare a rewrite against the catalog it started from.
func checkCatalog(c *Catalog) []Problem {
	var problems []Problem
	id := c.ID
	for _, code := range c.Config.Locales {
		if !c.Locales[code].Exists {
			problems = append(problems, Problem{Catalog: id, Kind: "file-missing", Locale: code, Detail: c.Locales[code].Path})
		}
	}
	for _, k := range c.Keys() {
		for _, code := range c.Config.RequiredLocales() {
			if _, ok := c.Lookup(code, k); !ok && c.expectedIn(code, k) {
				problems = append(problems, Problem{Catalog: id, Kind: "missing", Key: k, Locale: code, Detail: "required locale has no value"})
			}
		}
		if _, plural := c.Config.BaseKey(k); c.Config.Style == StyleEnglishAsKey && !plural && !c.Config.Exempted(k) {
			if v, ok := c.Lookup("en", k); ok && v != k {
				problems = append(problems, Problem{Catalog: id, Kind: "english-as-key", Key: k, Locale: "en", Detail: fmt.Sprintf("en value %q must equal the key", v)})
			}
		}
		if c.unwordedPlural(k) {
			problems = append(problems, Problem{Catalog: id, Kind: "en-unworded", Key: k, Locale: "en", Severity: "warning", Detail: "en plural variant carries the literal key; word it with `lok set` --tr en=…"})
		}
		var ref []string
		refLocale := ""
		for _, code := range c.Config.Locales {
			v, ok := c.Lookup(code, k)
			if !ok {
				continue
			}
			ph := placeholders(v)
			if refLocale == "" {
				ref, refLocale = ph, code
				continue
			}
			if strings.Join(ph, ",") != strings.Join(ref, ",") {
				problems = append(problems, Problem{Catalog: id, Kind: "placeholders", Key: k, Locale: code, Detail: fmt.Sprintf("{{…}} set %v differs from %s %v", ph, refLocale, ref)})
			}
		}
	}
	return problems
}

func placeholders(s string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range rePlaceholder.FindAllStringSubmatch(s, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out = append(out, m[1])
		}
	}
	sort.Strings(out)
	return out
}

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func rel(root string, paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if r, err := filepath.Rel(root, p); err == nil {
			out = append(out, r)
		} else {
			out = append(out, p)
		}
	}
	return out
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func shellQuote(s string) string {
	if !strings.ContainsAny(s, " '\"$`\\{}") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ensure os is used on all platforms
var _ = os.ErrNotExist
