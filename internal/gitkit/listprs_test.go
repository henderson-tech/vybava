package gitkit

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestReasonFor(t *testing.T) {
	cr, approved := ptr("CHANGES_REQUESTED"), ptr("APPROVED")
	for _, tc := range []struct {
		decision   *string
		unresolved int
		want       string
	}{
		{cr, 0, "CHANGES_REQUESTED"}, {approved, 1, "1 unresolved thread"}, {cr, 2, "CHANGES_REQUESTED + 2 unresolved threads"},
	} {
		if got := reasonFor(tc.decision, tc.unresolved); got != tc.want {
			t.Errorf("reasonFor = %q, want %q", got, tc.want)
		}
	}
}

// Only viewer-authored PRs with CHANGES_REQUESTED or an unresolved thread qualify.
func TestSelectPRs(t *testing.T) {
	var nodes []prNode
	if err := json.Unmarshal([]byte(`[
		{"number":1,"headRefName":"feat-a","reviewDecision":"CHANGES_REQUESTED","author":{"login":"me"},"reviewThreads":{"nodes":[]}},
		{"number":2,"headRefName":"feat-b","reviewDecision":"APPROVED","author":{"login":"me"},"reviewThreads":{"nodes":[{"isResolved":false},{"isResolved":true}]}},
		{"number":3,"headRefName":"feat-c","reviewDecision":"APPROVED","author":{"login":"me"},"reviewThreads":{"nodes":[{"isResolved":true}]}},
		{"number":4,"headRefName":"feat-d","reviewDecision":"CHANGES_REQUESTED","author":{"login":"someone-else"},"reviewThreads":{"nodes":[]}},
		{"number":5,"headRefName":"feat-e","reviewDecision":null,"author":null,"reviewThreads":{"nodes":[{"isResolved":false}]}},
		{"number":6,"headRefName":"feat-f","reviewDecision":null,"author":{"login":"me"},"reviewThreads":{"nodes":[]}}
	]`), &nodes); err != nil {
		t.Fatal(err)
	}
	want := []SelectedPR{{1, "feat-a", "CHANGES_REQUESTED"}, {2, "feat-b", "1 unresolved thread"}}
	if got := selectPRs(nodes, "me"); !slices.Equal(got, want) {
		t.Fatalf("selectPRs = %+v", got)
	}
}

// list-prs takes the repo anchor only; anything else is refused before a gh call.
func TestListPRsArgs(t *testing.T) {
	for _, argv := range [][]string{{"--repo", "/abs"}, {"--repo=/abs"}, nil} {
		if _, _, err := listPRsArgs.parse("list-prs", argv); err != nil {
			t.Errorf("parse(%q): %v", argv, err)
		}
	}
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"all", "--repo", "/abs"}, `unexpected argument "all"`},
		{[]string{"--author", "me"}, "unknown argument --author"},
	} {
		var stdout, stderr strings.Builder
		if code := runListPRs(tc.argv, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), tc.want) ||
			!strings.Contains(stderr.String(), listPRsArgs.usage) || stdout.Len() != 0 {
			t.Errorf("list-prs %q: exit %d, stderr %q, want %q + usage", tc.argv, code, stderr.String(), tc.want)
		}
	}
}
