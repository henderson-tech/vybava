package claudeguards

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/henderson-tech/vybava/internal/vconfig"
)

// ---------------------------------------------------------------------------
// context:locale-catalog — a file that vybava.config.ts declares as a `lok`
// catalog is never read raw, not even a 200-line range: the catalog is
// queried through `lok get/grep` and written through `lok add/set/rm`, which
// keep every locale in sync. The catalog files come from the config load the
// guards section already made (Config.lokFiles); a load failure means "no
// catalogs" for THIS call only and is never remembered.
// ---------------------------------------------------------------------------

// lokCatalogFiles resolves the catalog files a loaded config declares.
func lokCatalogFiles(cfg *vconfig.Config) []string {
	raw, ok := cfg.Sections["lok"]
	if !ok {
		return nil
	}
	var lok struct {
		Catalogs map[string]struct {
			Files   string   `json:"files"`
			Locales []string `json:"locales"`
		} `json:"catalogs"`
	}
	if json.Unmarshal(raw, &lok) != nil { // the guard needs only paths; other fields may be anything
		return nil
	}
	var files []string
	for _, c := range lok.Catalogs {
		for _, loc := range c.Locales {
			files = append(files, filepath.Join(cfg.Root, strings.ReplaceAll(c.Files, "{locale}", loc)))
		}
	}
	return files
}

func isLokCatalog(abs string, cfg Config) bool {
	if !strings.HasSuffix(abs, ".json") {
		return false
	}
	for _, f := range cfg.lokFiles {
		if f == abs {
			return true
		}
	}
	return false
}

const catalogMsg = `%s is a locale catalog managed by lok (vybava.config.ts → lok.catalogs).
Reading it raw floods the context with thousands of strings you will not use,
and editing it by hand desynchronises the other locales.

Query:  lok get '<key>' --json   ·   lok grep '<pattern>' --json   ·   lok missing --json
Write:  lok add '<key>' --tr cs='…' --json   ·   lok set … · lok rm …
Bulk:   lok sub '<re>' '<replacement>' --json (dry run; next = the exact write)   ·   lok mv '<old>' '<new>' (key + call sites)
Source: lok scan --json   (missing literal keys + probable orphans)`

func catalogDenial(abs string) *Denial {
	return deny("context:locale-catalog", fmt.Sprintf(catalogMsg, abs), contextReadEscape)
}
