package mergeassist

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMergeLeavesOnlyCode replays the shape of a long-lived branch merging
// main: both sides added a catalog key (and regenerated its types), added a
// migration — the branch's now behind main's — and edited the same code
// line. Git runs the real drivers through a freshly built vybava on PATH.
func TestMergeLeavesOnlyCode(t *testing.T) {
	bin := t.TempDir()
	build := exec.Command("go", "build", "-o", filepath.Join(bin, "vybava"), "../../cmd/vybava")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build vybava: %v\n%s", err, out)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

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
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	migration := func(ts, name string) {
		write("db/migrations/"+ts+"-"+name+".ts", "export class "+name+ts+" {\n  name = '"+name+ts+"';\n}\n")
	}
	commit := func(msg string) {
		// the regen every branch ran when it touched the catalog
		data, _ := os.ReadFile(filepath.Join(root, "i18n/cs.json"))
		write("gen/keys.d.ts", "// generated\n"+string(data))
		git("add", "-A")
		git("commit", "-q", "-m", msg)
	}

	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	write("vybava.config.json", `{
	  "lok": {"catalogs": {"web": {"style": "english-as-key", "files": "i18n/{locale}.json", "locales": ["cs"]}}},
	  "merge": {
	    "generated": [{"paths": ["gen/*.d.ts"], "regen": "printf '// generated\\n' > gen/keys.d.ts && cat i18n/cs.json >> gen/keys.d.ts && rm -f gen/stale.d.ts"}],
	    "migrations": [{"dir": "db/migrations", "style": "typeorm", "step": 100, "check": "true"}]
	  }
	}`)
	write("i18n/cs.json", "{\n  \"Archive\": \"Archivovat\"\n}\n")
	write("src/app.txt", "shared\nvalue\n")
	write("gen/stale.d.ts", "// obsolete output the regen deletes\n")
	migration("100", "Init")
	tool, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	commit("base") // no setup: the merge registers its own drivers

	git("switch", "-q", "-c", "feature")
	write("i18n/cs.json", "{\n  \"Archive\": \"Archivovat\",\n  \"Feature\": \"Funkce\"\n}\n")
	write("src/app.txt", "shared\nfeature\n")
	migration("200", "AddFeature")
	write("db/seed.ts", "import { AddFeature200 } from './migrations/200-AddFeature';\n")
	commit("feature")

	git("switch", "-q", "main")
	write("i18n/cs.json", "{\n  \"Archive\": \"Archivovat\",\n  \"Main\": \"Hlavní\"\n}\n")
	write("src/app.txt", "shared\nmain\n")
	write("db/seed.ts", "// main's seed\n") // conflicts with the branch's, which names its migration
	migration("300", "AddMain")
	commit("main")
	git("switch", "-q", "feature")

	preview, err := tool.Merge(MergeOptions{Onto: "main", DryRun: true, NoFetch: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := classes(preview); got != "catalog:auto generated:auto migration:auto regen:pending code:open code:open" {
		t.Fatalf("preview rows: %s", got)
	}
	if git("status", "--porcelain") != "" {
		t.Fatal("a dry run must touch nothing")
	}
	if _, err := tool.Setup(true); err != nil {
		t.Fatalf("the preview must leave the drivers registered: %v", err)
	}

	rep, err := tool.Merge(MergeOptions{Onto: "main", NoFetch: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := classes(rep); got != "catalog:auto generated:auto migration:auto migration:auto regen:auto code:open code:open" || rep.Open != 2 || rep.Committed != "" {
		t.Fatalf("merge rows: %s (open %d, committed %q)", got, rep.Open, rep.Committed)
	}
	if rep.Rows[len(rep.Rows)-1].Detail != "L2-6" {
		t.Fatalf("code hunk: %+v", rep.Rows[len(rep.Rows)-1])
	}
	catalog, _ := os.ReadFile(filepath.Join(root, "i18n/cs.json"))
	if string(catalog) != "{\n  \"Archive\": \"Archivovat\",\n  \"Feature\": \"Funkce\",\n  \"Main\": \"Hlavní\"\n}\n" {
		t.Fatalf("catalog:\n%s", catalog)
	}
	if keys, _ := os.ReadFile(filepath.Join(root, "gen/keys.d.ts")); string(keys) != "// generated\n"+string(catalog) {
		t.Fatalf("generated file was not regenerated from the merged catalog:\n%s", keys)
	}
	moved, err := os.ReadFile(filepath.Join(root, "db/migrations/401-AddFeature.ts"))
	if err != nil || string(moved) != "export class AddFeature401 {\n  name = 'AddFeature401';\n}\n" {
		t.Fatalf("renumbered migration: %v\n%s", err, moved)
	}
	if seed, _ := os.ReadFile(filepath.Join(root, "db/seed.ts")); !strings.Contains(string(seed), "AddFeature401 } from './migrations/401-AddFeature'") {
		t.Fatalf("reference not rewritten: %s", seed)
	}
	if !strings.Contains(git("status", "--porcelain", "--", "db/seed.ts"), "AA db/seed.ts") {
		t.Fatal("a conflicted reference is rewritten inside its markers but must stay unmerged")
	}
	if !strings.Contains(git("status", "--porcelain", "--", "gen"), "D  gen/stale.d.ts") {
		t.Fatal("a file the regen deleted must be staged as deleted")
	}
	if _, err := os.Stat(filepath.Join(root, "db/migrations/300-AddMain.ts")); err != nil {
		t.Fatal("main's migration must stay as it is")
	}
	for _, line := range strings.Split(strings.TrimSpace(git("status", "--porcelain")), "\n") {
		if line != "UU src/app.txt" && line != "AA db/seed.ts" && line[1] != ' ' {
			t.Fatalf("everything but the code conflict must be staged, got %q", line)
		}
	}
}

func classes(r Report) string {
	var out []string
	for _, row := range r.Rows {
		out = append(out, row.Class+":"+row.State)
	}
	return strings.Join(out, " ")
}

// TestFailedMigrationCheckBlocksCommit: a renumber whose guard fails leaves
// the merge uncommitted even when nothing else conflicts, until a rerun passes.
func TestFailedMigrationCheckBlocksCommit(t *testing.T) {
	root := t.TempDir()
	git := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	add := func(rel, body string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		git("add", "-A")
		git("commit", "-q", "-m", rel)
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	add("vybava.config.json", `{"merge": {"migrations": [{"dir": "m", "style": "typeorm", "check": "echo guard says no; false"}]}}`)
	git("switch", "-q", "-c", "feature")
	add("m/200-AddFeature.ts", "export class AddFeature200 {}\n")
	git("switch", "-q", "main")
	add("m/300-AddMain.ts", "export class AddMain300 {}\n")
	git("switch", "-q", "feature")

	tool, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := tool.Merge(MergeOptions{Onto: "main", NoFetch: true})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Committed != "" || rep.Open != 1 || classes(rep) != "migration:auto migration:failed" {
		t.Fatalf("failed check must block the commit: %s open=%d committed=%q", classes(rep), rep.Open, rep.Committed)
	}

	// Fixed by editing the check command itself: rerunning the migrations
	// reruns it though nothing is left to rename, and the failed row clears.
	if err := os.WriteFile(filepath.Join(root, "vybava.config.json"), []byte(`{"merge": {"migrations": [{"dir": "m", "style": "typeorm", "check": "true"}]}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if tool, err = Open(root); err != nil {
		t.Fatal(err)
	}
	plans, err := tool.PlanMigrations("main", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.ApplyMigrations(plans); err != nil {
		t.Fatal(err)
	}
	if st, err := tool.Status(); err != nil || st.Open != 0 {
		t.Fatalf("a passing rerun must clear the failed check: %v %s", err, classes(st))
	}
}

func TestValidateRejectsDuplicateMigrationDirs(t *testing.T) {
	c := Config{Migrations: []Migrations{{Dir: "m", Style: StyleTypeORM, Check: "a"}, {Dir: "m", Style: StyleTypeORM, Check: "b"}}}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "listed twice") {
		t.Fatalf("two entries for one directory would share its check row: %v", err)
	}
}
