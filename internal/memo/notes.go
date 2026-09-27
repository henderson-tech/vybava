package memo

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// WriteNote creates `notes/<slug>.md` under home in the v2 note form (name,
// description, type, status, last-verified) with body as its text, and
// returns the slug for a `[[notes/<slug>]]` link. The slug is
// `<type>-<topic>`, numbered when that note already exists: a note is never
// overwritten. It is `memo add --note`'s one-call path, and where a second
// sentence NormalizeSentence split off lands.
func WriteNote(home, typ, topic, description, body string, now time.Time) (string, error) {
	dir := filepath.Join(home, "notes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	base := NoteSlug(typ, topic)
	desc := strings.ReplaceAll(strings.ReplaceAll(description, `\`, `\\`), `"`, `\"`)
	// Create exclusively and move to the next number on a collision: a stat
	// followed by a write would let two concurrent adds pick the same slug
	// and the later one truncate the earlier note.
	for n := 1; ; n++ {
		slug := base
		if n > 1 {
			slug = fmt.Sprintf("%s-%d", base, n)
		}
		f, err := os.OpenFile(filepath.Join(dir, slug+".md"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		text := fmt.Sprintf("---\nname: %s\ndescription: \"%s\"\ntype: %s\nstatus: active\nlast-verified: %s\n---\n\n%s\n",
			slug, desc, typ, now.Format("2006-01-02"), strings.TrimSpace(body))
		if _, err := f.WriteString(text); err != nil {
			f.Close()
			return "", err
		}
		if err := f.Close(); err != nil {
			return "", err
		}
		return slug, nil
	}
}

// NoteSlug is the slug WriteNote starts from for a row: <type>-<topic>.
// A caller that validates the row before writing the note links this
// prospective slug; the written one differs only by a number on collision.
func NoteSlug(typ, topic string) string { return Slugify(typ + "-" + topic) }
