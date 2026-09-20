// Package lok owns locale catalogs for AI-driven sessions: a JSON catalog is
// never read whole — it is queried by key, written by verb, and kept in sync
// across locales by construction. Configured through the `lok` section of
// vybava.config.ts (see internal/vconfig).
package lok

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Style is a catalog's key style.
type Style string

const (
	// StyleEnglishAsKey — the key is the English source text; an `en` file maps key → key.
	StyleEnglishAsKey Style = "english-as-key"
	// StylePath — nested JSON addressed by dotted path.
	StylePath Style = "path"
)

var defaultPlurals = []string{"_zero", "_one", "_two", "_few", "_many", "_other"}

// ScanConfig is the source scan for english-as-key catalogs.
type ScanConfig struct {
	Roots      []string `json:"roots"`
	Call       Calls    `json:"call,omitempty"`
	Extensions []string `json:"extensions,omitempty"`
}

// Calls is the `scan.call` field: the call shapes whose first literal
// argument is a key. It decodes from a plain string (`"t"`, the original
// form) or a list. Each entry is either a bare identifier (`t` — matched
// only when nothing dotted precedes it) or a method form (`*.T` — matched
// as `.T(` on any receiver, the shape Go and class-based code use).
type Calls []string

func (c *Calls) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*c = nil
		if one != "" {
			*c = Calls{one}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return errors.New("scan.call must be a string or an array of strings")
	}
	*c = many
	return nil
}

var callIdent = regexp.MustCompile(`^(\*\.)?[A-Za-z_$][A-Za-z0-9_$]*$`)

// Validate rejects anything that is neither an identifier nor `*.Ident`.
func (c Calls) Validate() error {
	for _, call := range c {
		if !callIdent.MatchString(call) {
			return fmt.Errorf("scan.call entry %q must be an identifier (t) or a method form (*.T)", call)
		}
	}
	return nil
}

// CatalogConfig mirrors the TypeScript CatalogConfig.
type CatalogConfig struct {
	Style      Style       `json:"style"`
	Files      string      `json:"files"`
	Locales    []string    `json:"locales"`
	Required   []string    `json:"required,omitempty"`
	Plurals    []string    `json:"plurals,omitempty"`
	AfterWrite string      `json:"afterWrite,omitempty"`
	Exempt     []string    `json:"exempt,omitempty"`
	Scan       *ScanConfig `json:"scan,omitempty"`
}

// Config is the `lok` section.
type Config struct {
	Catalogs map[string]CatalogConfig `json:"catalogs"`
}

// Validate checks the shape once so every verb can trust it.
func (c *Config) Validate() error {
	if len(c.Catalogs) == 0 {
		return errors.New("lok.catalogs is empty")
	}
	for id, cat := range c.Catalogs {
		switch cat.Style {
		case StyleEnglishAsKey, StylePath:
		default:
			return fmt.Errorf("catalog %q: style must be english-as-key or path, got %q", id, cat.Style)
		}
		if !strings.Contains(cat.Files, "{locale}") {
			return fmt.Errorf("catalog %q: files must contain {locale}", id)
		}
		if len(cat.Locales) == 0 {
			return fmt.Errorf("catalog %q: locales is empty", id)
		}
		for _, r := range cat.Required {
			if !contains(cat.Locales, r) {
				return fmt.Errorf("catalog %q: required locale %q is not in locales", id, r)
			}
		}
		for _, e := range cat.Exempt {
			if _, err := regexp.Compile(e); err != nil {
				return fmt.Errorf("catalog %q: exempt pattern %q: %w", id, e, err)
			}
		}
		if cat.Scan != nil && cat.Style != StyleEnglishAsKey {
			return fmt.Errorf("catalog %q: scan is only supported for english-as-key catalogs", id)
		}
		if cat.Scan != nil {
			if err := cat.Scan.Call.Validate(); err != nil {
				return fmt.Errorf("catalog %q: %w", id, err)
			}
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Required returns the locales every key must carry.
func (c CatalogConfig) RequiredLocales() []string {
	if len(c.Required) > 0 {
		return c.Required
	}
	return c.Locales
}

// PluralSuffixes returns the configured or default plural suffixes.
func (c CatalogConfig) PluralSuffixes() []string {
	if len(c.Plurals) > 0 {
		return c.Plurals
	}
	return defaultPlurals
}

// BaseKey strips a plural suffix: "{{count}} hour_one" → "{{count}} hour".
func (c CatalogConfig) BaseKey(key string) (base string, plural bool) {
	for _, s := range c.PluralSuffixes() {
		if strings.HasSuffix(key, s) && len(key) > len(s) {
			return strings.TrimSuffix(key, s), true
		}
	}
	return key, false
}

// FilePath resolves the catalog file for one locale.
func (c CatalogConfig) FilePath(root, locale string) string {
	return filepath.Join(root, strings.ReplaceAll(c.Files, "{locale}", locale))
}

// ---------------------------------------------------------------------------
// Catalog files
// ---------------------------------------------------------------------------

// Locale is one loaded locale file.
type Locale struct {
	Code   string
	Path   string
	Object *Object
	Exists bool
}

// Catalog is one configured catalog with every locale loaded.
type Catalog struct {
	ID      string
	Root    string
	Config  CatalogConfig
	Locales map[string]*Locale
}

// LoadCatalog reads every locale file (a missing file is an empty locale
// flagged Exists=false so `check` can report it).
func LoadCatalog(root, id string, cfg CatalogConfig) (*Catalog, error) {
	c := &Catalog{ID: id, Root: root, Config: cfg, Locales: map[string]*Locale{}}
	for _, code := range cfg.Locales {
		p := cfg.FilePath(root, code)
		loc := &Locale{Code: code, Path: p, Object: &Object{}}
		data, err := os.ReadFile(p)
		switch {
		case err == nil:
			obj, err := ParseObject(data)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
			loc.Object, loc.Exists = obj, true
		case errors.Is(err, os.ErrNotExist):
		default:
			return nil, err
		}
		c.Locales[code] = loc
	}
	return c, nil
}

// Keys returns the union of leaf keys across locales, in the order of the
// first locale that has them.
func (c *Catalog) Keys() []string {
	seen := map[string]bool{}
	var out []string
	for _, code := range c.Config.Locales {
		for _, e := range c.Locales[code].Object.Leaves() {
			if !seen[e.Key] {
				seen[e.Key] = true
				out = append(out, e.Key)
			}
		}
	}
	return out
}

// Save writes every locale whose object was touched. Files are written whole
// but only the changed keys differ, because order is preserved.
func (c *Catalog) Save() ([]string, error) {
	var written []string
	for _, code := range c.Config.Locales {
		loc := c.Locales[code]
		if !loc.Exists {
			continue
		}
		data := loc.Object.Marshal()
		if old, err := os.ReadFile(loc.Path); err == nil && bytes.Equal(old, data) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(loc.Path), 0o755); err != nil {
			return written, err
		}
		if err := os.WriteFile(loc.Path, data, 0o644); err != nil {
			return written, err
		}
		written = append(written, loc.Path)
	}
	sort.Strings(written)
	return written, nil
}

// Exempted reports whether key is excused from the english-as-key invariant
// (structured keys such as `…_help` whose en value is prose, not the key).
func (c CatalogConfig) Exempted(key string) bool {
	for _, e := range c.Exempt {
		if regexp.MustCompile(e).MatchString(key) {
			return true
		}
	}
	return false
}
