package gitkit

import (
	"slices"
	"testing"
)

// Skills name these verbs; dropping one breaks a skill silently.
func TestScriptsListEveryVerbSkillsCall(t *testing.T) {
	want := []string{
		"before-review", "classify-paths", "github-io", "list-prs", "merge-precheck", "pr-events",
		"pr-extensions", "resolve-fetch", "sync-context", "tdd-classify", "worktree",
	}
	if got := Scripts(); !slices.Equal(got, want) {
		t.Fatalf("Scripts() = %v, want %v", got, want)
	}
	for _, name := range want {
		if _, ok := Native(name); !ok {
			t.Errorf("verb %s has no implementation", name)
		}
	}
}
