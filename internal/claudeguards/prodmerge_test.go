package claudeguards

import (
	"errors"
	"strings"
	"testing"
)

// stubProdMerge wires the rule's lookups: repoDir declares PROD_BRANCHES,
// its origin is Reservine/ReservineBack, bases maps a selector (or a pulls
// API path) to its PR base, current is the checked-out branch.
func stubProdMerge(t *testing.T, repoDir string, bases map[string]string, current string) *[]string {
	t.Helper()
	calls := &[]string{}
	saved := []any{prodBranchesFor, prBaseFor, apiPRBaseFor, originSlugFor, currentBranchOf}
	t.Cleanup(func() {
		prodBranchesFor = saved[0].(func(string) ([]string, error))
		prBaseFor = saved[1].(func(string, string, string) (string, error))
		apiPRBaseFor = saved[2].(func(string, string) (string, error))
		originSlugFor = saved[3].(func(string) string)
		currentBranchOf = saved[4].(func(string) string)
	})
	prodBranchesFor = func(dir string) ([]string, error) {
		if strings.HasPrefix(dir, repoDir) {
			return []string{"canary", "release", "master"}, nil
		}
		return nil, nil
	}
	lookup := func(key string) (string, error) {
		*calls = append(*calls, key)
		if b, ok := bases[key]; ok {
			return b, nil
		}
		return "", errors.New("HTTP 502")
	}
	prBaseFor = func(_, repo, sel string) (string, error) { return lookup(repo + "#" + sel) }
	apiPRBaseFor = func(_, path string) (string, error) { return lookup(path) }
	originSlugFor = func(dir string) string {
		if strings.HasPrefix(dir, repoDir) {
			return "Reservine/ReservineBack"
		}
		return "henderson-tech/vybava"
	}
	currentBranchOf = func(string) string { return current }
	return calls
}

func bashInput(cwd, cmd string) *HookInput {
	in := &HookInput{CWD: cwd}
	in.ToolInput.Command = cmd
	return in
}

func TestProdMerge(t *testing.T) {
	const be = "/Users/u/Work/Projects/Reservine/ReservineBack"
	const wt = be + "/.worktrees/promote-canary"
	const other = "/Users/u/Work/Projects/FixIt-Technologies/vybava"
	bases := map[string]string{
		"#":                          "canary", // the current branch's PR
		"#12":                        "canary",
		"#13":                        "devlp",
		"#14":                        "master",
		"Reservine/ReservineBack#12": "canary",
		"Reservine/ReservineBack#13": "devlp",
		"Reservine/Reservine#7":      "release",
		"henderson-tech/FanDeck#3":   "master",
		"Reservine/Reservine#https://github.com/Reservine/Reservine/pull/7": "release",
		"repos/Reservine/ReservineBack/pulls/12":                            "canary",
		"repos/{owner}/{repo}/pulls/13":                                     "devlp",
	}
	cases := []struct {
		name, cmd, cwd, current string
		want                    bool
	}{
		// gh pr merge (prm's terminus runs exactly this)
		{"merge into canary", "gh pr merge 12 --merge --delete-branch", wt, "", true},
		{"auto-merge into canary", "gh pr merge 12 --auto --merge", wt, "", true},
		{"admin merge into master", "gh pr merge 14 --squash --admin", be, "", true},
		{"current branch's PR into canary", "gh pr merge --merge", wt, "", true},
		{"merge into devlp passes", "gh pr merge 13 --squash --delete-branch", wt, "", false},
		{"after cd into the repo", "cd " + wt + " && gh pr merge 12 --merge", other, "", true},
		{"--repo naming another repo gets the default set", "gh pr merge 7 --repo Reservine/Reservine --merge", other, "", true},
		{"--repo naming another repo, not a prod base", "gh pr merge 13 -R Reservine/ReservineBack --squash", other, "", false},
		{"PR URL names its repo", "gh pr merge https://github.com/Reservine/Reservine/pull/7 --merge", other, "", true},
		{"repo without PROD_BRANCHES: master is its trunk", "gh pr merge 3 --squash", other, "", false},
		{"base unreadable fails closed", "gh pr merge 99 --merge", wt, "", true},
		{"escape hatch", "CLAUDE_ALLOW_PROD_MERGE=1 gh pr merge 12 --merge", wt, "", false},
		{"escape named in a message only", `echo "CLAUDE_ALLOW_PROD_MERGE=1" && gh pr merge 12 --merge`, wt, "", true},
		{"merge mentioned in a commit message", `git commit -m "gh pr merge 12 into canary"`, wt, "", false},
		{"gh pr view is not a merge", "gh pr view 12 --json baseRefName", wt, "", false},

		// gh api
		{"REST merge into canary", "gh api -X PUT repos/Reservine/ReservineBack/pulls/12/merge -f merge_method=merge", other, "", true},
		{"REST merge into devlp", "gh api --method PUT repos/{owner}/{repo}/pulls/13/merge", wt, "", false},
		{"REST merge status read", "gh api repos/Reservine/ReservineBack/pulls/12/merge", wt, "", false},
		{"GraphQL merge mutation", `gh api graphql -f query='mutation { mergePullRequest(input: {pullRequestId: "PR_x"}) { clientMutationId } }'`, wt, "", true},
		{"GraphQL auto-merge mutation", `gh api graphql -f query='mutation { enablePullRequestAutoMerge(input: {pullRequestId: "PR_x"}) { clientMutationId } }'`, wt, "", true},
		{"GraphQL query passes", `gh api graphql -f query='{ viewer { login } }'`, wt, "", false},
		{"ref write on canary", "gh api -X PATCH repos/{owner}/{repo}/git/refs/heads/canary -f sha=abc", wt, "", true},

		// git push
		{"push refspec to canary", "git push origin promote/canary-20260925:canary", wt, "", true},
		{"push HEAD to release", "git push origin HEAD:refs/heads/release", wt, "", true},
		{"force push to master", "git push -f origin +master", be, "", true},
		{"delete canary", "git push origin --delete canary", wt, "", true},
		{"bare push from canary", "git push", wt, "canary", true},
		{"push HEAD from master", "git -C " + be + " push origin HEAD", other, "master", true},
		{"push --all", "git push --all origin", wt, "promote/x", true},
		{"push the feature branch", "git push -u origin promote/canary-20260925", wt, "promote/canary-20260925", false},
		{"push option value is not the remote", "git push -o ci.skip origin feat/x", wt, "feat/x", false},
		{"push in a repo without PROD_BRANCHES", "git push origin master", other, "master", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stubProdMerge(t, be, bases, c.current)
			d := guardProdMerge(bashInput(c.cwd, c.cmd))
			if got := d != nil; got != c.want {
				t.Fatalf("blocked=%v, want %v (%v)", got, c.want, d)
			}
			if d != nil && (d.Rule != "prod-merge:merge" || !strings.Contains(d.EscapeHatch, "CLAUDE_ALLOW_PROD_MERGE=1")) {
				t.Fatalf("denial %q / %q", d.Rule, d.EscapeHatch)
			}
		})
	}
}

// A repo that declares nothing never pays for a gh round trip.
func TestProdMergeNoLookupWithoutPolicy(t *testing.T) {
	calls := stubProdMerge(t, "/nowhere", map[string]string{}, "")
	if d := guardProdMerge(bashInput("/Users/u/app", "gh pr merge 5 --squash")); d != nil {
		t.Fatalf("blocked: %v", d)
	}
	if len(*calls) != 0 {
		t.Fatalf("looked up %v", *calls)
	}
}
