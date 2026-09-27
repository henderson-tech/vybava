package memo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A note lands as v2 frontmatter plus the body, under a <type>-<topic> slug
// that is numbered rather than overwritten when it already exists.
func TestWriteNote(t *testing.T) {
	home := t.TempDir()
	now := time.Date(2026, 9, 27, 21, 0, 0, 0, time.UTC)
	slug, err := WriteNote(home, "feedback", "zsh loops", `Never name it "path".`, "Seen 1,765 times.\n\nSecond paragraph.", now)
	if err != nil || slug != "feedback-zsh-loops" {
		t.Fatalf("slug %q, err %v", slug, err)
	}
	raw, err := os.ReadFile(filepath.Join(home, "notes", slug+".md"))
	if err != nil {
		t.Fatal(err)
	}
	want := "---\nname: feedback-zsh-loops\ndescription: \"Never name it \\\"path\\\".\"\ntype: feedback\nstatus: active\nlast-verified: 2026-09-27\n---\n\nSeen 1,765 times.\n\nSecond paragraph.\n"
	if string(raw) != want {
		t.Errorf("note:\n%s\nwant:\n%s", raw, want)
	}
	again, err := WriteNote(home, "feedback", "zsh loops", "x.", "more", now)
	if err != nil || again != "feedback-zsh-loops-2" {
		t.Fatalf("second note: slug %q, err %v", again, err)
	}
	if raw2, _ := os.ReadFile(filepath.Join(home, "notes", slug+".md")); !strings.Contains(string(raw2), "Seen 1,765 times.") {
		t.Errorf("the first note was overwritten")
	}
}
