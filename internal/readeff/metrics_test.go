package readeff

import "testing"

func read(path string, start, n, lines int) Call {
	return Call{Tool: "Read", Class: ClassRead, Read: true, Visible: 1, Lines: lines, Spans: []Span{{Path: path, Start: start, N: n}}}
}

// A re-read is an overlapping range of a file the model still holds: an
// edit of the file or a compaction releases it.
func TestRereads(t *testing.T) {
	s := Session{ID: "s", Calls: []Call{
		read("/r/a.go", 1, 50, 50),
		read("/r/a.go", 40, 20, 20), // overlaps 40-50: re-read
		read("/r/a.go", 100, 10, 10),
		{Tool: "Edit", Class: ClassEdit, Edit: true, Edited: []string{"/r/a.go"}, Changed: 4},
		read("/r/a.go", 1, 50, 50), // after the edit: verification, not a re-read
		{Tool: compactTool},
		read("/r/a.go", 1, 50, 50), // after compaction: not held any more
		read("/r/a.go", 45, 5, 5),  // re-read
	}}
	got := analyze(s, DefaultConfig, map[string]*fileAcc{}, nil)
	if got.Rereads != 2 || got.RereadLines != 25 || got.RangedReads != 6 {
		t.Errorf("rereads = %d (%d lines) of %d, want 2 (25 lines) of 6", got.Rereads, got.RereadLines, got.RangedReads)
	}
	if got.Changed != 4 || got.LinesPerChange() != 46.3 || got.Compactions != 1 {
		t.Errorf("changed %d, lines/change %.1f, compactions %d", got.Changed, got.LinesPerChange(), got.Compactions)
	}
}

// A search counts as a hit when a file it printed is read within the
// window; an empty search re-run successfully counts as retried.
func TestSearchOutcomes(t *testing.T) {
	grep := Call{Tool: "Bash", Class: ClassSearch, Search: true, Visible: 1, Query: "grep -rn x .", Found: []string{"/r/a.go"}}
	empty := Call{Tool: "Bash", Class: ClassSearch, Search: true, Visible: 1, Query: "grep -rn y .", Empty: true}
	s := Session{ID: "s", Calls: []Call{
		grep, {Tool: "Bash", Class: ClassOther}, read("/r/a.go", 1, 10, 10), // hit
		grep, {Class: ClassOther}, {Class: ClassOther}, {Class: ClassOther}, read("/r/a.go", 1, 10, 10), // outside the window: miss
		empty, {Tool: "Bash", Class: ClassSearch, Search: true, Visible: 1, Query: "grep -rn y ."}, // retried
	}}
	got := analyze(s, DefaultConfig, map[string]*fileAcc{}, nil)
	if got.SearchesFound != 2 || got.SearchHits != 1 || got.EmptySearches != 1 || got.RetriedEmpty != 1 {
		t.Errorf("found %d hits %d empty %d retried %d, want 2 1 1 1", got.SearchesFound, got.SearchHits, got.EmptySearches, got.RetriedEmpty)
	}
}

// A whole read of a file above the threshold counts as big; files rank
// with their worktree copies folded into the repository's file.
func TestBigReadsAndFiles(t *testing.T) {
	s := Session{ID: "s", Repo: "/r", Calls: []Call{
		{Tool: "Read", Class: ClassRead, Read: true, Visible: 1, Lines: 900, Spans: []Span{{Path: "/r/.worktrees/x/big.go", Start: 1, N: 900, Total: 900, Whole: true}}},
		{Tool: "Read", Class: ClassRead, Read: true, Visible: 1, Lines: 30, Spans: []Span{{Path: "/r/small.go", Start: 1, N: 30, Total: 30, Whole: true}}},
		read("/r/big.go", 1, 40, 40),
	}}
	files := map[string]*fileAcc{}
	got := analyze(s, DefaultConfig, files, nil)
	if got.BigWhole != 1 || got.BigWholeLines != 900 {
		t.Errorf("big whole = %d (%d lines), want 1 (900)", got.BigWhole, got.BigWholeLines)
	}
	top := topFiles(files, 1)
	if len(top) != 1 || top[0].Path != "/r/big.go" || top[0].Lines != 940 || top[0].Reads != 2 {
		t.Errorf("top file = %+v, want /r/big.go with 940 lines over 2 reads", top)
	}
}
