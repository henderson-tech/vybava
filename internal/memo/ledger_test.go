package memo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newHome(t *testing.T, kind Kind, rows ...string) *Ledger {
	t.Helper()
	dir := t.TempDir()
	l, err := Create(filepath.Join(dir, LedgerFile), "t", kind, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if err := appendLine(l.Path, r); err != nil {
			t.Fatal(err)
		}
	}
	l, d, err := Load(l.Path)
	if err != nil || d != nil {
		t.Fatalf("Load: %v %v", err, d)
	}
	return l
}

func TestRowRoundTrip(t *testing.T) {
	cases := []string{
		"- #45 feedback/git Never `git stash`; parallel sessions share the tree. -> [[notes/git-stash-race]] ^m45",
		"- #46 reference/macos! AX exposes only the current Space; an off-Space frame() hangs until timeout. ^m46",
		"- #61 feedback/git supersedes #45: stash is fine inside `.worktrees/`. ^m61",
		"- #62 feedback/git retires #46. The rule moved into claude-guards. -> [[LEDGER#^m45]] [[fixit-team/notes/ax]] https://x.y/z ^m62",
	}
	for _, line := range cases {
		row, d := ParseRow(line)
		if d != nil {
			t.Fatalf("%s: %v", line, d)
		}
		if got := row.Format(); got != line {
			t.Errorf("round trip\n got %s\nwant %s", got, line)
		}
	}
	if r, _ := ParseRow(cases[2]); r.Supersedes() != 45 {
		t.Errorf("Supersedes() = %d", r.Supersedes())
	}
	if r, _ := ParseRow(cases[3]); r.Retires() != 46 || !strings.HasPrefix(r.Sentence, "retires #46.") {
		t.Errorf("Retires() = %d", r.Retires())
	}
}

func TestParseRowRefusals(t *testing.T) {
	cases := []struct{ line, code string }{
		{"- #1 feedback/git A rule \u2014 with a dash. ^m1", DiagRowLongDash},
		{"- #1 feedback/git First fact. Second fact. ^m1", DiagRowTwoSentences},
		{"- #1 feedback/git No period here ^m1", DiagRowNoPeriod},
		{"- #1 feedback/git " + strings.Repeat("x", 201) + ". ^m1", DiagRowTooLong},
		{"- #1 feedback/git Ends wrong. ^m2", DiagRowSyntax},
		{"- #1 runbook/git Unknown type. ^m1", DiagRowSyntax},
		{"- #1 feedback/Git Bad topic. ^m1", DiagRowSyntax},
		{"- #1 feedback/git Bad link. -> notes/x ^m1", DiagRowNoPeriod}, // an invalid tail is prose, and that prose lacks a period
		{"#1 feedback/git Not a bullet. ^m1", DiagRowSyntax},
		{"- #1 2026-09-20 feedback/git Dated rows are the old grammar. ^m1", DiagRowSyntax},
	}
	for _, c := range cases {
		if _, d := ParseRow(c.line); d == nil || d.Code != c.code {
			t.Errorf("%q: got %v, want %s", c.line, d, c.code)
		}
	}
	if d := ValidateSentence("retires #4. The reason follows here."); d != nil {
		t.Errorf("retire reason is one sentence: %v", d)
	}
	if d := ValidateSentence("Runs e.g. `bun x` fine."); d != nil {
		t.Errorf("abbreviation before lowercase is fine: %v", d)
	}
	if w := SentenceWarning(strings.Repeat("y", 170) + "."); w == nil || w.Code != DiagRowLong {
		t.Errorf("SentenceWarning = %v", w)
	}
}

func TestAppendRefusalsAndMonotonicIDs(t *testing.T) {
	l := newHome(t, KindPersonal, "- #3 feedback/git First. ^m3")
	if got := l.NextID(); got != 4 {
		t.Fatalf("NextID = %d", got)
	}
	cases := []struct {
		name string
		row  Row
		code string
	}{
		{"wrong home", Row{Type: "project", Topic: "api", Sentence: "Team fact."}, DiagRowTypeHome},
		{"unknown target", Row{Type: "feedback", Topic: "git", Sentence: "supersedes #9: gone."}, DiagTargetUnknown},
		{"missing note", Row{Type: "feedback", Topic: "git", Sentence: "Links away.", Links: []string{"[[notes/none]]"}}, DiagRowLinkInvalid},
		{"long dash", Row{Type: "feedback", Topic: "git", Sentence: "A \u2013 B."}, DiagRowLongDash},
	}
	for _, c := range cases {
		if _, d, err := l.Append(c.row); err != nil || d == nil || d.Code != c.code {
			t.Errorf("%s: got %v %v, want %s", c.name, d, err, c.code)
		}
		if got := l.NextID(); got != 4 {
			t.Errorf("%s: refusal must not consume an id, NextID = %d", c.name, got)
		}
	}
	row, d, err := l.Append(Row{Type: "feedback", Topic: "git", Sentence: "supersedes #3: Second."})
	if err != nil || d != nil || row.ID != 4 {
		t.Fatalf("Append: %v %v %+v", d, err, row)
	}
	if _, d, _ := l.Append(Row{Type: "feedback", Topic: "git", Sentence: "supersedes #3: Third."}); d == nil || d.Code != DiagTargetClosed {
		t.Errorf("second supersede of #3: %v", d)
	}
	reloaded, d, err := Load(l.Path)
	if err != nil || d != nil || len(reloaded.Rows) != 2 || reloaded.Rows[1].ID != 4 {
		t.Fatalf("reload: %v %v %+v", d, err, reloaded)
	}
	if status, by := reloaded.Status(3); status != "superseded" || by != 4 {
		t.Errorf("Status(3) = %s %d", status, by)
	}
}

func TestLoadRefusesNonMonotonicIDs(t *testing.T) {
	l := newHome(t, KindTeam, "- #t5 project/api A. ^t5")
	if err := appendLine(l.Path, "- #t5 project/api B. ^t5"); err != nil {
		t.Fatal(err)
	}
	if _, d, _ := Load(l.Path); d == nil || d.Code != DiagLedgerInvalid || d.Line != 8 {
		t.Errorf("Load = %v", d)
	}
}

func TestOrderAndScore(t *testing.T) {
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	l := newHome(t, KindPersonal,
		"- #1 feedback/git Old and unused. ^m1",
		"- #2 feedback/git Old but cited lately. ^m2",
		"- #3 feedback/git Fresh, never cited. ^m3",
		"- #4 feedback/git Superseded soon. ^m4",
		"- #5 feedback/git! Pinned and ancient. ^m5",
		"- #6 feedback/git supersedes #4: Replacement. ^m6",
		"- #7 feedback/git retires #3. ^m7",
		"- #8 feedback/git Touched twice. ^m8",
		"- #9 feedback/git Old, only ever added. ^m9",
		"- #10 feedback/git Legacy row without an add event. ^m10",
	)
	// Creation lives in the add events: #1, #2, #5 and #9 are ancient, the
	// rest fresh; #10 has no add event at all (a legacy ledger).
	events := []Event{
		NewEvent(1, "add", "s0", now.AddDate(0, 0, -400)),
		NewEvent(2, "add", "s0", now.AddDate(0, 0, -400)),
		NewEvent(5, "add", "s0", now.AddDate(0, 0, -400)),
		NewEvent(9, "add", "s0", now.AddDate(0, 0, -400)),
		NewEvent(9, "add", "s9", now), // a later duplicate add never rejuvenates a row
		NewEvent(3, "add", "s0", now.AddDate(0, 0, -19)),
		NewEvent(6, "add", "s0", now.AddDate(0, 0, -16)),
		NewEvent(7, "add", "s0", now.AddDate(0, 0, -15)),
		NewEvent(8, "add", "s0", now.AddDate(0, 0, -14)),
		NewEvent(2, "cite", "s1", now.AddDate(0, 0, -10)),
		NewEvent(8, "touch", "s1", now.AddDate(0, 0, -90)),
		NewEvent(6, "cite", "s1", now),
	}
	var ids []int
	for _, r := range Order(l, events, now) {
		ids = append(ids, r.Row.ID)
	}
	// pinned #5; #8 and #6 tie at 1.0 so the newer id leads; #2 decays; #10 and #7 are unscored so the newer id leads; #1 and #9 are stale, #3 retired, #4 superseded.
	want := []int{5, 8, 6, 2, 10, 7}
	if len(ids) != len(want) {
		t.Fatalf("Order ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("Order ids = %v, want %v", ids, want)
		}
	}
	if s := Score(events, 8, now); s < 0.99 || s > 1.01 {
		t.Errorf("Score(#8, 90 days) = %.3f, want 1.0", s)
	}
	if s := Score(events, 6, now); s != 1 {
		t.Errorf("Score(#6, now) = %.3f", s)
	}
	if s := Score(events, 9, now); s != 0 {
		t.Errorf("Score(#9, add only) = %.3f, want 0: add never scores", s)
	}
}

func TestRenderCapAndLayout(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	var rows []string
	for i := 1; i <= 120; i++ {
		rows = append(rows, Row{ID: i, Type: "feedback", Topic: "t", Sentence: "Fact."}.Format())
	}
	rows = append(rows, "- #121 user/me! Pinned. ^m121")
	l := newHome(t, KindPersonal, rows...)
	l.Repo = "/repo"
	out := Render(l, nil, now)
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) != MaxIndexLines {
		t.Fatalf("rendered %d lines, want %d", len(lines), MaxIndexLines)
	}
	if lines[2] != "Team memory: /repo/.claude/memory/MEMORY.md" {
		t.Errorf("routing line = %q", lines[2])
	}
	pinnedAt := indexOf(lines, "## Pinned")
	if pinnedAt < 0 || lines[pinnedAt+1] != "- #121 user/me! Pinned. ^m121" {
		t.Errorf("pinned section wrong: %v", lines[pinnedAt:pinnedAt+2])
	}
	hotAt := indexOf(lines, "## Hot")
	if hotAt < 0 || !strings.HasPrefix(lines[hotAt+1], "- #120 ") {
		t.Errorf("hot section must start with the newest row: %q", lines[hotAt+1])
	}
	if !strings.HasPrefix(lines[len(lines)-1], "- #") {
		t.Errorf("last line must be a row, got %q", lines[len(lines)-1])
	}
	changed, err := WriteIndex(l, nil, now)
	if err != nil || !changed {
		t.Fatalf("WriteIndex: %v %v", changed, err)
	}
	if same, _ := CheckIndex(l, nil, now); !same {
		t.Error("CheckIndex after WriteIndex must be true")
	}
	if err := os.WriteFile(filepath.Join(l.Home(), IndexFile), []byte("# hand edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if same, _ := CheckIndex(l, nil, now); same {
		t.Error("CheckIndex must detect drift")
	}
}

func indexOf(lines []string, want string) int {
	for i, l := range lines {
		if l == want {
			return i
		}
	}
	return -1
}

func TestImportAssignsIDsInOrder(t *testing.T) {
	l := newHome(t, KindTeam, "- #t2 project/api Existing. ^t2")
	// A pre-amendment draft carries a leading date; it is accepted and dropped.
	rows, ds := ParseImport([]byte("# comment\n\n- project/api! First. -> https://a.b\n- 2026-09-21 reference/db supersedes #t3: Second.\n"))
	if ds != nil {
		t.Fatal(ds[0])
	}
	added, ds, err := l.Import(rows)
	if err != nil || ds != nil || len(added) != 2 || added[0].ID != 3 || added[1].ID != 4 || !added[0].Pinned {
		t.Fatalf("Import: %v %v %+v", ds, err, added)
	}
	if added[1].Format() != "- #t4 reference/db supersedes #t3: Second. ^t4" {
		t.Errorf("dated import row must land without the date: %q", added[1].Format())
	}
	if status, by := l.Status(3); status != "superseded" || by != 4 {
		t.Errorf("import-internal supersede: %s %d", status, by)
	}
	bad, ds := ParseImport([]byte("- project/api Fine.\n- project/api No period\n- project/api A \u2014 B.\n"))
	if bad != nil || len(ds) != 2 || ds[0].Code != DiagRowNoPeriod || ds[0].Line != 2 || ds[1].Code != DiagRowLongDash || ds[1].Line != 3 {
		t.Errorf("ParseImport must report every bad line: %v", ds)
	}
	rows, _ = ParseImport([]byte("- project/api Ok.\n- user/me Wrong home.\n"))
	before := len(l.Rows)
	if _, ds, _ := l.Import(rows); len(ds) != 1 || ds[0].Code != DiagRowTypeHome || len(l.Rows) != before {
		t.Errorf("Import must be all-or-nothing: %v rows=%d", ds, len(l.Rows))
	}
}

func TestParseRef(t *testing.T) {
	cases := []struct {
		in    string
		alias string
		id    int
		ok    bool
	}{
		{"45", "", 45, true}, {"#45", "", 45, true}, {"^m45", "", 45, true},
		{"fixit-team#12", "fixit-team", 12, true}, {"[[LEDGER#^m7]]", "", 7, true},
		{"[[vybava/LEDGER#^m8]]", "vybava", 8, true}, {"abc", "", 0, false}, {"#", "", 0, false},
	}
	for _, c := range cases {
		ref, d := ParseRef(c.in)
		if (d == nil) != c.ok || ref.Alias != c.alias || ref.ID != c.id {
			t.Errorf("ParseRef(%q) = %+v %v", c.in, ref, d)
		}
	}
}

func TestSlugs(t *testing.T) {
	if got := ProjectSlug("/Users/me/.claude"); got != "-Users-me--claude" {
		t.Errorf("ProjectSlug = %q", got)
	}
	if got := Slugify("FixIt Technologies"); got != "fixit-technologies" {
		t.Errorf("Slugify = %q", got)
	}
}

func TestRegistryRejectsUnknownFields(t *testing.T) {
	env := Env{UserHome: t.TempDir(), Cwd: t.TempDir()}
	if err := os.MkdirAll(filepath.Dir(env.RegistryPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.RegistryPath(), []byte(`{"version":1,"homes":[],"extra":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, d, _ := env.LoadRegistry(); d == nil || d.Code != DiagRegistryInvalid {
		t.Errorf("LoadRegistry = %v", d)
	}
}

func TestImportSkipsHeadingsRefusesProse(t *testing.T) {
	rows, ds := ParseImport([]byte("# personal\n\n## git\n- feedback/git One.\n\n### deep\n- feedback/git Two.\n"))
	if ds != nil || len(rows) != 2 {
		t.Fatalf("headings must be skipped: %v %d", ds, len(rows))
	}
	_, ds = ParseImport([]byte("## git\n- feedback/git One.\nstray prose line\n"))
	if len(ds) != 1 || ds[0].Code != DiagImportInvalid || ds[0].Line != 3 {
		t.Errorf("prose must be a line-numbered diagnostic: %v", ds)
	}
}

func TestArrowInsideSentence(t *testing.T) {
	cases := []struct {
		rest     string
		sentence string
		links    int
	}{
		{`never chain "watcher done -> merge" in one breath.`, `never chain "watcher done -> merge" in one breath.`, 0},
		{`a pin (react -> @types/react) kills it. -> [[notes/x]] https://a.b`, `a pin (react -> @types/react) kills it.`, 2},
		{`x -> y. -> [[LEDGER#^m3]]`, `x -> y.`, 1},
	}
	for _, c := range cases {
		s, l := splitLinks(c.rest)
		if s != c.sentence || len(l) != c.links {
			t.Errorf("splitLinks(%q) = %q %v", c.rest, s, l)
		}
	}
	line := "- #7 feedback/pr Never chain \"done -> merge\" in one breath. ^m7"
	if r, d := ParseRow(line); d != nil || r.Format() != line {
		t.Errorf("round trip with arrow in prose: %v %q", d, r.Format())
	}
}
