package memo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScanTranscriptCitations(t *testing.T) {
	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Per #45 and [[LEDGER#^m46]] plus ^m47, see [[fixit-team/LEDGER#^m12]]; PR #1550 is a PR, x#9 and ##8 are not."}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"/h/notes/git-stash-race.md"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"memo show 45 --json && memo show fixit-team#12"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"text","text":"#99 from the user does not count"}]}}`,
		`not json`,
		`{"type":"assistant","message":{"content":"plain string #50."}}`,
	}
	c, err := scanTranscript(strings.NewReader(strings.Join(lines, "\n") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int{45, 46, 47, 50, 1550} {
		if !c.Cites[id] {
			t.Errorf("cite %d missing: %v", id, c.Cites)
		}
	}
	for _, id := range []int{9, 8, 99} {
		if c.Cites[id] {
			t.Errorf("cite %d must not count", id)
		}
	}
	if !c.Scoped["fixit-team"][12] {
		t.Errorf("scoped cite missing: %v", c.Scoped)
	}
	if !c.Shows[45] {
		t.Errorf("show 45 missing: %v", c.Shows)
	}
	if !c.Reads["/h/notes/git-stash-race.md"] {
		t.Errorf("read missing: %v", c.Reads)
	}
}

func TestHookStopRecordsDedupedEvents(t *testing.T) {
	user := t.TempDir()
	repo := t.TempDir()
	env := Env{UserHome: user, Cwd: repo}
	personal, _ := env.SessionHomes()
	if _, err := Create(filepath.Join(personal.Path, LedgerFile), "t", KindPersonal, repo); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(personal.Path, NotesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(personal.Path, NotesDir, "race.md"), []byte("---\nname: race\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{
		"- #1 2026-09-20 feedback/git Cited. ^m1",
		"- #2 2026-09-20 feedback/git Read via note. -> [[notes/race]] ^m2",
		"- #3 2026-09-20 feedback/git Never used. ^m3",
	} {
		if err := appendLine(filepath.Join(personal.Path, LedgerFile), r); err != nil {
			t.Fatal(err)
		}
	}
	transcript := filepath.Join(t.TempDir(), "s.jsonl")
	body := `{"type":"assistant","message":{"content":[{"type":"text","text":"Doing #1 and #1 again, #77 does not exist."},{"type":"tool_use","name":"Read","input":{"file_path":"` + filepath.Join(personal.Path, NotesDir, "race.md") + `"}}]}}` + "\n"
	if err := os.WriteFile(transcript, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	payload := HookPayload{HookEventName: "Stop", Cwd: repo, SessionID: "sess-1", TranscriptPath: transcript}
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	res, err := env.RunHook(payload, now)
	if err != nil || res.Recorded != 2 || len(res.Rendered) != 1 {
		t.Fatalf("first Stop: %+v %v", res, err)
	}
	res, err = env.RunHook(payload, now.Add(time.Hour))
	if err != nil || res.Recorded != 0 {
		t.Fatalf("second Stop of the same session must dedup: %+v %v", res, err)
	}
	payload.SessionID = "sess-2"
	if res, _ = env.RunHook(payload, now); res.Recorded != 2 {
		t.Fatalf("another session credits again: %+v", res)
	}
	events, d, err := LoadEvents(personal.Path)
	if err != nil || d != nil || len(events) != 4 {
		t.Fatalf("events: %d %v %v", len(events), d, err)
	}
	kinds := map[string]int{}
	for _, e := range events {
		kinds[e.Kind]++
	}
	if kinds["cite"] != 2 || kinds["read"] != 2 {
		t.Errorf("kinds = %v", kinds)
	}
}

func TestHookPreToolUseRefusesHandWrites(t *testing.T) {
	home := t.TempDir()
	if _, err := Create(filepath.Join(home, LedgerFile), "t", KindTeam, ""); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(home, LedgerFile)
	index := filepath.Join(home, IndexFile)
	usage := filepath.Join(home, UsageFile)
	other := filepath.Join(t.TempDir(), "MEMORY.md") // no LEDGER.md beside it
	cases := []struct {
		name, tool, path, command string
		refuse                    bool
	}{
		{"edit ledger", "Edit", ledger, "", true},
		{"write index", "Write", index, "", true},
		{"write usage", "Write", usage, "", true},
		{"edit note", "Edit", filepath.Join(home, "notes", "x.md"), "", false},
		{"legacy index", "Edit", other, "", false},
		{"heredoc", "Bash", "", "cat > " + ledger + " <<'EOF'\n- #9\nEOF", true},
		{"append redirect", "Bash", "", "echo '- #9 x' >> " + ledger, true},
		{"sed in place", "Bash", "", "sed -i '' 's/a/b/' " + index, true},
		{"sed -i.bak", "Bash", "", "sed -i.bak -e 's/a/b/' " + usage, true},
		{"tee", "Bash", "", "printf x | tee " + ledger, true},
		{"read only", "Bash", "", "grep -n stash " + ledger + " | head", false},
		{"sed print", "Bash", "", "sed -n '1,20p' " + ledger, false},
		{"quoted mention", "Bash", "", "echo 'sed -i x " + ledger + "'", false},
		{"relative cwd", "Bash", "", "sed -i '' 's/a/b/' LEDGER.md", true},
	}
	for _, c := range cases {
		p := HookPayload{HookEventName: "PreToolUse", ToolName: c.tool, Cwd: home}
		p.ToolInput.FilePath = c.path
		p.ToolInput.Command = c.command
		d := RefuseHandWrite(p)
		if (d != nil) != c.refuse {
			t.Errorf("%s: refused=%v (%v)", c.name, d != nil, d)
			continue
		}
		if d != nil && (d.Code != DiagHookRefused || !strings.Contains(d.Fix, "memo ")) {
			t.Errorf("%s: diag %+v", c.name, d)
		}
	}
}

func TestVaultIdempotent(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "Memory")
	homes := []Home{{Alias: "a", Path: t.TempDir()}, {Alias: "b", Path: t.TempDir()}}
	first, err := Vault(vault, homes)
	if err != nil || first.Config != "created" || first.Entries[0].Action != "created" {
		t.Fatalf("first: %+v %v", first, err)
	}
	homes[1].Path = t.TempDir()
	second, err := Vault(vault, homes)
	if err != nil || second.Config != "kept" || second.Entries[0].Action != "kept" || second.Entries[1].Action != "updated" {
		t.Fatalf("second: %+v %v", second, err)
	}
	if target, _ := os.Readlink(filepath.Join(vault, "b")); target != homes[1].Path {
		t.Errorf("b -> %s", target)
	}
}

func TestSnapshotLogRestore(t *testing.T) {
	home := Home{Alias: "t", Path: t.TempDir(), Kind: KindPersonal}
	l, err := Create(filepath.Join(home.Path, LedgerFile), "t", KindPersonal, "")
	if err != nil {
		t.Fatal(err)
	}
	rev, d, err := Snapshot(home, "one")
	if err != nil || d != nil || rev == "" {
		t.Fatalf("Snapshot: %s %v %v", rev, d, err)
	}
	if _, d, _ := Snapshot(home, "again"); d == nil || d.Code != DiagSnapshotClean {
		t.Errorf("clean snapshot: %v", d)
	}
	if err := appendLine(l.Path, "- #1 2026-09-20 feedback/git Row. ^m1"); err != nil {
		t.Fatal(err)
	}
	if _, d, err := Snapshot(home, "two"); err != nil || d != nil {
		t.Fatalf("second: %v %v", d, err)
	}
	entries, d, err := Log(home, 10)
	if err != nil || d != nil || len(entries) != 2 || entries[0].Message != "two" {
		t.Fatalf("Log: %+v %v %v", entries, d, err)
	}
	if d, err := Restore(home, entries[1].Rev, LedgerFile); err != nil || d != nil {
		t.Fatalf("Restore: %v %v", d, err)
	}
	data, _ := os.ReadFile(l.Path)
	if strings.Contains(string(data), "#1") {
		t.Error("restore must bring the older ledger back")
	}
	if _, d, _ := Snapshot(Home{Path: home.Path, Kind: KindTeam}, "x"); d == nil || d.Code != DiagSnapshotTeamOwned {
		t.Errorf("team home: %v", d)
	}
}

func TestLintLedgerHome(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	l := newHome(t, KindTeam,
		"- #t1 2026-09-20 project/api Fine. ^t1",
		"- #t2 2026-09-20 project/api supersedes #t1: Once. ^t2",
		"- #t3 2026-09-20 project/api supersedes #t1: Twice. ^t3",
		"- #t4 2026-09-20 project/api Points nowhere. -> [[notes/none]] [[nope/LEDGER#^t1]] ^t4",
	)
	if err := os.MkdirAll(filepath.Join(l.Home(), NotesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(l.Home(), NotesDir, "orphan.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	rules := map[string]int{}
	for _, f := range Lint(l.Home(), nil, now) {
		rules[f.Rule]++
	}
	if rules[RuleTarget] != 1 || rules[RuleLink] != 2 || rules[RuleDrift] != 1 || rules[RuleNoteOrphan] != 1 {
		t.Errorf("rules = %v", rules)
	}
}

// TestSnapshotInsideExistingWorkTree pins the Claudik case: the personal home
// sits inside an already tracked repo, so memo never git-inits, stages only
// the home's own files and leaves the repo's other dirty files alone.
func TestSnapshotInsideExistingWorkTree(t *testing.T) {
	root := t.TempDir()
	if out, err := git(root, "init", "-q"); err != nil {
		t.Fatal(out)
	}
	unrelated := filepath.Join(root, "CLAUDE.md")
	if err := os.WriteFile(unrelated, []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	home := Home{Alias: "fixit", Kind: KindPersonal, Path: filepath.Join(root, "projects", "slug", "memory")}
	if _, err := Create(filepath.Join(home.Path, LedgerFile), "fixit", KindPersonal, ""); err != nil {
		t.Fatal(err)
	}
	rev, d, err := Snapshot(home, "")
	if err != nil || d != nil || rev == "" {
		t.Fatalf("Snapshot: %s %v %v", rev, d, err)
	}
	if _, err := os.Stat(filepath.Join(home.Path, ".git")); err == nil {
		t.Fatal("must not git init inside an existing work tree")
	}
	status, _ := git(root, "status", "--porcelain")
	if !strings.Contains(status, "?? CLAUDE.md") || strings.Contains(status, "memory") {
		t.Errorf("unrelated file must stay untracked and the home must be committed: %q", status)
	}
	entries, d, err := Log(home, 5)
	if err != nil || d != nil || len(entries) != 1 || entries[0].Message != "memo: snapshot fixit" {
		t.Fatalf("Log: %+v %v %v", entries, d, err)
	}
	ledger := filepath.Join(home.Path, LedgerFile)
	if err := appendLine(ledger, "- #1 2026-09-20 feedback/git Row. ^m1"); err != nil {
		t.Fatal(err)
	}
	if d, err := Restore(home, entries[0].Rev, LedgerFile); err != nil || d != nil {
		t.Fatalf("Restore: %v %v", d, err)
	}
	if data, _ := os.ReadFile(ledger); strings.Contains(string(data), "#1") {
		t.Error("restore must bring the snapshot back")
	}
	if data, _ := os.ReadFile(unrelated); string(data) != "dirty\n" {
		t.Error("unrelated file was touched")
	}
}

// TestHookStopCreditsEnclosingRegisteredHome: cwd resolves to no session home,
// but lies inside a registered one, so that home is credited.
func TestHookStopCreditsEnclosingRegisteredHome(t *testing.T) {
	user := t.TempDir()
	home := filepath.Join(t.TempDir(), "memory")
	if _, err := Create(filepath.Join(home, LedgerFile), "reg", KindTeam, ""); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, NotesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, NotesDir, "x.md"), []byte("---\nname: x\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for id := 1; id <= 106; id++ {
		row := Row{ID: id, Team: true, Date: "2026-09-20", Type: "project", Topic: "t", Sentence: "Fact."}
		if id == 50 {
			row.Links = []string{"[[notes/x]]"}
		}
		if err := appendLine(filepath.Join(home, LedgerFile), row.Format()); err != nil {
			t.Fatal(err)
		}
	}
	env := Env{UserHome: user, Cwd: t.TempDir()}
	if d, err := env.Register("reg", home); d != nil || err != nil {
		t.Fatalf("Register: %v %v", d, err)
	}
	transcript := filepath.Join(t.TempDir(), "s.jsonl")
	body := `{"type":"assistant","message":{"content":[{"type":"text","text":"Per #t83 and [[LEDGER#^t106]]."},{"type":"tool_use","name":"Read","input":{"file_path":"` + filepath.Join(home, NotesDir, "x.md") + `"}}]}}` + "\n"
	if err := os.WriteFile(transcript, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	p := HookPayload{HookEventName: "Stop", Cwd: filepath.Join(home, NotesDir), SessionID: "s", TranscriptPath: transcript}
	res, err := env.RunHook(p, time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC))
	if err != nil || res.Recorded != 3 || res.Skipped != "" {
		t.Fatalf("RunHook: %+v %v", res, err)
	}
	p.Cwd = t.TempDir()
	if res, _ := env.RunHook(p, time.Now()); res.Recorded != 0 || res.Skipped == "" {
		t.Errorf("cwd outside any home must report Skipped, not block: %+v", res)
	}
}

func TestResolveHomeArgIsPathWhenDirectoryExists(t *testing.T) {
	cwd := t.TempDir()
	if _, err := Create(filepath.Join(cwd, "personal", LedgerFile), "p", KindPersonal, ""); err != nil {
		t.Fatal(err)
	}
	env := Env{UserHome: t.TempDir(), Cwd: cwd}
	homes, d, err := env.Resolve("personal", "")
	if err != nil || d != nil || len(homes) != 1 || homes[0].Path != filepath.Join(cwd, "personal") {
		t.Fatalf("Resolve(dir) = %+v %v %v", homes, d, err)
	}
	if _, d, _ := env.Resolve("nosuch", ""); d == nil || d.Code != DiagHomeNotFound {
		t.Errorf("Resolve(alias) = %v", d)
	}
}

// TestAliasDerivation: personal alias = main repo basename, team alias =
// <basename>-team, both from inside a linked worktree.
func TestAliasDerivation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "FixIt")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@x", "commit", "-q", "--allow-empty", "-m", "init"}, {"worktree", "add", "-q", filepath.Join(root, ".worktrees", "memo-ledger")}} {
		if out, err := git(root, args...); err != nil {
			t.Fatal(out)
		}
	}
	wt := filepath.Join(root, ".worktrees", "memo-ledger")
	env := Env{UserHome: t.TempDir(), Cwd: wt}
	personal, team := env.SessionHomes()
	if personal.Alias != "fixit" || team.Alias != "fixit-team" {
		t.Errorf("SessionHomes aliases = %s %s", personal.Alias, team.Alias)
	}
	l, d, err := env.Open(personal, true)
	if err != nil || d != nil || l.Alias != "fixit" || filepath.Base(l.Repo) != "FixIt" {
		t.Fatalf("personal ledger: %v %v alias=%s repo=%s", d, err, l.Alias, l.Repo)
	}
	teamPath := Home{Kind: KindTeam, Path: filepath.Join(wt, ".claude", "memory"), Alias: "claude"}
	tl, d, err := env.Open(teamPath, true)
	if err != nil || d != nil || tl.Alias != "fixit-team" {
		t.Fatalf("team ledger: %v %v alias=%s", d, err, tl.Alias)
	}
	if d, err := SetAlias(tl.Path, "fixit-main-team"); d != nil || err != nil {
		t.Fatalf("SetAlias: %v %v", d, err)
	}
	if re, d, err := Load(tl.Path); err != nil || d != nil || re.Alias != "fixit-main-team" {
		t.Errorf("SetAlias reload: %v %v %+v", d, err, re)
	}
}

// TestPersonalAliasFromSlugWhenCwdElsewhere: a personal home addressed by
// path, with the cwd outside the repo, decodes the slug back to the checkout.
func TestPersonalAliasFromSlugWhenCwdElsewhere(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "FixIt")
	if err := os.MkdirAll(filepath.Join(repo, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := git(repo, "init", "-q"); err != nil {
		t.Fatal(out)
	}
	env := Env{UserHome: t.TempDir(), Cwd: t.TempDir()}
	home := Home{Kind: KindPersonal, Path: filepath.Join(env.UserHome, ".claude", "projects", ProjectSlug(repo), "memory"), Alias: "wrong"}
	l, d, err := env.Open(home, true)
	if err != nil || d != nil || l.Alias != "fixit" || filepath.Base(l.Repo) != "FixIt" { // git reports /private/var for /var
		t.Fatalf("Open: %v %v alias=%s repo=%s", d, err, l.Alias, l.Repo)
	}
	orphan := Home{Kind: KindPersonal, Path: filepath.Join(env.UserHome, ".claude", "projects", "-nowhere-at-all", "memory"), Alias: "nowhere-at-all"}
	if l, d, err := env.Open(orphan, true); err != nil || d != nil || l.Alias != "nowhere-at-all" || l.Repo != "" {
		t.Errorf("unresolvable slug keeps the directory alias: %v %v %+v", d, err, l)
	}
}

// TestTeamIDPrefix pins the team id space: #t12 ... ^t12 round-trips, a
// mixed prefix is refused, and a personal-form row cannot enter a team ledger.
func TestTeamIDPrefix(t *testing.T) {
	line := "- #t12 2026-09-20 project/api supersedes #t3: Team fact. -> [[LEDGER#^t3]] ^t12"
	r, d := ParseRow(line)
	if d != nil || !r.Team || r.ID != 12 || r.Supersedes() != 3 || r.Format() != line || r.Cite() != "#t12" {
		t.Fatalf("team row: %v %+v %q", d, r, r.Format())
	}
	for _, bad := range []string{"- #t12 2026-09-20 project/api X. ^m12", "- #12 2026-09-20 project/api X. ^t12"} {
		if _, d := ParseRow(bad); d == nil || d.Code != DiagRowSyntax {
			t.Errorf("%q must be refused: %v", bad, d)
		}
	}
	l := newHome(t, KindTeam)
	row, d, err := l.Append(Row{Date: "2026-09-20", Type: "project", Topic: "api", Sentence: "Fact."})
	if err != nil || d != nil || !row.Team || row.Format() != "- #t1 2026-09-20 project/api Fact. ^t1" {
		t.Fatalf("Append into team: %v %v %q", d, err, row.Format())
	}
	if err := appendLine(l.Path, "- #2 2026-09-20 project/api Bare. ^m2"); err != nil {
		t.Fatal(err)
	}
	if _, d, _ := Load(l.Path); d == nil || d.Code != DiagLedgerInvalid {
		t.Errorf("bare id in a team ledger must fail Load: %v", d)
	}
	for _, c := range []struct {
		in   string
		team bool
	}{{"t12", true}, {"#t12", true}, {"^t12", true}, {"[[LEDGER#^t12]]", true}, {"12", false}, {"[[x/LEDGER#^m12]]", false}} {
		if ref, d := ParseRef(c.in); d != nil || ref.Team != c.team || ref.ID != 12 {
			t.Errorf("ParseRef(%q) = %+v %v", c.in, ref, d)
		}
	}
}

// TestWorktreeSessionSplitsPersonalAndTeam: from a linked worktree the
// personal home is the MAIN checkout's slug, the team ledger is the
// worktree's own, bare ids credit personal only, t-ids team only, and the
// vault/listing name the main checkout's team path.
func TestWorktreeSessionSplitsPersonalAndTeam(t *testing.T) {
	root := linkedWorktreeRepo(t)
	wt := filepath.Join(root, ".worktrees", "memo-ledger")
	env := Env{UserHome: t.TempDir(), Cwd: wt}
	personal, team := env.SessionHomes()
	if personal.Path != filepath.Join(env.UserHome, ".claude", "projects", ProjectSlug(root), "memory") {
		t.Errorf("personal home must use the MAIN checkout slug: %s", personal.Path)
	}
	if team.Path != filepath.Join(wt, ".claude", "memory") {
		t.Errorf("team ledger must be the worktree's own: %s", team.Path)
	}
	pl, d, err := env.Open(personal, true)
	if err != nil || d != nil {
		t.Fatal(d, err)
	}
	tl, d, err := env.Open(team, true)
	if err != nil || d != nil {
		t.Fatal(d, err)
	}
	if err := os.MkdirAll(filepath.Join(personal.Path, NotesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(personal.Path, NotesDir, "hang.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"- #83 2026-09-20 feedback/shell Personal. -> [[notes/hang]] ^m83", "- #106 2026-09-20 feedback/db Personal too. ^m106"} {
		if err := appendLine(pl.Path, r); err != nil {
			t.Fatal(err)
		}
	}
	for id := 1; id <= 83; id++ {
		if err := appendLine(tl.Path, Row{ID: id, Team: true, Date: "2026-09-20", Type: "project", Topic: "t", Sentence: "Team."}.Format()); err != nil {
			t.Fatal(err)
		}
	}
	transcript := filepath.Join(t.TempDir(), "s.jsonl")
	body := `{"type":"assistant","message":{"content":[{"type":"text","text":"Per #83, [[LEDGER#^m106]] and [[fixit-team/LEDGER#^t3]]."},{"type":"tool_use","name":"Read","input":{"file_path":"` + filepath.Join(personal.Path, NotesDir, "hang.md") + `"}}]}}` + "\n"
	if err := os.WriteFile(transcript, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := env.RunHook(HookPayload{HookEventName: "Stop", Cwd: wt, SessionID: "s", TranscriptPath: transcript}, time.Now())
	if err != nil || res.Recorded != 4 {
		t.Fatalf("RunHook: %+v %v", res, err)
	}
	pe, _, _ := LoadEvents(personal.Path)
	te, _, _ := LoadEvents(team.Path)
	if len(pe) != 3 || len(te) != 1 || te[0].Row != 3 {
		t.Errorf("personal events %+v, team events %+v", pe, te)
	}
	homes, d, err := env.Discover()
	if d != nil || err != nil {
		t.Fatal(d, err)
	}
	report, err := Vault(filepath.Join(t.TempDir(), "Memory"), homes)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range report.Entries {
		if e.Alias == "fixit-team" {
			found = true
			if e.Target != filepath.Join(root, ".claude", "memory") {
				t.Errorf("fixit-team must link the MAIN checkout, got %s", e.Target)
			}
		}
	}
	if !found {
		t.Errorf("vault lacks fixit-team: %+v", report.Entries)
	}
}

// linkedWorktreeRepo builds a git repo "FixIt" with a linked worktree at
// .worktrees/memo-ledger and returns the symlink-resolved root (git reports
// /private/var for macOS temp dirs).
func linkedWorktreeRepo(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "FixIt")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@x", "commit", "-q", "--allow-empty", "-m", "init"}, {"worktree", "add", "-q", filepath.Join(root, ".worktrees", "memo-ledger")}} {
		if out, err := git(root, args...); err != nil {
			t.Fatal(out)
		}
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}
