package memo

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// File names inside a home. usage.jsonl and MEMORY.md are derived; LEDGER.md
// is the only file a human or agent ever appends to, and only via `memo add`.
const (
	LedgerFile = "LEDGER.md"
	IndexFile  = "MEMORY.md"
	UsageFile  = "usage.jsonl"
	NotesDir   = "notes"

	MaxSentence  = 200
	WarnSentence = 160
)

// Kind is where a home lives; it decides which row types it accepts.
type Kind string

const (
	KindPersonal Kind = "personal"
	KindTeam     Kind = "team"
)

// Types is the closed note-type vocabulary; TypeKind maps each onto the home
// that owns it.
var TypeKind = map[string]Kind{"user": KindPersonal, "feedback": KindPersonal, "project": KindTeam, "reference": KindTeam}

// Row is one ledger line. Sentence excludes the trailing block id. A row
// carries no date: its creation is the `add` event in usage.jsonl.
type Row struct {
	ID       int      `json:"id"`
	Type     string   `json:"type"`
	Topic    string   `json:"topic"`
	Pinned   bool     `json:"pinned"`
	Team     bool     `json:"team,omitempty"` // team ledgers prefix ids with t: #t12 ... ^t12
	Sentence string   `json:"sentence"`
	Links    []string `json:"links,omitempty"`
	Line     int      `json:"line,omitempty"`
}

// Ledger is a parsed LEDGER.md plus its frontmatter.
type Ledger struct {
	Path  string
	Alias string
	Kind  Kind
	Repo  string
	Rows  []Row
}

// Home returns the directory the ledger sits in.
func (l *Ledger) Home() string { return filepath.Dir(l.Path) }

var (
	// A row carries no date (amendment 2026-09-21). A ledger written before
	// that amendment still leads each row with `YYYY-MM-DD`; the parser
	// accepts and drops it — the same tolerance `memo import` has — so a
	// legacy ledger keeps reading, rendering and taking `memo add` instead
	// of refusing at its first row with no verb able to repair it.
	rowPattern      = regexp.MustCompile(`^- #(t?)(\d+) (?:\d{4}-\d{2}-\d{2} )?([a-z]+)/([a-z0-9]+(?:-[a-z0-9]+)*)(!?) (.+) \^([mt])(\d+)$`)
	headPattern     = regexp.MustCompile(`^([a-z]+)/([a-z0-9]+(?:-[a-z0-9]+)*)(!?)$`)
	supersedesRE    = regexp.MustCompile(`^supersedes #t?(\d+): `)
	retiresRE       = regexp.MustCompile(`^retires #t?(\d+)\.(?: |$)`)
	sentenceBreakRE = regexp.MustCompile(`[.!?]\s+\p{Lu}`)
	longDashRE      = regexp.MustCompile("[\u2013\u2014]") // en dash, em dash
	noteLinkRE      = regexp.MustCompile(`^\[\[notes/([a-z0-9]+(?:-[a-z0-9]+)*)\]\]$`)
	rowLinkRE       = regexp.MustCompile(`^\[\[LEDGER#\^[mt](\d+)\]\]$`)
	aliasRowLinkRE  = regexp.MustCompile(`^\[\[([a-z0-9]+(?:-[a-z0-9]+)*)/LEDGER#\^[mt](\d+)\]\]$`)
	aliasNoteLinkRE = regexp.MustCompile(`^\[\[([a-z0-9]+(?:-[a-z0-9]+)*)/notes/([a-z0-9]+(?:-[a-z0-9]+)*)\]\]$`)
	grammarComment  = "<!-- - #<id> <type>/<topic>[!] <sentence> [-> <link> ...] ^m<id>  (team ledger: #t<id> ... ^t<id>) -->"
)

// Format renders a row in the exact ledger grammar.
func (r Row) Format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "- #%s%d %s/%s", r.IDPrefix(), r.ID, r.Type, r.Topic)
	if r.Pinned {
		b.WriteString("!")
	}
	b.WriteString(" ")
	b.WriteString(r.Sentence)
	if len(r.Links) > 0 {
		b.WriteString(" -> ")
		b.WriteString(strings.Join(r.Links, " "))
	}
	fmt.Fprintf(&b, " ^%s%d", r.BlockPrefix(), r.ID)
	return b.String()
}

// IDPrefix is "t" for a team row, "" for a personal one; BlockPrefix is the
// matching Obsidian block-id letter (t / m).
func (r Row) IDPrefix() string {
	if r.Team {
		return "t"
	}
	return ""
}

func (r Row) BlockPrefix() string {
	if r.Team {
		return "t"
	}
	return "m"
}

// Cite is the short citation form: #12 or #t12.
func (r Row) Cite() string { return "#" + r.IDPrefix() + strconv.Itoa(r.ID) }

// Supersedes returns the id this row supersedes, or 0.
func (r Row) Supersedes() int { return firstInt(supersedesRE, r.Sentence) }

// Retires returns the id this row retires, or 0.
func (r Row) Retires() int { return firstInt(retiresRE, r.Sentence) }

func firstInt(re *regexp.Regexp, s string) int {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// ParseRow reads one ledger line. The trailing block id must equal #id and
// the sentence part must pass ValidateSentence.
func ParseRow(line string) (Row, *Diag) {
	m := rowPattern.FindStringSubmatch(line)
	if m == nil {
		return Row{}, errorDiag(DiagRowSyntax, "line is not a ledger row: "+line, "")
	}
	team := m[1] == "t"
	id, _ := strconv.Atoi(m[2])
	block, _ := strconv.Atoi(m[8])
	if id != block || (m[7] == "t") != team {
		return Row{}, errorDiag(DiagRowSyntax, fmt.Sprintf("row #%s%d ends with ^%s%d; the block id must equal the row id (personal ^m, team ^t)", m[1], id, m[7], block), "")
	}
	if _, ok := TypeKind[m[3]]; !ok {
		return Row{}, errorDiag(DiagRowSyntax, fmt.Sprintf("row #%s%d has type %q; use user, feedback, project or reference", m[1], id, m[3]), "")
	}
	sentence, links := splitLinks(m[6])
	if d := ValidateSentence(sentence); d != nil {
		d.Detail = fmt.Sprintf("row #%d: %s", id, d.Detail)
		return Row{}, d
	}
	for _, l := range links {
		if d := validateLink(l); d != nil {
			d.Detail = fmt.Sprintf("row #%d: %s", id, d.Detail)
			return Row{}, d
		}
	}
	return Row{ID: id, Team: team, Type: m[3], Topic: m[4], Pinned: m[5] == "!", Sentence: sentence, Links: links}, nil
}

// ParseHead reads the `<type>/<topic>[!]` argument of `memo add`.
func ParseHead(head string) (typ, topic string, pinned bool, d *Diag) {
	m := headPattern.FindStringSubmatch(head)
	if m == nil {
		return "", "", false, errorDiag(DiagRowSyntax, fmt.Sprintf("%q is not <type>/<topic>[!]; the topic is kebab-case (git, api-data)", head), "memo add feedback/git \"<sentence>.\"")
	}
	if _, ok := TypeKind[m[1]]; !ok {
		return "", "", false, errorDiag(DiagRowSyntax, fmt.Sprintf("type %q is not one of user, feedback, project, reference", m[1]), "memo add feedback/"+m[2]+" \"<sentence>.\"")
	}
	return m[1], m[2], m[3] == "!", nil
}

// splitLinks separates the trailing link list from the sentence. A sentence
// may itself contain ` -> ` ("watcher done -> merge"), so the separator is
// the LAST arrow whose tail is made only of valid link tokens.
func splitLinks(rest string) (string, []string) {
	for at := strings.LastIndex(rest, " -> "); at >= 0; at = strings.LastIndex(rest[:at], " -> ") {
		links := strings.Fields(rest[at+4:])
		if len(links) == 0 || !allLinks(links) {
			continue
		}
		return rest[:at], links
	}
	return rest, nil
}

func allLinks(tokens []string) bool {
	for _, t := range tokens {
		if validateLink(t) != nil {
			return false
		}
	}
	return true
}

// ValidateSentence enforces: one line, one sentence, plain hyphens, ends with
// a period, at most MaxSentence characters, no reserved tokens.
func ValidateSentence(s string) *Diag {
	if strings.ContainsAny(s, "\n\r") || strings.Contains(s, "^m") {
		return errorDiag(DiagRowSyntax, "sentence must be one line without `^m`", "")
	}
	if strings.TrimSpace(s) != s || s == "" {
		return errorDiag(DiagRowSyntax, "sentence must not be empty or padded with spaces", "")
	}
	if longDashRE.MatchString(s) {
		return errorDiag(DiagRowLongDash, "sentence carries a long dash; use a plain hyphen or restructure", "")
	}
	if !strings.HasSuffix(s, ".") {
		return errorDiag(DiagRowNoPeriod, "sentence must end with a period", "")
	}
	body := retiresRE.ReplaceAllString(s, "")
	if sentenceBreakRE.MatchString(strings.TrimSuffix(body, ".")) {
		return errorDiag(DiagRowTwoSentences, "one row is one sentence; split the second one into its own row or a note", "")
	}
	if n := len([]rune(s)); n > MaxSentence {
		return errorDiag(DiagRowTooLong, fmt.Sprintf("sentence is %d characters; the cap is %d, move the detail into a note", n, MaxSentence), "")
	}
	return nil
}

// SentenceWarning returns the soft-length warning, or nil.
func SentenceWarning(s string) *Diag {
	if n := len([]rune(s)); n > WarnSentence {
		return &Diag{Code: DiagRowLong, Severity: "warning", Detail: fmt.Sprintf("sentence is %d characters; %d reads better on the rendered surface", n, WarnSentence)}
	}
	return nil
}

func validateLink(l string) *Diag {
	switch {
	case strings.HasPrefix(l, "https://"), strings.HasPrefix(l, "http://"):
		return nil
	case noteLinkRE.MatchString(l), rowLinkRE.MatchString(l), aliasRowLinkRE.MatchString(l), aliasNoteLinkRE.MatchString(l):
		return nil
	}
	return errorDiag(DiagRowLinkInvalid, fmt.Sprintf("link %q is not [[notes/<slug>]], [[LEDGER#^m<id>]], [[<alias>/LEDGER#^m<id>]], [[<alias>/notes/<slug>]] or https://...", l), "")
}

// Load parses a LEDGER.md. A missing file is reported as os.ErrNotExist
// through err; a malformed one as a Diag.
func Load(path string) (*Ledger, *Diag, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	l := &Ledger{Path: path}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	line, inFront, seenFront := 0, false, false
	for sc.Scan() {
		line++
		text := sc.Text()
		switch {
		case line == 1 && text == "---":
			inFront, seenFront = true, true
			continue
		case inFront && text == "---":
			inFront = false
			continue
		case inFront:
			if d := l.frontmatterLine(text); d != nil {
				d.Line = line
				return nil, d, nil
			}
			continue
		case strings.TrimSpace(text) == "", strings.HasPrefix(text, "<!--"):
			continue
		}
		row, d := ParseRow(text)
		if d != nil {
			d.Line = line
			d.Fix = fmt.Sprintf("fix %s:%d by hand, then `memorylint check %s`", path, line, filepath.Dir(path))
			return nil, d, nil
		}
		row.Line = line
		if row.Team != (l.Kind == KindTeam) {
			return nil, &Diag{Code: DiagLedgerInvalid, Severity: "error", Line: line, Detail: fmt.Sprintf("row #%s%d: a %s ledger uses the %s id prefix", row.IDPrefix(), row.ID, l.Kind, map[Kind]string{KindTeam: "#t / ^t", KindPersonal: "bare # / ^m"}[l.Kind]), Fix: fmt.Sprintf("fix %s:%d by hand, then `memorylint check %s`", path, line, filepath.Dir(path))}, nil
		}
		if n := len(l.Rows); n > 0 && row.ID <= l.Rows[n-1].ID {
			return nil, &Diag{Code: DiagLedgerInvalid, Severity: "error", Line: line, Detail: fmt.Sprintf("row #%d follows #%d; ids are strictly increasing", row.ID, l.Rows[n-1].ID), Fix: fmt.Sprintf("renumber %s:%d by hand, then `memorylint check %s`", path, line, filepath.Dir(path))}, nil
		}
		l.Rows = append(l.Rows, row)
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	if !seenFront || l.Alias == "" || l.Kind == "" {
		return nil, &Diag{Code: DiagLedgerInvalid, Severity: "error", Line: 1, Detail: path + " must open with frontmatter carrying memo: 1, alias: <alias>, kind: personal|team", Fix: "add the frontmatter by hand, then `memorylint check " + filepath.Dir(path) + "`"}, nil
	}
	return l, nil, nil
}

func (l *Ledger) frontmatterLine(text string) *Diag {
	key, value, ok := strings.Cut(text, ":")
	value = strings.TrimSpace(value)
	if !ok {
		return errorDiag(DiagLedgerInvalid, "frontmatter line is not key: value: "+text, "")
	}
	switch key {
	case "memo":
		if value != "1" {
			return errorDiag(DiagLedgerInvalid, "memo: "+value+" is not a known ledger version (1)", "")
		}
	case "alias":
		l.Alias = value
	case "kind":
		if value != string(KindPersonal) && value != string(KindTeam) {
			return errorDiag(DiagLedgerInvalid, "kind must be personal or team, not "+value, "")
		}
		l.Kind = Kind(value)
	case "repo":
		l.Repo = value
	default:
		return errorDiag(DiagLedgerInvalid, "unknown frontmatter key "+key+" (memo, alias, kind, repo)", "")
	}
	return nil
}

// Create writes an empty ledger with its frontmatter and grammar comment.
func Create(path, alias string, kind Kind, repo string) (*Ledger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString("---\nmemo: 1\nalias: " + alias + "\nkind: " + string(kind) + "\n")
	if repo != "" {
		b.WriteString("repo: " + repo + "\n")
	}
	b.WriteString("---\n" + grammarComment + "\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return nil, err
	}
	return &Ledger{Path: path, Alias: alias, Kind: kind, Repo: repo}, nil
}

// NextID is the id the next appended row receives.
func (l *Ledger) NextID() int {
	if len(l.Rows) == 0 {
		return 1
	}
	return l.Rows[len(l.Rows)-1].ID + 1
}

// Find returns the row with the id, if present.
func (l *Ledger) Find(id int) (Row, bool) {
	for _, r := range l.Rows {
		if r.ID == id {
			return r, true
		}
	}
	return Row{}, false
}

// Status of a row: "active", "superseded" or "retired". Superseded/retired
// rows keep the id that closed them in the second return.
func (l *Ledger) Status(id int) (string, int) {
	for _, r := range l.Rows {
		if r.Supersedes() == id {
			return "superseded", r.ID
		}
		if r.Retires() == id {
			return "retired", r.ID
		}
	}
	return "active", 0
}

// Validate checks a candidate row against this home before appending: type
// belongs here, supersede/retire targets exist and are open, links to local
// notes and rows resolve.
func (l *Ledger) Validate(r Row) *Diag {
	if TypeKind[r.Type] != l.Kind {
		other := "team"
		if TypeKind[r.Type] == KindPersonal {
			other = "personal"
		}
		return errorDiag(DiagRowTypeHome, fmt.Sprintf("%s rows live in the %s home, not in %s (%s)", r.Type, other, l.Alias, l.Kind), fmt.Sprintf("memo add %s/%s %q  # without --home, memo picks the %s home", r.Type, r.Topic, r.Sentence, other))
	}
	for _, target := range []int{r.Supersedes(), r.Retires()} {
		if target == 0 {
			continue
		}
		if _, ok := l.Find(target); !ok {
			return errorDiag(DiagTargetUnknown, fmt.Sprintf("row #%d does not exist in %s", target, l.Alias), fmt.Sprintf("memo find <words> --home %s  # locate the row, then retry", l.Alias))
		}
		if status, by := l.Status(target); status != "active" {
			return errorDiag(DiagTargetClosed, fmt.Sprintf("row #%d is already %s by #%d", target, status, by), fmt.Sprintf("memo show %d --home %s  # supersede #%d instead", by, l.Alias, by))
		}
	}
	for _, link := range r.Links {
		if m := rowLinkRE.FindStringSubmatch(link); m != nil {
			id, _ := strconv.Atoi(m[1])
			if _, ok := l.Find(id); !ok {
				return errorDiag(DiagRowLinkInvalid, fmt.Sprintf("link %s points at a row that does not exist in %s", link, l.Alias), "")
			}
		}
		if m := noteLinkRE.FindStringSubmatch(link); m != nil {
			if _, err := os.Stat(filepath.Join(l.Home(), NotesDir, m[1]+".md")); err != nil {
				return errorDiag(DiagRowLinkInvalid, fmt.Sprintf("link %s points at a note that does not exist: %s", link, filepath.Join(NotesDir, m[1]+".md")), "")
			}
		}
	}
	return nil
}

// Append validates and appends one row, returning it with its id assigned.
func (l *Ledger) Append(r Row) (Row, *Diag, error) {
	r.ID = l.NextID()
	r.Team = l.Kind == KindTeam
	if d := ValidateSentence(r.Sentence); d != nil {
		return Row{}, d, nil
	}
	for _, link := range r.Links {
		if d := validateLink(link); d != nil {
			return Row{}, d, nil
		}
	}
	if d := l.Validate(r); d != nil {
		return Row{}, d, nil
	}
	if err := appendLine(l.Path, r.Format()); err != nil {
		return Row{}, nil, err
	}
	l.Rows = append(l.Rows, r)
	return r, nil, nil
}

func appendLine(path, line string) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if info, err := f.Stat(); err == nil && info.Size() > 0 {
		buf := make([]byte, 1)
		if _, err := f.ReadAt(buf, info.Size()-1); err == nil && buf[0] != '\n' {
			line = "\n" + line
		}
	}
	_, err = f.WriteString(line + "\n")
	return err
}
