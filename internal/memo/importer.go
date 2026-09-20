package memo

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

var importRowRE = regexp.MustCompile(`^- (\d{4}-\d{2}-\d{2}) ([a-z]+)/([a-z0-9]+(?:-[a-z0-9]+)*)(!?) (.+)$`)

// ParseImport reads an id-less import file: the row grammar minus `#id` and
// `^mid`; blank lines, Markdown headings and `#` comments are skipped. Every
// bad line is reported (one Diag each) and nothing is returned for import
// until all of them are fixed. Home rules are checked by Import.
func ParseImport(data []byte) ([]Row, []*Diag) {
	var rows []Row
	var diags []*Diag
	bad := func(d *Diag) { diags = append(diags, d) }
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	line := 0
	for sc.Scan() {
		line++
		text := sc.Text()
		if strings.TrimSpace(text) == "" || strings.HasPrefix(text, "#") {
			continue // blank, Markdown heading (#, ##, ...) or comment: a grouped draft imports as-is
		}
		m := importRowRE.FindStringSubmatch(text)
		if m == nil {
			bad(&Diag{Code: DiagImportInvalid, Severity: "error", Line: line, Detail: fmt.Sprintf("line %d is neither a row `- <YYYY-MM-DD> <type>/<topic>[!] <sentence> [-> <link> ...]` nor a heading or blank line: %s", line, text), Fix: fmt.Sprintf("edit line %d, then re-run memo import", line)})
			continue
		}
		if _, ok := TypeKind[m[2]]; !ok {
			bad(&Diag{Code: DiagImportInvalid, Severity: "error", Line: line, Detail: fmt.Sprintf("line %d: type %q is not user, feedback, project or reference", line, m[2]), Fix: fmt.Sprintf("edit line %d, then re-run memo import", line)})
			continue
		}
		sentence, links := splitLinks(m[5])
		if d := ValidateSentence(sentence); d != nil {
			d.Line = line
			d.Detail = fmt.Sprintf("line %d: %s", line, d.Detail)
			d.Fix = fmt.Sprintf("edit line %d, then re-run memo import", line)
			bad(d)
			continue
		}
		linkOK := true
		for _, l := range links {
			if d := validateLink(l); d != nil {
				d.Line = line
				d.Detail = fmt.Sprintf("line %d: %s", line, d.Detail)
				d.Fix = fmt.Sprintf("edit line %d, then re-run memo import", line)
				bad(d)
				linkOK = false
				break
			}
		}
		if !linkOK {
			continue
		}
		rows = append(rows, Row{Date: m[1], Type: m[2], Topic: m[3], Pinned: m[4] == "!", Sentence: sentence, Links: links, Line: line})
	}
	if err := sc.Err(); err != nil {
		bad(errorDiag(DiagImportInvalid, err.Error(), ""))
	}
	if len(diags) > 0 {
		return nil, diags
	}
	return rows, nil
}

// Import assigns ids in file order and appends every row. Validation is
// all-or-nothing against a dry copy of the ledger, so a bad line 12 leaves
// lines 1 to 11 unwritten.
func (l *Ledger) Import(rows []Row) ([]Row, []*Diag, error) {
	dry := &Ledger{Path: l.Path, Alias: l.Alias, Kind: l.Kind, Repo: l.Repo, Rows: append([]Row(nil), l.Rows...)}
	var diags []*Diag
	for i := range rows {
		rows[i].ID = dry.NextID()
		rows[i].Team = l.Kind == KindTeam
		if d := dry.Validate(rows[i]); d != nil {
			d.Detail = fmt.Sprintf("line %d: %s", rows[i].Line, d.Detail)
			d.Line = rows[i].Line
			diags = append(diags, d)
		}
		dry.Rows = append(dry.Rows, rows[i])
	}
	if len(diags) > 0 {
		return nil, diags, nil
	}
	var out []Row
	for _, r := range rows {
		r.Line = 0
		if err := appendLine(l.Path, r.Format()); err != nil {
			return out, nil, err
		}
		l.Rows = append(l.Rows, r)
		out = append(out, r)
	}
	return out, nil, nil
}

// MigrateNote is one v2 note found by `memo migrate`.
type MigrateNote struct {
	Path        string   `json:"path"`
	Name        string   `json:"name"`
	Type        string   `json:"type"`
	Description string   `json:"description"`
	Bullets     []string `json:"bullets"`
}

// Migrate lists the v2 notes of a home and renders an import template with
// one row per top-level bullet. The template is a starting point for a
// human; every generated sentence still has to pass `memo import`.
func Migrate(home string, today time.Time) ([]MigrateNote, string, error) {
	entries, err := os.ReadDir(home)
	if err != nil {
		return nil, "", err
	}
	var notes []MigrateNote
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || name == IndexFile || name == LedgerFile {
			continue
		}
		note, ok := readNote(filepath.Join(home, name))
		if ok {
			notes = append(notes, note)
		}
	}
	sort.Slice(notes, func(i, j int) bool { return notes[i].Path < notes[j].Path })
	var b strings.Builder
	date := today.Format("2006-01-02")
	fmt.Fprintf(&b, "# memo import template for %s (%d notes); lines starting with # are ignored\n", home, len(notes))
	b.WriteString("# grammar: - <YYYY-MM-DD> <type>/<topic>[!] <sentence> [-> <link> ...]\n")
	for _, n := range notes {
		fmt.Fprintf(&b, "\n# %s: %s\n", filepath.Base(n.Path), n.Description)
		topic := strings.TrimPrefix(strings.TrimSuffix(filepath.Base(n.Path), ".md"), n.Type+"-")
		if topic == "" {
			topic = "topic"
		}
		for _, bullet := range n.Bullets {
			fmt.Fprintf(&b, "# %s\n- %s %s/%s <one sentence>.\n", bullet, date, n.Type, Slugify(topic))
		}
		if len(n.Bullets) == 0 {
			fmt.Fprintf(&b, "- %s %s/%s <one sentence>.\n", date, n.Type, Slugify(topic))
		}
	}
	return notes, b.String(), nil
}

func readNote(path string) (MigrateNote, bool) {
	data, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(data, []byte("---\n")) {
		return MigrateNote{}, false
	}
	end := bytes.Index(data[4:], []byte("\n---"))
	if end < 0 {
		return MigrateNote{}, false
	}
	var fm struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description"`
		Type        string `yaml:"type"`
		Metadata    struct {
			Type string `yaml:"type"`
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal(data[4:4+end], &fm); err != nil {
		return MigrateNote{}, false
	}
	if fm.Type == "" {
		fm.Type = fm.Metadata.Type
	}
	note := MigrateNote{Path: path, Name: fm.Name, Type: fm.Type, Description: fm.Description, Bullets: []string{}}
	for _, line := range strings.Split(string(data[4+end:]), "\n") {
		if strings.HasPrefix(line, "- ") {
			note.Bullets = append(note.Bullets, strings.TrimSpace(strings.TrimPrefix(line, "- ")))
		}
	}
	return note, true
}
