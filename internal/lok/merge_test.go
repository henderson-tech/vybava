package lok

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/henderson-tech/vybava/internal/mergeassist/gitmerge"
)

func TestMergeCatalogByKey(t *testing.T) {
	base := `{
  "Archive": "Archivovat",
  "Delete": "Smazat",
  "Payout": "Výplata",
  "meta": {
    "title": "FixIt"
  }
}
`
	// ours: deletes Delete, adds Banana, adds meta.lead
	ours := `{
  "Archive": "Archivovat",
  "Banana": "Banán",
  "Payout": "Výplata",
  "meta": {
    "title": "FixIt",
    "lead": "Opravy"
  }
}
`
	// theirs: deletes Payout, adds Zebra (out of sorted order), changes meta.title
	theirs := `{
  "Zebra": "Zebra",
  "Archive": "Archivovat",
  "Delete": "Smazat",
  "meta": {
    "title": "FixIt CZ"
  }
}
`
	got, clashes, err := MergeCatalog([]byte(base), []byte(ours), []byte(theirs), PreferNone)
	if err != nil || len(clashes) != 0 {
		t.Fatalf("merge: %v %+v", err, clashes)
	}
	// Banana takes the slot `lok add` would pick on theirs: before the first
	// key sorting after it, even the out-of-order Zebra.
	want := `{
  "Banana": "Banán",
  "Zebra": "Zebra",
  "Archive": "Archivovat",
  "meta": {
    "lead": "Opravy",
    "title": "FixIt CZ"
  }
}
`
	if string(got) != want {
		t.Fatalf("merged:\n%s\nwant:\n%s", got, want)
	}
}

func TestMergeCatalogClash(t *testing.T) {
	base := "{\n  \"Save\": \"Uložit\",\n  \"Open\": \"Otevřít\"\n}\n"
	ours := "{\n  \"Save\": \"Uschovat\",\n  \"Open\": \"Otevřít\"\n}\n"
	theirs := "{\n  \"Save\": \"Ulož\"\n}\n"
	data, clashes, err := MergeCatalog([]byte(base), []byte(ours), []byte(theirs), PreferNone)
	if err != nil || data != nil || len(clashes) != 1 || clashes[0] != (Clash{Key: "Save", Base: `"Uložit"`, Ours: `"Uschovat"`, Theirs: `"Ulož"`}) {
		t.Fatalf("clash: %v %q %+v", err, data, clashes)
	}
	data, _, _ = MergeCatalog([]byte(base), []byte(ours), []byte(theirs), PreferTheirs)
	if string(data) != theirs {
		t.Fatalf("prefer theirs: %s", data)
	}
	data, _, _ = MergeCatalog([]byte(base), []byte(ours), []byte(theirs), PreferOurs)
	if string(data) != "{\n  \"Save\": \"Uschovat\"\n}\n" {
		t.Fatalf("prefer ours keeps theirs' deletion of the untouched key: %s", data)
	}
}

func TestMergeCatalogRefusesNonCanonical(t *testing.T) {
	base := "{\n  \"a\": \"1\"\n}\n"
	if _, _, err := MergeCatalog([]byte(base), []byte(base), []byte("{\n    \"a\": \"2\"\n}\n"), PreferNone); !errors.Is(err, ErrNotCanonical) {
		t.Fatalf("4-space theirs must be refused, got %v", err)
	}
	dup := "{\n  \"a\": \"1\",\n  \"a\": \"2\"\n}\n"
	if _, _, err := MergeCatalog([]byte(base), []byte(dup), []byte(base), PreferNone); !errors.Is(err, ErrDuplicateKey) {
		t.Fatalf("a side holding a key twice must be refused, got %v", err)
	}
}

func TestMergeDriverJournalsAndFallsBack(t *testing.T) {
	tool := fixture(t)
	t.Setenv(gitmerge.JournalEnv, filepath.Join(t.TempDir(), "journal.jsonl"))
	dir := t.TempDir()
	side := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	b := side("b", "{\n  \"Save\": \"Uložit\"\n}\n")
	o := side("o", "{\n  \"Open\": \"Otevřít\",\n  \"Save\": \"Uložit\"\n}\n")
	th := side("t", "{\n  \"Save\": \"Uložit\",\n  \"Undo\": \"Zpět\"\n}\n")
	conflict, err := MergeDriver(tool.Root, b, o, th, "locales/cs.json", io.Discard)
	if err != nil || conflict {
		t.Fatalf("declared catalog: conflict=%v err=%v", conflict, err)
	}
	if got, _ := os.ReadFile(o); string(got) != "{\n  \"Open\": \"Otevřít\",\n  \"Save\": \"Uložit\",\n  \"Undo\": \"Zpět\"\n}\n" {
		t.Fatalf("result:\n%s", got)
	}

	// An undeclared JSON gets git's text merge: both appended after the same line.
	o2 := side("o2", "{\n  \"Save\": \"Uložit\",\n  \"Open\": \"Otevřít\"\n}\n")
	conflict, err = MergeDriver(tool.Root, b, o2, th, "other/cs.json", io.Discard)
	if err != nil || !conflict {
		t.Fatalf("undeclared path must fall back to a conflicting text merge: conflict=%v err=%v", conflict, err)
	}
	if got, _ := os.ReadFile(o2); !strings.Contains(string(got), "<<<<<<< ours") {
		t.Fatalf("text merge left no markers:\n%s", got)
	}

	// Both add "Tools" worded differently, at different lines: git's line merge
	// is clean and keeps both copies, so the clash must force the conflict.
	o3 := side("o3", "{\n  \"Save\": \"Uložit\",\n  \"Tools\": \"Potřebné nářadí\"\n}\n")
	t3 := side("t3", "{\n  \"Tools\": \"Nářadí\",\n  \"Save\": \"Uložit\"\n}\n")
	if conflict, err := MergeDriver(tool.Root, b, o3, t3, "locales/cs.json", io.Discard); err != nil || !conflict {
		t.Fatalf("a key clash must be a conflict even when the text merge is clean: conflict=%v err=%v", conflict, err)
	}

	events, err := gitmerge.Events(tool.Root)
	if err != nil || len(events) != 2 || events[0].Outcome != gitmerge.OutcomeResolved || events[1].Outcome != gitmerge.OutcomeConflict {
		t.Fatalf("journal records declared catalogs only: %v %+v", err, events)
	}
}
