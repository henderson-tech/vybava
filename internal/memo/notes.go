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
	base := Slugify(typ + "-" + topic)
	slug := base
	for n := 2; ; n++ {
		if _, err := os.Stat(filepath.Join(dir, slug+".md")); os.IsNotExist(err) {
			break
		}
		slug = fmt.Sprintf("%s-%d", base, n)
	}
	desc := strings.ReplaceAll(strings.ReplaceAll(description, `\`, `\\`), `"`, `\"`)
	text := fmt.Sprintf("---\nname: %s\ndescription: \"%s\"\ntype: %s\nstatus: active\nlast-verified: %s\n---\n\n%s\n",
		slug, desc, typ, now.Format("2006-01-02"), strings.TrimSpace(body))
	if err := os.WriteFile(filepath.Join(dir, slug+".md"), []byte(text), 0o644); err != nil {
		return "", err
	}
	return slug, nil
}
