package configdiscover

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/henderson-tech/vybava/internal/lok"
	"github.com/henderson-tech/vybava/internal/vconfig"
)

func TestDiscoverAndDrift(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	git("init", "-q")
	files := map[string]string{
		"apps/client/locales/en.json":   `{"Save":"Save","One_one":"One_one"}`,
		"apps/client/locales/cs.json":   `{"Save":"Uložit","One_one":"Jeden"}`,
		"apps/web/i18n/strings.cs.json": `{"Save":"Uložit"}`,
		"apps/web/i18n/strings.uk.json": `{"Save":"Зберегти"}`,
		"apps/api/openapi.json":         "{}",
		// A single-locale catalog counts when it lives in a locale home; a
		// lone locale-named JSON elsewhere is fixture data, not a catalog.
		"apps/admin-web/dictionaries/cs.json": `{"nav":{"home":"Domů"}}`,
		"apps/api/test/fixtures/cs.json":      `{"id":"cs"}`,
		// Both sit on a "strong" path, and both used to be proposed: a binary
		// nobody reads as lines, and the hand-written source the no-read
		// denial tells you to read instead.
		"packages/generated/report.pdf":     "%PDF-1.4\n\x00\x00binary payload",
		"scripts/openapi/export-openapi.ts": "export const run = () => {}\n",
		"vybava.config.json":                `{"guards":{"noRead":["apps/api/openapi.json"]},"lok":{"catalogs":{"mobile":{"style":"english-as-key","files":"apps/client/locales/{locale}.json","locales":["en","cs"]},"webStrings":{"style":"path","files":"apps/web/i18n/strings.{locale}.json","locales":["cs","uk"]},"adminWebDictionaries":{"style":"path","files":"apps/admin-web/dictionaries/{locale}.json","locales":["cs"]}}}}`,
	}
	for p, raw := range files {
		p = filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	git("add", ".")
	r, err := Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	if c := r.Lok.Catalogs["mobile"]; c.Style != lok.StyleEnglishAsKey || len(c.Plurals) != 1 {
		t.Fatalf("mobile: %+v", c)
	}
	if c, ok := r.Lok.Catalogs["adminWebDictionaries"]; !ok || c.Files != "apps/admin-web/dictionaries/{locale}.json" || len(c.Locales) != 1 {
		t.Fatalf("single-locale dictionary must be proposed: %+v %v", c, r.Lok.Catalogs)
	}
	for id, c := range r.Lok.Catalogs {
		if strings.Contains(c.Files, "fixtures") {
			t.Fatalf("fixture file proposed as catalog %s: %+v", id, c)
		}
	}
	for _, unwanted := range []string{"packages/generated/report.pdf", "scripts/openapi/export-openapi.ts"} {
		if slices.Contains(r.Guards.NoRead, unwanted) {
			t.Fatalf("%s must not be proposed: %v", unwanted, r.Guards.NoRead)
		}
	}
	cfg, err := vconfig.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	warnings, err := r.Drift(cfg)
	if err != nil || len(warnings) != 0 {
		t.Fatalf("drift: %v %v", warnings, err)
	}
	p := filepath.Join(root, "apps/web/i18n/strings.de.json")
	if err := os.WriteFile(p, []byte(`{"Save":"Speichern"}`), 0600); err != nil {
		t.Fatal(err)
	}
	r, err = Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	warnings, err = r.Drift(cfg)
	if err != nil || len(warnings) != 0 {
		t.Fatal("untracked file counted", warnings, err)
	}
	git("add", "apps/web/i18n/strings.de.json")
	r, err = Discover(root)
	if err != nil {
		t.Fatal(err)
	}
	warnings, err = r.Drift(cfg)
	if err != nil || len(warnings) != 1 || !strings.Contains(warnings[0], "strings.de.json") {
		t.Fatalf("new locale: %v %v", warnings, err)
	}
}

func TestWriteAbsentPreservesSections(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, vconfig.FileJSON)
	if err := os.WriteFile(p, []byte(`{"guards":{"maxDumpLines":70}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := vconfig.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	r := Result{Lok: lok.Config{Catalogs: map[string]lok.CatalogConfig{}}}
	written, err := r.WriteAbsent(cfg)
	if err != nil || len(written) != 1 || written[0] != "lok" {
		t.Fatal(written, err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"guards":{"maxDumpLines":70}`) {
		t.Fatal("existing section rewritten")
	}
	cfg, err = vconfig.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if written, err = r.WriteAbsent(cfg); err != nil || len(written) != 0 {
		t.Fatal(written, err)
	}
}

// .prettierignore / .biomeignore are gitignore-style. Matched as anchored
// globs, the commonest entries of all matched nothing at all.
func TestIgnoreMatchesUsesGitignoreSemantics(t *testing.T) {
	patterns := []string{"dist/", "generated", "*.min.js", "/root-only.txt", "vendor/", "!vendor/keep.ts"}
	for name, want := range map[string]bool{
		"dist/app.js":             true,  // trailing slash: the directory and everything under it
		"apps/web/dist/chunk.js":  true,  // ... at any depth
		"packages/generated/a.ts": true,  // no slash: a component anywhere
		"generated":               true,  // ... including as a file itself
		"a/b/c.min.js":            true,  // basename glob at depth
		"root-only.txt":           true,  // leading slash: anchored
		"nested/root-only.txt":    false, // ... so not at depth
		"vendor/lib.ts":           true,
		"vendor/keep.ts":          false, // a later negation wins
		"apps/web/src/index.ts":   false,
		"distribution/notes.md":   false, // "dist/" must not match a prefix
	} {
		if got := ignoreMatches(patterns, name); got != want {
			t.Fatalf("%s: got %v, want %v", name, got, want)
		}
	}
}

// vconfig.Find stats rather than lstats, so a symlinked config reads fine —
// and a plain rename would replace the link with a regular file.
func TestWriteAbsentWritesThroughASymlink(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(t.TempDir(), "real-config.json")
	if err := os.WriteFile(real, []byte(`{"guards":{"maxDumpLines":70}}`), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, vconfig.FileJSON)
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	cfg, err := vconfig.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	r := Result{Lok: lok.Config{Catalogs: map[string]lok.CatalogConfig{}}}
	if _, err := r.WriteAbsent(cfg); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a regular file")
	}
	raw, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"lok"`) {
		t.Fatalf("the link target was not updated: %s", raw)
	}
}

// "openapi." as a basename substring also matches export-openapi.ts, because
// the extension supplies the dot — so discovery proposed forbidding the very
// source its own denial message tells you to read instead.
func TestIsAPISpecMatchesSpecsNotGenerators(t *testing.T) {
	for path, want := range map[string]bool{
		"apps/api/openapi.json":                  true,
		"docs/openapi.yaml":                      true,
		"packages/contract/swagger.yml":          true,
		"apps/api/src/openapi/export-openapi.ts": false,
		"scripts/openapi/export-openapi.mjs":     false,
		"internal/importers/openapi.go":          false,
	} {
		if got := isAPISpec(path); got != want {
			t.Fatalf("%s: got %v, want %v", path, got, want)
		}
	}
}
