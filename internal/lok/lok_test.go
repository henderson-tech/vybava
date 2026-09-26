package lok

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T) *Tool {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("locales/en.json", "{\n  \"Archive\": \"Archive\",\n  \"Save\": \"Save\",\n  \"{{count}} hour_one\": \"{{count}} hour\",\n  \"{{count}} hour_other\": \"{{count}} hours\"\n}\n")
	write("locales/cs.json", "{\n  \"Archive\": \"Archivovat\",\n  \"Save\": \"Uložit\",\n  \"{{count}} hour_one\": \"{{count}} hodina\",\n  \"{{count}} hour_few\": \"{{count}} hodiny\",\n  \"{{count}} hour_other\": \"{{count}} hodin\"\n}\n")
	write("dict/en.json", "{\n  \"meta\": {\n    \"title\": \"FixIt\"\n  }\n}\n")
	write("dict/cs.json", "{\n  \"meta\": {\n    \"title\": \"FixIt CZ\"\n  }\n}\n")
	write("src/a.tsx", "const x = t('Save');\nconst y = t(\n  'Brand new'\n);\nconst z = t(`dyn ${k}`);\n")
	write("vybava.config.json", `{"lok":{"catalogs":{
	  "mobile":{"style":"english-as-key","files":"locales/{locale}.json","locales":["en","cs"],"required":["en","cs"],"scan":{"roots":["src"]}},
	  "dict":{"style":"path","files":"dict/{locale}.json","locales":["en","cs"]}}}}`)
	tool, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func TestGetGrepMissingCheck(t *testing.T) {
	tool := fixture(t)
	v, err := tool.Get("", "Save")
	if err != nil || v.Values["cs"] != "Uložit" || v.Catalog != "mobile" {
		t.Fatalf("get: %v %+v", err, v)
	}
	if _, err := tool.Get("", "Nope"); err == nil || err.(*Diag).Code != DiagKeyMissing {
		t.Fatalf("missing key must be KEY_MISSING, got %v", err)
	}
	g, err := tool.Grep("", "ulož", nil, 10)
	if err != nil || g.Total != 1 || g.Hits[0].Key != "Save" {
		t.Fatalf("grep: %v %+v", err, g)
	}
	g, _ = tool.Grep("", ".", nil, 2)
	if !g.Truncated || len(g.Hits) != 2 {
		t.Fatalf("grep cap: %+v", g)
	}
	gaps, total, err := tool.Missing("", nil, true, 10)
	if err != nil || total != 0 {
		t.Fatalf("missing: %v %+v", err, gaps)
	}
	problems, err := tool.Check("")
	if err != nil || len(problems) != 0 {
		t.Fatalf("check: %v %+v", err, problems)
	}
	// nested path lookup
	if v, err := tool.Get("dict", "meta.title"); err != nil || v.Values["cs"] != "FixIt CZ" {
		t.Fatalf("path get: %v %+v", err, v)
	}
}

func TestAddSetRmPreserveOrder(t *testing.T) {
	tool := fixture(t)
	if _, err := tool.Add("mobile", "Cancel", map[string]string{}); err == nil || err.(*Diag).Code != DiagLocaleRequired {
		t.Fatalf("add without cs must fail LOCALE_REQUIRED, got %v", err)
	}
	if _, err := tool.Add("mobile", "Cancel", map[string]string{"cs": "Zrušit", "de": "x"}); err == nil || err.(*Diag).Code != DiagLocaleUnknown {
		t.Fatalf("unknown locale, got %v", err)
	}
	res, err := tool.Add("mobile", "Cancel", map[string]string{"cs": "Zrušit"})
	if err != nil || len(res.Written) != 2 {
		t.Fatalf("add: %v %+v", err, res)
	}
	en, _ := os.ReadFile(filepath.Join(tool.Root, "locales/en.json"))
	want := "{\n  \"Archive\": \"Archive\",\n  \"Cancel\": \"Cancel\",\n  \"Save\": \"Save\","
	if !strings.HasPrefix(string(en), want) {
		t.Fatalf("insertion slot wrong:\n%s", en)
	}
	if _, err := tool.Add("mobile", "Cancel", map[string]string{"cs": "x"}); err == nil || err.(*Diag).Code != DiagKeyExists {
		t.Fatalf("duplicate add, got %v", err)
	}
	if _, err := tool.Set("", "Cancel", map[string]string{"cs": "Storno"}); err != nil {
		t.Fatal(err)
	}
	if v, _ := tool.Get("", "Cancel"); v.Values["cs"] != "Storno" || v.Values["en"] != "Cancel" {
		t.Fatalf("set: %+v", v)
	}
	if _, err := tool.Rm("", "{{count}} hour", nil); err != nil {
		t.Fatal(err)
	}
	cs, _ := os.ReadFile(filepath.Join(tool.Root, "locales/cs.json"))
	if strings.Contains(string(cs), "hour_few") {
		t.Fatalf("rm must drop plural variants:\n%s", cs)
	}
	// path-style add creates the nesting
	if _, err := tool.Add("dict", "pages.home.title", map[string]string{"en": "Home", "cs": "Domů"}); err != nil {
		t.Fatal(err)
	}
	if v, err := tool.Get("dict", "pages.home.title"); err != nil || v.Values["cs"] != "Domů" {
		t.Fatalf("nested add: %v %+v", err, v)
	}
	// only non-zero diff files are rewritten
	res, _ = tool.Set("dict", "pages.home.title", map[string]string{"cs": "Domů"})
	if len(res.Written) != 0 {
		t.Fatalf("no-op set must write nothing, got %v", res.Written)
	}
}

func TestScan(t *testing.T) {
	tool := fixture(t)
	res, err := tool.Scan("mobile", false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.Calls != 2 || len(res.Missing) != 1 || res.Missing[0] != "Brand new" {
		t.Fatalf("scan: %+v", res)
	}
	if res.OrphanTotal != 2 || !contains(res.Orphans, "Archive") { // Archive + the hour plural base are unused
		t.Fatalf("orphans: %+v", res)
	}
	res, err = tool.Scan("mobile", true, 10)
	if err != nil || len(res.Added) != 1 {
		t.Fatalf("scan --write: %v %+v", err, res)
	}
	gaps, total, _ := tool.Missing("mobile", nil, true, 10)
	if total != 1 || gaps[0].Key != "Brand new" || gaps[0].Locale != "cs" {
		t.Fatalf("after scan, cs must be reported missing: %+v", gaps)
	}
	problems, _ := tool.Check("mobile")
	if len(problems) != 1 || problems[0].Kind != "missing" {
		t.Fatalf("check must flag the gap: %+v", problems)
	}
}

func TestEnWordingOnPluralVariants(t *testing.T) {
	tool := fixture(t)
	// A base key never carries en wording.
	if _, err := tool.Add("mobile", "Item", map[string]string{"en": "An item", "cs": "Položka"}); err == nil || err.(*Diag).Code != DiagConfigInvalid {
		t.Fatalf("base key with --tr en must be refused, got %v", err)
	}
	// A plural variant may: en is the wording, not the key.
	if _, err := tool.Add("mobile", "{{count}} item_one", map[string]string{"en": "1 item", "cs": "1 položka"}); err != nil {
		t.Fatal(err)
	}
	if v, _ := tool.Get("", "{{count}} item_one"); v.Values["en"] != "1 item" {
		t.Fatalf("en wording must be stored verbatim: %+v", v)
	}
	if _, err := tool.Set("", "{{count}} hour_one", map[string]string{"en": "{{count}} hour", "cs": "{{count}} hodina"}); err != nil {
		t.Fatalf("set en on a plural variant must be allowed: %v", err)
	}
	// Without --tr en the literal key is derived — and flagged, never failed.
	if _, err := tool.Add("mobile", "{{count}} item_other", map[string]string{"cs": "{{count}} položek"}); err != nil {
		t.Fatal(err)
	}
	gaps, total, _ := tool.Missing("mobile", nil, true, 10)
	if total != 1 || gaps[0].Key != "{{count}} item_other" || gaps[0].Locale != "en" || gaps[0].Warning == "" {
		t.Fatalf("missing must warn about the unworded en plural variant: %d %+v", total, gaps)
	}
	problems, _ := tool.Check("mobile")
	if len(problems) != 1 || problems[0].Kind != "en-unworded" || problems[0].Severity != "warning" {
		t.Fatalf("check must warn, not fail, on an unworded en plural: %+v", problems)
	}
	if _, err := tool.Set("", "{{count}} item_other", map[string]string{"en": "{{count}} items"}); err != nil {
		t.Fatal(err)
	}
	if problems, _ := tool.Check("mobile"); len(problems) != 0 {
		t.Fatalf("wording the variant clears the warning: %+v", problems)
	}
}

func TestScanCallDecodesStringAndList(t *testing.T) {
	var one ScanConfig
	if err := json.Unmarshal([]byte(`{"roots":["src"],"call":"tr"}`), &one); err != nil || len(one.Call) != 1 || one.Call[0] != "tr" {
		t.Fatalf("string form must decode as a one-entry list: %v %+v", err, one)
	}
	var many ScanConfig
	if err := json.Unmarshal([]byte(`{"roots":["src"],"call":["t","*.T"]}`), &many); err != nil || len(many.Call) != 2 || many.Call[1] != "*.T" {
		t.Fatalf("list form must decode verbatim: %v %+v", err, many)
	}
	var bad ScanConfig
	if err := json.Unmarshal([]byte(`{"roots":["src"],"call":7}`), &bad); err == nil {
		t.Fatal("a number must be rejected")
	}
	cfg := Config{Catalogs: map[string]CatalogConfig{"m": {Style: StyleEnglishAsKey, Files: "l/{locale}.json", Locales: []string{"en"}, Scan: &ScanConfig{Roots: []string{"src"}, Call: Calls{"a.b"}}}}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), `"a.b"`) {
		t.Fatalf("a dotted receiver name must be rejected (use *.b): %v", err)
	}
}

// scanFixture opens lok over a fresh root holding files plus a one-catalog
// config ("m", en only, scanning src/ for t, *.T and *.N in .ts/.tsx/.go).
func scanFixture(t *testing.T, files map[string]string) *Tool {
	t.Helper()
	root := t.TempDir()
	files["vybava.config.json"] = `{"lok":{"catalogs":{
	  "m":{"style":"english-as-key","files":"locales/{locale}.json","locales":["en"],"scan":{"roots":["src"],"call":["t","*.T","*.N"],"extensions":[".ts",".tsx",".go"]}}}}}`
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tool, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	return tool
}

func TestScanMethodCalls(t *testing.T) {
	tool := scanFixture(t, map[string]string{
		"locales/en.json": "{\n  \"Save\": \"Save\"\n}\n",
		"src/a.tsx":       "t('Save');\nconst m = foo.t('Not plain');\nT('Not method');\nformat.t(\"Still not plain\");\n",
		"src/b.go":        "func f(l i18n.L) {\n\tl.T(\"Sites\")\n\ti18n.FromContext(ctx).N(\n\t\t\"{{count}} items\", n)\n\tls[i].T(`dynamic`)\n\tx.T(fmt.Sprintf(\"no literal\"))\n\tT(\"bare\")\n}\n",
	})
	res, err := tool.Scan("m", false, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Sites", "{{count}} items"}
	if res.Calls != 3 || strings.Join(res.Missing, "|") != strings.Join(want, "|") {
		t.Fatalf("plain t + method .T/.N on any receiver, never foo.t( or bare T(: %+v", res)
	}
}

func TestScanSkipsTestSources(t *testing.T) {
	tool := scanFixture(t, map[string]string{
		"locales/en.json":    "{\n  \"Save\": \"Save\"\n}\n",
		"src/a.ts":           "t('Real key');\n",
		"src/a.test.ts":      "t('Save'); t('From a test');\n",
		"src/b.spec.tsx":     "t('From a spec');\n",
		"src/a.test.int.ts":  "t('From a multi-segment test');\n",
		"src/b.spec.gen.ts":  "t('From a multi-segment spec');\n",
		"src/__tests__/c.ts": "t('From __tests__');\n",
		"src/i18n_test.go":   "l.T(\"Hello {{name}}\")\n",
		"src/testdata/d.go":  "l.T(\"From testdata\")\n",
		// a generated key union quotes every key; declarations are never usage
		"src/translation-keys.d.ts": "export type Key = 'Save' | 'Real key';\n",
		// another checkout of the repo: never read, so never rewritten by a rename
		"src/.worktrees/wt/a.ts": "t('Save'); t('From a worktree');\n",
	})
	res, err := tool.Scan("m", false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if res.FilesScanned != 1 || strings.Join(res.Missing, "|") != "Real key" || res.OrphanTotal != 1 {
		t.Fatalf("test sources and .d.ts files are neither extracted nor counted as usage (Save stays an orphan): %+v", res)
	}
}

func TestScanIgnoresComments(t *testing.T) {
	tool := scanFixture(t, map[string]string{
		"locales/en.json": "{}\n",
		"src/a.tsx": "// t('Line comment')\n/* t('Block\n   comment') */\n/**\n * t('Doc block')\n */\n" +
			"const u = t('Visit https://example.com'); // t('Trailing')\n" +
			"const r = /\\/*/; t('After regex');\n" +
			"<p>See https://voke.cz {t('After JSX URL')}</p>\n" +
			"const v = f()\n/* t('Semicolon-less\n   block') */\n",
		"src/b.go": "// Package x\n//\n//\tl.T(\"Hello {{name}}\", i18n.Vars{\"name\": n})\n//\tl.N(\"{{count}} items selected\", n)\npackage x\n\n" +
			"var p = `C:\\`\n// l.T(\"After raw string\")\nfunc f() { l.T(\"Real // not a comment\") }\n",
	})
	res, err := tool.Scan("m", false, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"After JSX URL", "After regex", "Real // not a comment", "Visit https://example.com"}
	if strings.Join(res.Missing, "|") != strings.Join(want, "|") {
		t.Fatalf("calls inside // and /* */ comments are not keys; // inside a literal or a URL is code: %+v", res.Missing)
	}
}

func TestScanTemplateInterpolations(t *testing.T) {
	tool := scanFixture(t, map[string]string{
		"locales/en.json": "{}\n",
		"src/a.ts": "const s = `${x /* t('Block in interpolation') */} // text ${t('After template text')}`;\n" +
			"const n = `${\n  // t('Line in interpolation')\n  ok ? `${t('Nested')} /* text` : t('Else')\n} */ ${t('After nested')}`;\n",
	})
	res, err := tool.Scan("m", false, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"After nested", "After template text", "Else", "Nested"}
	if strings.Join(res.Missing, "|") != strings.Join(want, "|") {
		t.Fatalf("comments inside ${…} are blanked, template text (nested too) is never a comment: %+v", res.Missing)
	}
}

func TestScanRegexLiteralsNeverSwallowCode(t *testing.T) {
	tool := scanFixture(t, map[string]string{
		"locales/en.json": "{}\n",
		"src/a.tsx": "const glob = /[/*]/; t('After class');\n" +
			"if (ok) /[/*]/.test(s) && t('After paren regex');\n" +
			"const r = s.replace(/a[*/]b/g, '').match(/[//]x/) && t('Same line');\n" +
			"if (/'/.test(s)) t('After quote regex // kept');\n" +
			"function f(s) { return /[//]/.test(s) || t('After return'); }\n" +
			"const d = a / b; // t('Division then comment')\n" +
			"const e = x.length / 2 /* t('Block after division') */;\n" +
			"<p>Files in src/* are listed {t('JSX glob')}</p>\n" +
			"t('Last');\n",
	})
	res, err := tool.Scan("m", false, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"After class", "After paren regex", "After quote regex // kept", "After return", "JSX glob", "Last", "Same line"}
	if strings.Join(res.Missing, "|") != strings.Join(want, "|") {
		t.Fatalf("a regex literal, or a /* after a value left open on its line, never blanks real code: %+v", res.Missing)
	}
}

func TestScanRegexAfterConditionParen(t *testing.T) {
	tool := scanFixture(t, map[string]string{
		"locales/en.json": "{}\n",
		"src/a.ts": "if (ok) /[//]/.test(s) && t('Live');\n" +
			"foo(x) / 2 // t('Commented')\n" +
			"while (a) /[//]x/.test(b) && t('W');\n" +
			"if (f(x)) /[//]/.test(s) && t('Nested paren');\n",
	})
	res, err := tool.Scan("m", false, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Live", "Nested paren", "W"}
	if strings.Join(res.Missing, "|") != strings.Join(want, "|") {
		t.Fatalf("a / after the ) of if/while/for/with opens a regex, after any other ) it divides: %+v", res.Missing)
	}
}

func TestScanGoBlockCommentAfterCode(t *testing.T) {
	tool := scanFixture(t, map[string]string{
		"locales/en.json": "{}\n",
		"src/a.go": "package x\n\nfunc f() { /* example\n\tl.T(\"Not a key\")\n*/ l.T(\"Live\") }\n\n" +
			"var n = 1 /* after a value\n\tl.T(\"Not a key either\")\n*/\n",
	})
	res, err := tool.Scan("m", false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Missing, "|") != "Live" {
		t.Fatalf("a Go /* is always a comment, even after code and across lines: %+v", res.Missing)
	}
}

func TestScanJSXBlockComment(t *testing.T) {
	tool := scanFixture(t, map[string]string{
		"locales/en.json": "{}\n",
		"src/a.tsx":       "return (\n  <div>\n    {/*\n      <Button>{t('Old label')}</Button>\n    */}\n    <Button>{t('Live label')}</Button>\n  </div>\n);\n",
	})
	res, err := tool.Scan("m", false, 10)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Missing, "|") != "Live label" {
		t.Fatalf("a multi-line JSX {/* … */} is a comment: %+v", res.Missing)
	}
}

func TestPlaceholderCheck(t *testing.T) {
	tool := fixture(t)
	if _, err := tool.Add("mobile", "Hi {{name}}", map[string]string{"cs": "Ahoj {{jmeno}}"}); err != nil {
		t.Fatal(err)
	}
	problems, _ := tool.Check("mobile")
	if len(problems) != 1 || problems[0].Kind != "placeholders" {
		t.Fatalf("placeholder mismatch must be flagged: %+v", problems)
	}
}

func TestArraysAndScalarsRoundTrip(t *testing.T) {
	src := "{\n  \"hero\": {\n    \"chips\": [\n      \"No fees\",\n      {\n        \"label\": \"Live\",\n        \"count\": 4,\n        \"on\": true,\n        \"none\": null\n      }\n    ]\n  }\n}\n"
	obj, err := ParseObject([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(obj.Marshal()); got != src {
		t.Fatalf("round trip differs:\n%s", got)
	}
	leaves := obj.Leaves()
	if len(leaves) != 2 || leaves[1].Key != "hero.chips.1.label" {
		t.Fatalf("leaves: %+v", leaves)
	}
	c := &Catalog{Config: CatalogConfig{Style: StylePath, Locales: []string{"en"}}, Locales: map[string]*Locale{"en": {Code: "en", Object: obj, Exists: true}}}
	if v, ok := c.Lookup("en", "hero.chips.1.label"); !ok || v != "Live" {
		t.Fatalf("array lookup: %q %v", v, ok)
	}
	if err := c.Put("en", "hero.chips.2", "Third"); err != nil {
		t.Fatal(err)
	}
	if err := c.Put("en", "hero.chips.9", "gap"); err == nil {
		t.Fatal("index past end must fail")
	}
	if !c.Remove("en", "hero.chips.0") {
		t.Fatal("array element remove")
	}
	if v, _ := c.Lookup("en", "hero.chips.1"); v != "Third" {
		t.Fatalf("after remove: %q", v)
	}
}

func TestExemptKeys(t *testing.T) {
	tool := fixture(t)
	tool.Config.Catalogs["mobile"] = withExempt(tool.Config.Catalogs["mobile"], "_help$")
	c, _ := LoadCatalog(tool.Root, "mobile", tool.Config.Catalogs["mobile"])
	_ = c.Put("en", "Join_help", "if you're an employee.")
	_ = c.Put("cs", "Join_help", "pokud jste zaměstnanec.")
	if _, err := c.Save(); err != nil {
		t.Fatal(err)
	}
	if problems, _ := tool.Check("mobile"); len(problems) != 0 {
		t.Fatalf("exempt key must not be flagged: %+v", problems)
	}
	tool.Config.Catalogs["mobile"] = withExempt(tool.Config.Catalogs["mobile"])
	if problems, _ := tool.Check("mobile"); len(problems) != 1 || problems[0].Kind != "english-as-key" {
		t.Fatalf("without exempt it must be flagged: %+v", problems)
	}
}

func withExempt(c CatalogConfig, e ...string) CatalogConfig { c.Exempt = e; return c }

func TestAddInfersCatalogForNewKey(t *testing.T) {
	tool := fixture(t)
	// parent path `meta` exists only in the path catalog → dict, receipt says so
	res, err := tool.Add("", "meta.subtitle", map[string]string{"en": "Sub", "cs": "Pod"})
	if err != nil || res.Catalog != "dict" || strings.Join(res.Locales, ",") != "en,cs" || res.AfterWrite != nil {
		t.Fatalf("parent-path inference: %v %+v", err, res)
	}
	// a sentence with no parent anywhere → the single english-as-key catalog
	if res, err := tool.Add("", "Delete forever", map[string]string{"cs": "Smazat navždy"}); err != nil || res.Catalog != "mobile" {
		t.Fatalf("english-as-key inference: %v %+v", err, res)
	}
	// a flat unknown word has no parent and is no sentence → ambiguous over every catalog
	_, err = tool.Add("", "orphan", map[string]string{"cs": "x"})
	if d, ok := err.(*Diag); !ok || d.Code != DiagCatalogAmbiguous || d.Fix != "pass --catalog=<dict|mobile>" {
		t.Fatalf("no parent anywhere: %v", err)
	}
	// the same parent in two path catalogs → ambiguous naming only those two
	tool.Config.Catalogs["dict2"] = CatalogConfig{Style: StylePath, Files: "dict2/{locale}.json", Locales: []string{"en"}}
	if err := os.MkdirAll(filepath.Join(tool.Root, "dict2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tool.Root, "dict2/en.json"), []byte(`{"meta":{"x":{"y":"z"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = tool.Add("", "meta.other", map[string]string{"en": "O", "cs": "J"})
	if d, ok := err.(*Diag); !ok || d.Code != DiagCatalogAmbiguous || d.Fix != "pass --catalog=<dict|dict2>" {
		t.Fatalf("tie: %v", err)
	}
	// a deeper parent wins the tie
	if res, err := tool.Add("", "meta.x.deeper", map[string]string{"en": "D"}); err != nil || res.Catalog != "dict2" {
		t.Fatalf("longest parent: %v %+v", err, res)
	}
	// an existing key still answers KEY_EXISTS from the catalog that holds it
	if _, err := tool.Add("", "Save", map[string]string{"cs": "x"}); err == nil || err.(*Diag).Code != DiagKeyExists {
		t.Fatalf("existing key: %v", err)
	}
}

// A prettier-formatted catalog keeps short arrays on one line. Reflowing them
// on every save rewrote locale files a write never touched (FixIt sk/uk on an
// en+cs add), so the "diff shows exactly the change" promise broke.
func TestSaveKeepsInlineArraysAndUntouchedFiles(t *testing.T) {
	tool := fixture(t)
	src := "{\n  \"hero\": {\n    \"chips\": [\"No fees\", \"Live\"],\n    \"title\": \"Hi\"\n  }\n}\n"
	if err := os.WriteFile(filepath.Join(tool.Root, "dict/en.json"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	obj, err := ParseObject([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(obj.Marshal()); got != src {
		t.Fatalf("inline array must round-trip:\n%s", got)
	}
	res, err := tool.Set("dict", "meta.title", map[string]string{"cs": "Jen CZ"})
	if err != nil || strings.Join(res.Written, ",") != "dict/cs.json" || strings.Join(res.Locales, ",") != "cs" {
		t.Fatalf("a cs-only write must leave en.json alone: %v %+v", err, res)
	}
}

func TestWriteReceiptReportsAfterWrite(t *testing.T) {
	tool := fixture(t)
	c := tool.Config.Catalogs["dict"]
	c.AfterWrite = "test -f dict/en.json"
	tool.Config.Catalogs["dict"] = c
	res, err := tool.Set("dict", "meta.title", map[string]string{"cs": "FixIt CZ2"})
	if err != nil || res.AfterWrite == nil || !res.AfterWrite.OK || res.AfterWrite.Cmd != c.AfterWrite || strings.Join(res.Locales, ",") != "cs" {
		t.Fatalf("receipt: %v %+v", err, res)
	}
	c.AfterWrite = "false"
	tool.Config.Catalogs["dict"] = c
	res, err = tool.Set("dict", "meta.title", map[string]string{"cs": "FixIt CZ3"})
	if d, ok := err.(*Diag); !ok || d.Code != DiagAfterWriteFailed || res.AfterWrite == nil || res.AfterWrite.OK {
		t.Fatalf("failed afterWrite must still return the receipt: %v %+v", err, res)
	}
}

// A JSON key holding a dot (an API failure code such as `bankid.x`) is one
// segment; before the escaped grammar grep printed it as two, and get, set,
// rm and check all walked a path that does not exist (vt-2931).
func TestDottedSegmentKeys(t *testing.T) {
	tool := fixture(t)
	for _, l := range []string{"en", "cs"} {
		body := "{\n  \"codes\": {\n    \"bankid.x\": {\n      \"title\": \"T-" + l + "\"\n    }\n  },\n  \"meta\": {\n    \"title\": \"FixIt\"\n  }\n}\n"
		if err := os.WriteFile(filepath.Join(tool.Root, "dict", l+".json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	g, err := tool.Grep("dict", "T-cs", nil, 10)
	if err != nil || g.Total != 1 || g.Hits[0].Key != `codes.bankid\.x.title` {
		t.Fatalf("grep prints the escaped key: %v %+v", err, g)
	}
	if v, err := tool.Get("dict", g.Hits[0].Key); err != nil || v.Values["cs"] != "T-cs" || v.Key != `codes.bankid\.x.title` {
		t.Fatalf("the printed key pastes into get: %v %+v", err, v)
	}
	if problems, err := tool.Check("dict"); err != nil || len(problems) != 0 {
		t.Fatalf("no false missing for a dotted segment: %v %+v", err, problems)
	}
	_, err = tool.Get("dict", "codes.bankid.x.title") // the shell ate the backslash
	if d, ok := err.(*Diag); !ok || d.Code != DiagKeyMissing || d.Fix != `lok get 'codes.bankid\.x.title' --catalog=dict` {
		t.Fatalf("did-you-mean names the exact escaped command, never resolves: %v", err)
	}
	if _, err := tool.Get("dict", `codes.bankid\x.title`); err == nil || err.(*Diag).Code != DiagConfigInvalid {
		t.Fatalf("an unknown escape is refused: %v", err)
	}
	_, err = tool.Add("dict", "codes.bankid.y.title", map[string]string{"en": "Y", "cs": "Y"})
	if d, ok := err.(*Diag); !ok || d.Code != DiagConfigInvalid || !strings.Contains(d.Detail, `'codes.bankid\.y.title'`) {
		t.Fatalf("a new `bankid` object beside dotted siblings is refused, naming the escaped key: %v", err)
	}
	if _, err := tool.Add("dict", `codes.bankid\.y.title`, map[string]string{"en": "Y", "cs": "Y"}); err != nil {
		t.Fatal(err)
	}
	if cs, _ := os.ReadFile(filepath.Join(tool.Root, "dict/cs.json")); !strings.Contains(string(cs), `"bankid.y": {`) {
		t.Fatalf("the escaped add writes one dotted segment:\n%s", cs)
	}
	if _, err := tool.Set("dict", `codes.bankid\.x.title`, map[string]string{"cs": "T2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Rm("dict", `codes.bankid\.x`, nil); code(err) != DiagKeyMissing {
		t.Fatalf("a container is no key: rm never drops a subtree: %v", err)
	}
	if res, err := tool.Rm("dict", `codes.bankid\.x.title`, nil); err != nil || strings.Join(res.Locales, ",") != "en,cs" {
		t.Fatalf("rm: %v %+v", err, res)
	}
	// english-as-key keys are verbatim: a trailing dot is text, never an escape.
	if _, err := tool.Add("mobile", "Save.", map[string]string{"cs": "Uložit."}); err != nil {
		t.Fatal(err)
	}
	if g, _ := tool.Grep("mobile", `Uložit\.`, nil, 10); g.Total != 1 || g.Hits[0].Key != "Save." {
		t.Fatalf("english-as-key keys are never escaped: %+v", g)
	}
}

// vt-2931 symptom 2: an inert en `_few` variant can go without the cs plurals.
func TestRmLocaleScope(t *testing.T) {
	tool := fixture(t)
	c, _ := LoadCatalog(tool.Root, "mobile", tool.Config.Catalogs["mobile"])
	if err := c.Put("en", "{{count}} hour_few", "{{count}} hour_few"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Save(); err != nil {
		t.Fatal(err)
	}
	res, err := tool.Rm("mobile", "{{count}} hour_few", []string{"en"})
	if err != nil || strings.Join(res.Locales, ",") != "en" || strings.Join(res.Written, ",") != "locales/en.json" {
		t.Fatalf("rm --locale en touches en only: %v %+v", err, res)
	}
	v, _ := tool.Get("mobile", "{{count}} hour")
	if _, ok := v.Values["en_few"]; ok || v.Values["cs_few"] != "{{count}} hodiny" {
		t.Fatalf("en _few gone, cs _few kept: %+v", v)
	}
	if _, err := tool.Rm("mobile", "{{count}} hour_few", []string{"en"}); err == nil || err.(*Diag).Code != DiagKeyMissing {
		t.Fatalf("absent in the scoped locale is KEY_MISSING: %v", err)
	}
	if _, err := tool.Rm("mobile", "Save", []string{"de"}); err == nil || err.(*Diag).Code != DiagLocaleUnknown {
		t.Fatalf("an unknown locale is refused: %v", err)
	}
	// A key held only outside the scope is no misspelling: the fix must not
	// be the unscoped rm that deletes it there; a real one keeps the scope.
	for _, l := range []string{"en", "cs"} {
		body := "{\n  \"codes\": {\n    \"bankid.x\": \"X\"" + map[string]string{"en": "", "cs": ",\n    \"only\": \"cs\""}[l] + "\n  }\n}\n"
		if err := os.WriteFile(filepath.Join(tool.Root, "dict", l+".json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tool.Rm("dict", "codes.only", []string{"en"}); err == nil || err.(*Diag).Fix != "lok grep codes.only" {
		t.Fatalf("no did-you-mean for the key itself: %v", err)
	}
	if _, err := tool.Rm("dict", "codes.bankid.x", []string{"en"}); err == nil || err.(*Diag).Fix != `lok rm 'codes.bankid\.x' --catalog=dict --locale en` {
		t.Fatalf("the did-you-mean keeps --locale: %v", err)
	}
}
