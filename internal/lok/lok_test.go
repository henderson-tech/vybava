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
	if _, err := tool.Rm("", "{{count}} hour"); err != nil {
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

func TestScanMethodCalls(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("locales/en.json", "{\n  \"Save\": \"Save\"\n}\n")
	write("src/a.tsx", "t('Save');\nconst m = foo.t('Not plain');\nT('Not method');\nformat.t(\"Still not plain\");\n")
	write("src/b.go", "func f(l i18n.L) {\n\tl.T(\"Sites\")\n\ti18n.FromContext(ctx).N(\n\t\t\"{{count}} items\", n)\n\tls[i].T(`dynamic`)\n\tx.T(fmt.Sprintf(\"no literal\"))\n\tT(\"bare\")\n}\n")
	write("vybava.config.json", `{"lok":{"catalogs":{
	  "m":{"style":"english-as-key","files":"locales/{locale}.json","locales":["en"],"scan":{"roots":["src"],"call":["t","*.T","*.N"],"extensions":[".tsx",".go"]}}}}}`)
	tool, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	res, err := tool.Scan("m", false, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Sites", "{{count}} items"}
	if res.Calls != 3 || strings.Join(res.Missing, "|") != strings.Join(want, "|") {
		t.Fatalf("plain t + method .T/.N on any receiver, never foo.t( or bare T(: %+v", res)
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
