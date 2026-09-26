package lok

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repo opens lok over a fresh root holding files (vybava.config.json included).
func repo(t *testing.T, files map[string]string) *Tool {
	t.Helper()
	root := t.TempDir()
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

func read(t *testing.T, tool *Tool, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(tool.Root, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func code(err error) string {
	if d, ok := err.(*Diag); ok {
		return d.Code
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func valuesRepo(t *testing.T) *Tool {
	return repo(t, map[string]string{
		"vybava.config.json": `{"lok":{"catalogs":{
		  "mobile":{"style":"english-as-key","files":"locales/{locale}.json","locales":["en","cs"]},
		  "dict":{"style":"path","files":"dict/{locale}.json","locales":["en","cs"],"afterWrite":"echo ran >> aw.log"}}}}`,
		"locales/en.json": "{\n  \"Hi {{name}}\": \"Hi {{name}}\",\n  \"Loading...\": \"Loading...\"\n}\n",
		"locales/cs.json": "{\n  \"Hi {{name}}\": \"Ahoj {{name}}\",\n  \"Loading...\": \"Načítání...\"\n}\n",
		"dict/en.json":    "{\n  \"hero\": {\n    \"count\": \"{n} items\",\n    \"mail\": \"E-mail us\",\n    \"wait\": \"Wait...\"\n  }\n}\n",
		"dict/cs.json":    "{\n  \"hero\": {\n    \"count\": \"{n} položek\",\n    \"mail\": \"Napište nám e-mail\",\n    \"wait\": \"Počkejte...\"\n  }\n}\n",
	})
}

func TestSubValues(t *testing.T) {
	tool := valuesRepo(t)
	dry, err := tool.Sub(SubOptions{Pattern: `\.\.\.`, Replacement: "…", Expect: -1, Limit: 20})
	if err != nil || dry.Total.Values != 3 || dry.Total.Keys != 2 || dry.Write || len(dry.Changes) != 3 {
		t.Fatalf("dry run: dict en+cs and mobile cs change: %v %+v", err, dry.Total)
	}
	if dry.Total.Skipped != 1 || dry.Skipped[0].Reason != "en-is-key" || dry.Skipped[0].Key != "Loading..." {
		t.Fatalf("the en value of a base key IS the key: skipped, never rewritten: %+v", dry.Skipped)
	}
	if !strings.Contains(read(t, tool, "dict/cs.json"), "Počkejte...") {
		t.Fatal("a dry run writes nothing")
	}
	sensitive, _ := tool.Sub(SubOptions{Pattern: "e-mail", Replacement: "email", Catalogs: []string{"dict"}, Expect: -1, Limit: 20})
	insensitive, _ := tool.Sub(SubOptions{Pattern: "e-mail", Replacement: "email", Catalogs: []string{"dict"}, IgnoreCase: true, Expect: -1, Limit: 20})
	if sensitive.Total.Values != 1 || insensitive.Total.Values != 2 || insensitive.Changes[0].After != "email us" {
		t.Fatalf("case-sensitive by default; -i matches E-mail and writes the replacement literally: %+v %+v", sensitive.Changes, insensitive.Changes)
	}
	if _, err := tool.Sub(SubOptions{Pattern: `\.\.\.`, Replacement: "…", Write: true, Expect: 2, Limit: 20}); code(err) != DiagSubDrift {
		t.Fatalf("--expect must match the reviewed count: %v", err)
	}
	if strings.Contains(read(t, tool, "dict/cs.json"), "…") {
		t.Fatal("SUB_DRIFT writes nothing")
	}
	res, err := tool.Sub(SubOptions{Pattern: `\.\.\.`, Replacement: "…", Write: true, Expect: 3, Limit: 20})
	if err != nil || !res.Write || len(res.ByCatalog) != 2 {
		t.Fatalf("write: %v %+v", err, res.ByCatalog)
	}
	if !strings.Contains(read(t, tool, "dict/cs.json"), `"Počkejte…"`) || !strings.Contains(read(t, tool, "locales/cs.json"), `"Načítání…"`) || !strings.Contains(read(t, tool, "locales/en.json"), `"Loading...": "Loading..."`) {
		t.Fatal("values rewritten, the english-as-key en entry untouched")
	}
	if log := read(t, tool, "aw.log"); log != "ran\n" {
		t.Fatalf("afterWrite runs once per written catalog, got %q", log)
	}
	if matches, _ := filepath.Glob(filepath.Join(tool.Root, "*", "*.lok-tmp")); len(matches) != 0 {
		t.Fatalf("no temp file survives a save: %v", matches)
	}
}

func TestSubRefusesInvariantBreaks(t *testing.T) {
	tool := valuesRepo(t)
	before := read(t, tool, "dict/cs.json")
	for _, c := range []struct {
		name string
		o    SubOptions
		want string
	}{
		{"{{x}} renamed", SubOptions{Pattern: `\{\{name\}\}`, Replacement: "{{jmeno}}"}, DiagPlaceholderChanged},
		{"{x} dropped", SubOptions{Pattern: `\{n\} `, Replacement: "5 ", Catalogs: []string{"dict"}}, DiagPlaceholderChanged},
		{"value emptied", SubOptions{Pattern: `^Wait\.\.\.$`, Replacement: " "}, DiagValueEmptied},
		{"$1a reads as group 1a", SubOptions{Pattern: `(Wait)\.\.\.`, Replacement: "$1a"}, DiagBadReplacement},
		{"unknown named group", SubOptions{Pattern: `(?P<w>Wait)`, Replacement: "${word}"}, DiagBadReplacement},
		{"bad RE2", SubOptions{Pattern: `(?<=a)b`, Replacement: "x"}, DiagBadPattern},
	} {
		c.o.Write, c.o.Expect, c.o.Limit = true, -1, 20
		_, err := tool.Sub(c.o)
		if code(err) != c.want {
			t.Fatalf("%s: want %s, got %v", c.name, c.want, err)
		}
		if c.name == "$1a reads as group 1a" && err.(*Diag).Fix != "write ${1}a" {
			t.Fatalf("$1a names its fix: %+v", err)
		}
	}
	if read(t, tool, "dict/cs.json") != before {
		t.Fatal("a refusal writes nothing")
	}
	res, err := tool.Sub(SubOptions{Pattern: `(Wait)\.\.\.`, Replacement: "Hold on", Expect: -1, Limit: 20})
	if err != nil || len(res.Warnings) != 1 || res.Warnings[0].Code != DiagBadReplacement {
		t.Fatalf("groups but no $ in the replacement: was $1 eaten by the shell? %v %+v", err, res.Warnings)
	}
	nbsp, err := tool.Sub(SubOptions{Pattern: `(\d|\}) `, Replacement: `${1}\x{00A0}`, Catalogs: []string{"dict"}, Expect: -1, Limit: 20})
	if err != nil || nbsp.Total.Values != 2 || nbsp.Changes[0].After != "{n}\u00a0items" || !strings.Contains(nbsp.Text(), `{n}\x{00A0}items`) {
		t.Fatalf("\\x{00A0} decodes in the replacement; the human diff shows it: %v %+v", err, nbsp.Changes)
	}

	// Freshness: a file another writer changed since load is never overwritten.
	c, _ := LoadCatalog(tool.Root, "dict", tool.Config.Catalogs["dict"])
	if err := c.Put("cs", "hero.wait", "Moment"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tool.Root, "dict/cs.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Save(); code(err) != DiagCatalogChanged || read(t, tool, "dict/cs.json") != "{}\n" {
		t.Fatalf("CATALOG_CHANGED, and the other writer's content stays: %v", err)
	}
}

func keysRepo(t *testing.T) *Tool {
	return repo(t, map[string]string{
		"vybava.config.json": `{"lok":{"catalogs":{
		  "mobile":{"style":"english-as-key","files":"locales/{locale}.json","locales":["en","cs"],"scan":{"roots":["src"]}}}}}`,
		"locales/en.json": "{\n  \"Loading...\": \"Loading...\",\n  \"Loading…\": \"Loading…\",\n  \"Save\": \"Save\",\n" +
			"  \"You're done...\": \"You're done...\",\n  \"{{count}} offers..._one\": \"One offer...\",\n  \"{{count}} offers..._other\": \"{{count}} offers..._other\"\n}\n",
		"locales/cs.json": "{\n  \"Loading...\": \"Načítání…\",\n  \"Loading…\": \"Načítání…\",\n  \"Save\": \"Uložit\",\n" +
			"  \"You're done...\": \"Hotovo…\",\n  \"{{count}} offers..._few\": \"{{count}} nabídky\",\n  \"{{count}} offers..._one\": \"{{count}} nabídka\",\n  \"{{count}} offers..._other\": \"{{count}} nabídek\"\n}\n",
		"src/a.tsx":      "t('Loading...');\nt(\"{{count}} offers...\", { count });\nt('You\\'re done...');\n// t('Loading...') in a comment stays\n",
		"src/a.test.tsx": "expect(t('Loading...')).toBe('Načítání…');\n",
		"src/table.ts":   "export const LABELS = { busy: 'Loading...' };\n",
	})
}

func TestSubKeysRenamesFamilyAndCallSites(t *testing.T) {
	tool := keysRepo(t)
	o := SubOptions{Pattern: `\.\.\.`, Replacement: "…", Keys: true, Expect: -1, Limit: 20}
	if _, err := tool.Sub(o); code(err) != DiagKeyExists || err.(*Diag).Fix != FixRerunWithMerge {
		t.Fatalf("an identical twin needs --merge: %v", err)
	}
	o.Merge = true
	dry, err := tool.Sub(o)
	if err != nil || dry.Total.Renames != 3 || len(dry.CallSites) != 4 || len(dry.Literals) != 1 || dry.Literals[0].File != "src/table.ts" {
		t.Fatalf("dry run: 3 families, 4 t() sites (one in a test), the lookup table left over: %v %+v %+v %+v", err, dry.Total, dry.CallSites, dry.Literals)
	}
	if len(dry.Warnings) != 1 || dry.Warnings[0].Code != DiagCallSitesUnresolved {
		t.Fatalf("a dry run warns about the leftover: %+v", dry.Warnings)
	}
	o.Write = true
	if _, err := tool.Sub(o); code(err) != DiagCallSitesUnresolved || !strings.Contains(read(t, tool, "src/a.tsx"), "t('Loading...')") {
		t.Fatalf("--write refuses a leftover literal and writes nothing: %v", err)
	}
	o.AllowLiterals = true
	if _, err := tool.Sub(o); err != nil {
		t.Fatal(err)
	}
	v, err := tool.Get("mobile", "{{count}} offers…")
	if err != nil || v.Values["en_one"] != "One offer…" || v.Values["en_other"] != "{{count}} offers…_other" || v.Values["cs_few"] != "{{count}} nabídky" {
		t.Fatalf("each locale keeps its own variants; en wording takes the regex, a derived en value follows its key: %v %+v", err, v)
	}
	if _, ok := v.Values["en_few"]; ok {
		t.Fatal("nothing is manufactured in a locale that lacked a variant")
	}
	if l, _ := tool.Get("mobile", "Loading…"); l.Values["cs"] != "Načítání…" {
		t.Fatalf("the twin survives the merge: %+v", l)
	}
	if _, err := tool.Get("mobile", "Loading..."); code(err) != DiagKeyMissing {
		t.Fatalf("the merged family is gone: %v", err)
	}
	src := read(t, tool, "src/a.tsx")
	if !strings.Contains(src, "t('Loading…')") || !strings.Contains(src, `t("{{count}} offers…"`) || !strings.Contains(src, `t('You\'re done…')`) || !strings.Contains(src, "// t('Loading...')") {
		t.Fatalf("both quote styles rewritten and re-escaped, comments untouched:\n%s", src)
	}
	if !strings.Contains(read(t, tool, "src/a.test.tsx"), "t('Loading…')") {
		t.Fatal("a test's t() literal is rewritten too")
	}
	en := read(t, tool, "locales/en.json")
	if strings.Index(en, `"Save"`) > strings.Index(en, `"You're done…"`) || strings.Index(en, `"You're done…"`) > strings.Index(en, `"{{count}} offers…_one"`) {
		t.Fatalf("renamed keys land at the sorted slot, like lok add:\n%s", en)
	}
	if scan, _ := tool.Scan("mobile", false, 10); len(scan.Missing) != 0 {
		t.Fatalf("after the rename scan finds no missing key: %+v", scan.Missing)
	}
}

func TestMirrorRename(t *testing.T) {
	tool := repo(t, map[string]string{
		"vybava.config.json": `{"lok":{"catalogs":{
		  "webErrors":{"style":"english-as-key","files":"web/api-errors.{locale}.json","locales":["cs"],"mirrors":{"roots":["api"]}},
		  "appErrors":{"style":"english-as-key","files":"app/api-errors.{locale}.json","locales":["cs"],"mirrors":{"roots":["api"],"bundledInStoreApp":true}},
		  "strings":{"style":"english-as-key","files":"web/strings.{locale}.json","locales":["cs"]},
		  "dict":{"style":"path","files":"dict/{locale}.json","locales":["cs"]}}}}`,
		"web/api-errors.cs.json":        "{\n  \"No ID \u2014 nothing to verify\": \"Chybí IČO\"\n}\n",
		"app/api-errors.cs.json":        "{\n  \"No ID \u2014 nothing to verify\": \"Chybí IČO\"\n}\n",
		"web/strings.cs.json":           "{\n  \"Save\": \"Uložit\"\n}\n",
		"dict/cs.json":                  "{\n  \"meta\": {\n    \"title\": \"FixIt\"\n  }\n}\n",
		"api/companies.service.ts":      "throw new BadRequestException('No ID \u2014 nothing to verify');\n",
		"api/companies.service.spec.ts": "await expect(p).rejects.toThrow('No ID \u2014 nothing to verify');\n",
	})
	old, next := `No ID \x{2014} nothing to verify`, "No ID - nothing to verify"
	_, err := tool.Mv("webErrors", old, next, SubOptions{Expect: -1, Limit: 20})
	if code(err) != DiagMirrorSource || !strings.Contains(err.Error(), "api/companies.service.ts:1") {
		t.Fatalf("a mirrored key refuses without --with-mirrors, naming file:line: %v", err)
	}
	res, err := tool.Mv("webErrors", old, next, SubOptions{WithMirrors: true, Write: true, Expect: -1, Limit: 20})
	if err != nil || res.Total.Renames != 2 || len(res.Mirrors) != 1 || len(res.MirrorSpecs) != 1 || res.MirrorSpecs[0].File != "api/companies.service.spec.ts" {
		t.Fatalf("both mirror catalogs move, the source literal is one edit, the asserting spec is listed: %v %+v", err, res)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Code != DiagMirrorSource || !strings.Contains(res.Warnings[0].Detail, "appErrors ships inside a store app") {
		t.Fatalf("a store-bundled catalog always warns: %+v", res.Warnings)
	}
	if src := read(t, tool, "api/companies.service.ts"); src != "throw new BadRequestException('No ID - nothing to verify');\n" {
		t.Fatalf("source literal: %q", src)
	}
	if !strings.Contains(read(t, tool, "app/api-errors.cs.json"), `"No ID - nothing to verify": "Chybí IČO"`) {
		t.Fatal("the other mirror catalog holding the key moved in the same run")
	}
	if _, err := tool.Mv("strings", "Save", "Store", SubOptions{Expect: -1, Limit: 20}); code(err) != DiagNoScan {
		t.Fatalf("no scan and no mirrors: the call sites cannot be found: %v", err)
	}
	if _, err := tool.Mv("dict", "meta.title", "meta.name", SubOptions{Expect: -1, Limit: 20}); code(err) != DiagKeysUnsupported {
		t.Fatalf("path keys are not renamed: %v", err)
	}
}
