package gitkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// list-prs — the current repo's open PRs authored by the viewer that need
// action (CHANGES_REQUESTED or an unresolved thread): [{pr, headRef, reason}].

type prNode struct {
	Number         int     `json:"number"`
	HeadRefName    string  `json:"headRefName"`
	ReviewDecision *string `json:"reviewDecision"`
	Author         *struct {
		Login string `json:"login"`
	} `json:"author"`
	ReviewThreads *struct {
		Nodes []struct {
			IsResolved bool `json:"isResolved"`
		} `json:"nodes"`
	} `json:"reviewThreads"`
}

func changesRequested(reviewDecision *string) bool {
	return reviewDecision != nil && strings.ToUpper(*reviewDecision) == "CHANGES_REQUESTED"
}

func reasonFor(reviewDecision *string, unresolved int) string {
	parts := []string{}
	if changesRequested(reviewDecision) {
		parts = append(parts, "CHANGES_REQUESTED")
	}
	if unresolved > 0 {
		plural := "s"
		if unresolved == 1 {
			plural = ""
		}
		parts = append(parts, fmt.Sprintf("%d unresolved thread%s", unresolved, plural))
	}
	return strings.Join(parts, " + ")
}

// SelectedPR is one list-prs row, keys in wire order.
type SelectedPR struct {
	PR      int    `json:"pr"`
	HeadRef string `json:"headRef"`
	Reason  string `json:"reason"`
}

func selectPRs(nodes []prNode, viewerLogin string) []SelectedPR {
	out := []SelectedPR{}
	for _, n := range nodes {
		if n.Author == nil || n.Author.Login != viewerLogin {
			continue
		}
		unresolved := 0
		if n.ReviewThreads != nil {
			for _, t := range n.ReviewThreads.Nodes {
				if !t.IsResolved {
					unresolved++
				}
			}
		}
		if !changesRequested(n.ReviewDecision) && unresolved == 0 {
			continue
		}
		out = append(out, SelectedPR{PR: n.Number, HeadRef: n.HeadRefName, Reason: reasonFor(n.ReviewDecision, unresolved)})
	}
	return out
}

const listPRsQuery = `
query($owner:String!, $repo:String!) {
  viewer { login }
  repository(owner:$owner, name:$repo) {
    pullRequests(states: OPEN, first: 50, orderBy: {field: UPDATED_AT, direction: DESC}) {
      nodes {
        number headRefName reviewDecision
        author { login }
        reviewThreads(first: 100) { nodes { isResolved } }
      }
    }
  }
}`

// ghRepo resolves the anchored checkout's owner/name via `gh repo view`.
func ghRepo(opts execOpts) (owner, name string, err error) {
	out, err := execFile(opts, "gh", "repo", "view", "--json", "nameWithOwner")
	if err != nil {
		return "", "", err
	}
	var repo struct {
		NameWithOwner string `json:"nameWithOwner"`
	}
	if err := json.Unmarshal([]byte(out), &repo); err != nil {
		return "", "", err
	}
	owner, name, _ = strings.Cut(repo.NameWithOwner, "/")
	return owner, name, nil
}

func runListPRs(args []string, stdout, stderr io.Writer) int {
	root, err := repoRoot(args)
	if err != nil {
		return fail(stderr, err)
	}
	opts := execOpts{dir: root, echo: stderr, timeout: 60 * time.Second, maxBuffer: 32 << 20}
	owner, name, err := ghRepo(opts)
	if err != nil {
		return fail(stderr, err)
	}
	out, err := execFile(opts, "gh", "api", "graphql", "-f", "query="+listPRsQuery, "-f", "owner="+owner, "-f", "repo="+name)
	if err != nil {
		return fail(stderr, err)
	}
	var resp struct {
		Data *struct {
			Viewer struct {
				Login string `json:"login"`
			} `json:"viewer"`
			Repository *struct {
				PullRequests struct {
					Nodes []prNode `json:"nodes"`
				} `json:"pullRequests"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return fail(stderr, err)
	}
	if resp.Data == nil || resp.Data.Repository == nil {
		return fail(stderr, errors.New("graphql response carries no repository data"))
	}
	if err := writeJSON(stdout, selectPRs(resp.Data.Repository.PullRequests.Nodes, resp.Data.Viewer.Login)); err != nil {
		return fail(stderr, err)
	}
	return 0
}
