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
		{"piped read has no known range", "cat a.go | grep foo", ClassRead, []Span{{Path: "/r/a.go"}}, nil},
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
