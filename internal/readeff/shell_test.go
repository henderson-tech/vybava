package readeff

import (
	"slices"
	"testing"
)

// A command is classified by what its segments do, and a read keeps the
// range it printed, resolved against where it ran.
func TestShellCall(t *testing.T) {
	cases := []struct {
		name   string
		cmd    string
		class  Class
		spans  []Span
		edited []string
	}{
		{"sed range", `sed -n '40,90p' a.go`, ClassRead, []Span{{Path: "/r/a.go", Start: 40, N: 51}}, nil},
		{"head count", "head -n 30 a.go", ClassRead, []Span{{Path: "/r/a.go", Start: 1, N: 30}}, nil},
		{"cat is whole", "cat a.go b.go", ClassRead, []Span{{Path: "/r/a.go", Start: 1, Whole: true}, {Path: "/r/b.go", Start: 1, Whole: true}}, nil},
		{"assigned dir and cd", `W=/w/wt && cd $W && sed -n '1,5p' x.go`, ClassRead, []Span{{Path: "/w/wt/x.go", Start: 1, N: 5}}, nil},
		{"subshell cd", "(cd /w && cat x.go)", ClassRead, []Span{{Path: "/w/x.go", Start: 1, Whole: true}}, nil},
		{"a read filtered downstream loses its range", "cat a.go | head -5", ClassRead, []Span{{Path: "/r/a.go"}}, nil},
		{"a read feeding a search is a search", "cat big.go | grep needle", ClassSearch, nil, nil},
		{"a read into a scratch file shows nothing", "cat a.go > /tmp/out", ClassOther, nil, nil},
		{"git show path", "git show HEAD:a.go", ClassRead, []Span{{Path: "/r/a.go"}}, nil},
		{"grep is a search", "grep -rn foo internal | head -20", ClassSearch, nil, nil},
		{"stderr to /dev/null is not a write", "ls -la 2>/dev/null", ClassSearch, nil, nil},
		{"quoted > is not a redirect", `echo "a > b"`, ClassOther, nil, nil},
		{"heredoc overwrite is an edit", "cat > out.go <<'EOF'\npackage x\nEOF", ClassEdit, nil, []string{"/r/out.go"}},
		{"sed -i is an edit", `sed -i '' 's/a/b/' a.go`, ClassEdit, nil, []string{"/r/a.go"}},
		{"scratch output is not an edit", "jq . a.json > /tmp/x.json", ClassOther, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := shellCall(tc.cmd, "/r")
			if c.Class != tc.class {
				t.Errorf("class = %s, want %s", c.Class, tc.class)
			}
			if !slices.Equal(c.Spans, tc.spans) {
				t.Errorf("spans = %+v, want %+v", c.Spans, tc.spans)
			}
			if !slices.Equal(c.Edited, tc.edited) {
				t.Errorf("edited = %q, want %q", c.Edited, tc.edited)
			}
		})
	}
}

// Several outputs in one result are all recorded, and none gets the lines;
// a write through the shell counts what it changed where the command says.
func TestShellCallMixedAndWrites(t *testing.T) {
	mixed := shellCall("cat a.go; rg foo", "/r")
	if !mixed.Read || !mixed.Search || mixed.Visible != 2 {
		t.Errorf("cat; rg = read %v search %v visible %d, want both and 2", mixed.Read, mixed.Search, mixed.Visible)
	}
	if two := shellCall("cat a.go; echo ---; cat b.go", "/r"); two.outputs != 1<<0 {
		t.Errorf("cat; echo; cat outputs = %b, want reads only", two.outputs)
	}
	readThenEdit := shellCall("cat a.go; echo x > a.go", "/r")
	if !readThenEdit.Read || !readThenEdit.Edit || readThenEdit.Visible != 1 {
		t.Errorf("cat; echo > = %+v, want a visible read and an edit", readThenEdit)
	}
	patch := shellCall("apply_patch <<'EOF'\n*** Begin Patch\n*** Update File: a.go\n-x\n+y\n+z\n*** End Patch\nEOF", "/r")
	if !patch.Edit || patch.Changed != 3 || !slices.Equal(patch.Edited, []string{"/r/a.go"}) {
		t.Errorf("apply_patch heredoc = edited %q changed %d, want /r/a.go and 3", patch.Edited, patch.Changed)
	}
	if inert := shellCall(`rg -n '*** Begin Patch' docs`, "/r"); inert.Edit || inert.Changed != 0 {
		t.Errorf("searching for the patch marker = edit %v changed %d, want neither", inert.Edit, inert.Changed)
	}
	filtered := shellCall(`bash -lc 'cat a.go' | head -5`, "/r")
	if !filtered.Read || len(filtered.Spans) != 1 || filtered.Spans[0] != (Span{Path: "/r/a.go"}) {
		t.Errorf("runner piped into head = %+v, want a read of a.go with an unknown range", filtered.Spans)
	}
	heredoc := shellCall("cat > new.go <<'EOF'\npackage x\n\nfunc F() {}\nEOF", "/r")
	if !heredoc.Edit || heredoc.Changed != 3 {
		t.Errorf("heredoc write changed %d, want its 3 body lines", heredoc.Changed)
	}
}
