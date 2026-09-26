package gitkit

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/skipci"
)

// admin-labels — `prm --admin`'s deterministic half: put the org skip labels
// on a PR and cancel the CI that already started for its head.
//
//	vybava gitkit admin-labels <pr> --repo <ABS repo path>
//
// The labels are skipci's (`skip-ci`, `eve-ignore`); both are created in the
// repo first (`gh label create --force`, idempotent) so a repo repolicy has
// not swept yet still works. Then, because a label added after the push
// cannot reach the runs the push already queued (the workflow saw the event
// payload without it), every queued or running workflow run on the head SHA
// is cancelled — the next push, if any, skips on the guard by itself. Eve is
// a webhook app that reads the label at review time; nothing to cancel there.
//
// stdout is one JSON object: pr, url, headSha, labels (now on the PR),
// labelsAdded, runsCancelled [{id, workflow}], runsLeft [{id, workflow,
// status}] (runs gh could not cancel — reported, never fatal).
const adminLabelsUsage = "usage: vybava gitkit admin-labels <pr> --repo <path>"

// adminLabelsArgs: one PR and the repo anchor.
var adminLabelsArgs = verbArgs{values: []string{"repo"}, positionals: 1, usage: adminLabelsUsage}

// AdminLabels is the verb's stdout, keys in wire order.
type AdminLabels struct {
	PR            int        `json:"pr"`
	URL           string     `json:"url"`
	HeadSha       string     `json:"headSha"`
	Labels        []string   `json:"labels"`
	LabelsAdded   []string   `json:"labelsAdded"`
	RunsCancelled []adminRun `json:"runsCancelled"`
	RunsLeft      []adminRun `json:"runsLeft"`
}

type adminRun struct {
	ID       int64  `json:"id"`
	Workflow string `json:"workflow"`
	Status   string `json:"status,omitempty"`
}

// ghRun is one `gh run list --json databaseId,status,workflowName` row.
type ghRun struct {
	DatabaseID   int64  `json:"databaseId"`
	Status       string `json:"status"`
	WorkflowName string `json:"workflowName"`
}

// planAdminLabels decides what to change: the labels the PR still lacks and
// the runs still alive. Pure, so the argv the verb issues is testable.
func planAdminLabels(present []string, runs []ghRun) (toAdd []string, toCancel []ghRun) {
	for _, l := range skipci.Labels() {
		if !slices.Contains(present, l.Name) {
			toAdd = append(toAdd, l.Name)
		}
	}
	for _, r := range runs {
		switch strings.ToLower(r.Status) {
		case "queued", "in_progress", "waiting", "pending", "requested":
			toCancel = append(toCancel, r)
		}
	}
	return toAdd, toCancel
}

func runAdminLabels(args []string, stdout, stderr io.Writer) int {
	// An absent, bare or empty --repo must never fall through to the cwd or
	// GIT_SKILL_REPO: this verb labels a PR and cancels its runs. parse
	// refuses bare and empty, and a second PR; absent is refused here.
	flags, pos, err := adminLabelsArgs.parse("admin-labels", args)
	if err != nil {
		return fail(stderr, err)
	}
	if _, ok := flags["repo"]; !ok {
		return fail(stderr, fmt.Errorf("admin-labels: --repo <path> is required\n%s", adminLabelsUsage))
	}
	prArg := ""
	if len(pos) == 1 {
		prArg = pos[0]
	}
	if _, ok := positiveInt(prArg); !ok {
		return fail(stderr, fmt.Errorf("a PR number is required\n%s", adminLabelsUsage))
	}
	root, err := repoRoot(repoAnchor(flags))
	if err != nil {
		return fail(stderr, err)
	}
	opts := execOpts{dir: root, echo: stderr, timeout: 60 * time.Second, maxBuffer: 32 << 20}
	gh := func(a ...string) (string, error) { return execFile(opts, "gh", a...) }

	repoOut, err := gh("repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner")
	if err != nil {
		return fail(stderr, err)
	}
	slug := strings.TrimSpace(repoOut)

	prOut, err := gh("pr", "view", prArg, "--repo", slug, "--json", "number,url,headRefOid,labels")
	if err != nil {
		return fail(stderr, err)
	}
	var pr struct {
		Number     int    `json:"number"`
		URL        string `json:"url"`
		HeadRefOid string `json:"headRefOid"`
		Labels     []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal([]byte(prOut), &pr); err != nil {
		return fail(stderr, err)
	}
	present := make([]string, 0, len(pr.Labels))
	for _, l := range pr.Labels {
		present = append(present, l.Name)
	}

	// Newest first; a head SHA with more than 200 runs is not a PR anyone is
	// reviewing, and gh has no live-only filter that covers every status.
	runsOut, err := gh("run", "list", "--repo", slug, "--commit", pr.HeadRefOid, "--limit", "200", "--json", "databaseId,status,workflowName")
	if err != nil {
		return fail(stderr, err)
	}
	var runs []ghRun
	if err := json.Unmarshal([]byte(runsOut), &runs); err != nil {
		return fail(stderr, err)
	}
	repoLabelsOut, err := gh("label", "list", "--repo", slug, "--limit", "1000", "--json", "name")
	if err != nil {
		return fail(stderr, err)
	}
	var repoLabelRows []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(repoLabelsOut), &repoLabelRows); err != nil {
		return fail(stderr, err)
	}
	repoLabels := make([]string, 0, len(repoLabelRows))
	for _, r := range repoLabelRows {
		repoLabels = append(repoLabels, r.Name)
	}

	toAdd, toCancel := planAdminLabels(present, runs)
	// Create only what the REPO lacks — a label a human recoloured or
	// re-described is theirs; the PR-side gap alone never rewrites it.
	for _, l := range skipci.Labels() {
		if slices.Contains(toAdd, l.Name) && !slices.Contains(repoLabels, l.Name) {
			// "already exists" (a label past the list, or a race) is not a
			// failure: the label is there, which is all the next step needs.
			if _, err := gh(skipci.LabelArgs(l, slug)...); err != nil && !strings.Contains(err.Error(), "already exists") {
				return fail(stderr, err)
			}
		}
	}
	if len(toAdd) > 0 {
		if _, err := gh("pr", "edit", strconv.Itoa(pr.Number), "--repo", slug, "--add-label", strings.Join(toAdd, ",")); err != nil {
			return fail(stderr, err)
		}
	}
	out := AdminLabels{
		PR: pr.Number, URL: pr.URL, HeadSha: pr.HeadRefOid,
		Labels: append(present, toAdd...), LabelsAdded: toAdd,
		RunsCancelled: []adminRun{}, RunsLeft: []adminRun{},
	}
	if out.LabelsAdded == nil {
		out.LabelsAdded = []string{}
	}
	for _, r := range toCancel {
		run := adminRun{ID: r.DatabaseID, Workflow: r.WorkflowName}
		if _, err := gh("run", "cancel", strconv.FormatInt(r.DatabaseID, 10), "--repo", slug); err != nil {
			run.Status = r.Status
			fmt.Fprintf(stderr, "note: run %d (%s) not cancelled: %v\n", r.DatabaseID, r.WorkflowName, err)
			out.RunsLeft = append(out.RunsLeft, run)
			continue
		}
		out.RunsCancelled = append(out.RunsCancelled, run)
	}
	if err := writeJSON(stdout, out); err != nil {
		return fail(stderr, err)
	}
	return 0
}
