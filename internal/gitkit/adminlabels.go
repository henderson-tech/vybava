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
	prArg := ""
	for i := 0; i < len(args); i++ {
		if args[i] == "--repo" {
			i++
			continue
		}
		if strings.HasPrefix(args[i], "--") {
			return fail(stderr, fmt.Errorf("unknown argument %s\n%s", args[i], adminLabelsUsage))
		}
		if prArg != "" {
			return fail(stderr, fmt.Errorf("one PR at a time\n%s", adminLabelsUsage))
		}
		prArg = args[i]
	}
	if _, ok := positiveInt(prArg); !ok {
		return fail(stderr, fmt.Errorf("a PR number is required\n%s", adminLabelsUsage))
	}
	root, err := repoRoot(args)
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

	runsOut, err := gh("run", "list", "--repo", slug, "--commit", pr.HeadRefOid, "--limit", "50", "--json", "databaseId,status,workflowName")
	if err != nil {
		return fail(stderr, err)
	}
	var runs []ghRun
	if err := json.Unmarshal([]byte(runsOut), &runs); err != nil {
		return fail(stderr, err)
	}

	toAdd, toCancel := planAdminLabels(present, runs)
	for _, l := range skipci.Labels() {
		if slices.Contains(toAdd, l.Name) {
			if _, err := gh(skipci.LabelArgs(l, slug)...); err != nil {
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
