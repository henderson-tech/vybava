package gitkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// pr-census — the facts /check-prs judges from: one row per selected PR with
// its link, size and four-bucket split (prcensus_split.go), drift and
// merge-tree result against its base, CI, reviews, local worktree state and
// live activity (prcensus_activity.go). It writes nothing but `git fetch`'s
// remote-tracking base refs and the PR heads' objects.

var prCensusArgs = verbArgs{
	values:      []string{"repo", "base", "since", "author", "state"},
	bools:       []string{"no-fetch"},
	positionals: -1,
	usage: "usage: vybava gitkit pr-census [--repo <path>] [--base <branch>] [--since <N>d|<YYYY-MM-DD>] " +
		"[--author <login>|@me] [--state open|merged|closed|all] [--no-fetch] [<pr>...]",
}

// censusNow is the clock; tests pin it.
var censusNow = time.Now

const censusCap = 200 // PRs one search lists; past it the census says so

// CensusQuery echoes the selection, keys in wire order.
type CensusQuery struct {
	Base    *string `json:"base"`
	Since   *string `json:"since"` // YYYY-MM-DD, created on or after
	Author  *string `json:"author"`
	State   string  `json:"state"`
	Numbers []int   `json:"numbers"`
}

// parseCensusQuery validates the selector flags and positionals.
func parseCensusQuery(flags map[string]string, pos []string, now time.Time) (CensusQuery, error) {
	q := CensusQuery{State: "open", Numbers: []int{}}
	if v, ok := flags["base"]; ok {
		q.Base = &v
	}
	if v, ok := flags["author"]; ok {
		q.Author = &v
	}
	if v, ok := flags["state"]; ok {
		if !slices.Contains([]string{"open", "merged", "closed", "all"}, v) {
			return q, fmt.Errorf("--state must be open, merged, closed or all, not %q", v)
		}
		q.State = v
	}
	if v, ok := flags["since"]; ok {
		day, err := parseSince(v, now)
		if err != nil {
			return q, err
		}
		q.Since = &day
	}
	for _, p := range pos {
		n, ok := positiveInt(strings.TrimPrefix(p, "#"))
		if !ok {
			return q, fmt.Errorf("unexpected argument %q (a PR number)", p)
		}
		q.Numbers = append(q.Numbers, n)
	}
	return q, nil
}

var sinceDays = regexp.MustCompile(`^(\d{1,4})d$`)

// parseSince reads "<N>d" (N days back from now) or a YYYY-MM-DD date.
func parseSince(v string, now time.Time) (string, error) {
	if m := sinceDays.FindStringSubmatch(v); m != nil {
		n, _ := strconv.Atoi(m[1])
		return now.AddDate(0, 0, -n).Format(time.DateOnly), nil
	}
	if _, err := time.Parse(time.DateOnly, v); err != nil {
		return "", fmt.Errorf("--since takes <N>d or YYYY-MM-DD, not %q", v)
	}
	return v, nil
}

// search is the GitHub search string for a selection without PR numbers.
func (q CensusQuery) search(slug string) string {
	parts := []string{"repo:" + slug, "is:pr"}
	switch q.State {
	case "open":
		parts = append(parts, "is:open")
	case "merged":
		parts = append(parts, "is:merged")
	case "closed":
		parts = append(parts, "is:closed", "is:unmerged")
	}
	if q.Base != nil {
		parts = append(parts, "base:"+*q.Base)
	}
	if q.Author != nil {
		parts = append(parts, "author:"+*q.Author)
	}
	if q.Since != nil {
		parts = append(parts, "created:>="+*q.Since)
	}
	return strings.Join(parts, " ") + " sort:created-desc"
}

const prFragment = `fragment pr on PullRequest {
  number url title body isDraft state createdAt updatedAt
  baseRefName headRefName headRefOid
  headRepository { nameWithOwner }
  author { login }
  additions deletions changedFiles
  reviewDecision
  latestOpinionatedReviews(first: 50) { nodes { state } }
  reviewThreads(first: 100) { nodes { isResolved } }
  commits(last: 1) { nodes { commit { committedDate statusCheckRollup { state
    contexts(first: 100) { nodes { __typename ... on CheckRun { name conclusion } ... on StatusContext { context state } } } } } } }
}`

const searchQuery = `query($q: String!, $after: String) {
  search(query: $q, type: ISSUE, first: 50, after: $after) {
    pageInfo { hasNextPage endCursor }
    nodes { ...pr }
  }
}
` + prFragment

// numbersQuery fetches PRs by number, one alias each.
func numbersQuery(numbers []int) string {
	var b strings.Builder
	b.WriteString("query($owner: String!, $repo: String!) {\n  repository(owner: $owner, name: $repo) {\n")
	for _, n := range numbers {
		fmt.Fprintf(&b, "    pr%d: pullRequest(number: %d) { ...pr }\n", n, n)
	}
	b.WriteString("  }\n}\n")
	return b.String() + prFragment
}

// ghPR is one PullRequest node.
type ghPR struct {
	Number         int    `json:"number"`
	URL            string `json:"url"`
	Title          string `json:"title"`
	Body           string `json:"body"`
	IsDraft        bool   `json:"isDraft"`
	State          string `json:"state"`
	CreatedAt      string `json:"createdAt"`
	UpdatedAt      string `json:"updatedAt"`
	BaseRefName    string `json:"baseRefName"`
	HeadRefName    string `json:"headRefName"`
	HeadRefOid     string `json:"headRefOid"`
	HeadRepository *struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"headRepository"`
	Author *struct {
		Login string `json:"login"`
	} `json:"author"`
	Additions                int     `json:"additions"`
	Deletions                int     `json:"deletions"`
	ChangedFiles             int     `json:"changedFiles"`
	ReviewDecision           *string `json:"reviewDecision"`
	LatestOpinionatedReviews struct {
		Nodes []struct {
			State string `json:"state"`
		} `json:"nodes"`
	} `json:"latestOpinionatedReviews"`
	ReviewThreads struct {
		Nodes []struct {
			IsResolved bool `json:"isResolved"`
		} `json:"nodes"`
	} `json:"reviewThreads"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				CommittedDate     string `json:"committedDate"`
				StatusCheckRollup *struct {
					State    string `json:"state"`
					Contexts struct {
						Nodes []struct {
							Typename   string `json:"__typename"`
							Name       string `json:"name"`
							Conclusion string `json:"conclusion"`
							Context    string `json:"context"`
							State      string `json:"state"`
						} `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

// Drift is a PR head against its base, keys in wire order.
type Drift struct {
	Ahead         int    `json:"ahead"`
	Behind        int    `json:"behind"`
	MergeBase     string `json:"mergeBase"`
	MergeBaseDate string `json:"mergeBaseDate"`
}

// Merge is `git merge-tree` of base and head. EmptyMerge: merging changes
// nothing — the content is already on the base. PatchOnBase: every commit
// of the PR has a patch-equivalent commit on the base (`git cherry`).
type Merge struct {
	Clean         bool     `json:"clean"`
	Conflicts     int      `json:"conflicts"`
	ConflictFiles []string `json:"conflictFiles"`
	EmptyMerge    bool     `json:"emptyMerge"`
	PatchOnBase   bool     `json:"patchOnBase"`
}

// CI is the head commit's check rollup.
type CI struct {
	State   string   `json:"state"` // success | failure | error | pending | expected | none
	Failing []string `json:"failing"`
}

// Review is the PR's review state.
type Review struct {
	Decision          string `json:"decision"` // approved | changes_requested | review_required | none
	Approvals         int    `json:"approvals"`
	UnresolvedThreads int    `json:"unresolvedThreads"`
}

// Local is the worktree holding the PR's head branch on this machine.
type Local struct {
	Worktree    string `json:"worktree"`
	Dirty       int    `json:"dirty"`
	Unpushed    int    `json:"unpushed"`
	LastWriteAt string `json:"lastWriteAt"` // newest uncommitted file, "" when clean
}

// CensusPR is one census row, keys in wire order. Split, Drift and Merge
// are null when the head could not be read; Drift and Merge also for a PR
// that is no longer open.
type CensusPR struct {
	Number       int      `json:"number"`
	URL          string   `json:"url"`
	Title        string   `json:"title"`
	Author       string   `json:"author"`
	State        string   `json:"state"`
	Draft        bool     `json:"draft"`
	Base         string   `json:"base"`
	Head         string   `json:"head"`
	HeadSHA      string   `json:"headSha"`
	CreatedAt    string   `json:"createdAt"`
	UpdatedAt    string   `json:"updatedAt"`
	LastCommitAt string   `json:"lastCommitAt"`
	AgeDays      int      `json:"ageDays"`
	Refs         []string `json:"refs"`
	Size         Lines    `json:"size"`
	Split        *Split   `json:"split"`
	Touches      []string `json:"touches"`
	Drift        *Drift   `json:"drift"`
	Merge        *Merge   `json:"merge"`
	CI           CI       `json:"ci"`
	Review       Review   `json:"review"`
	Local        *Local   `json:"local"`
	Activity     Activity `json:"activity"`
	Notes        []string `json:"notes"`
}

// Census is pr-census's output, keys in wire order.
type Census struct {
	Repo     string      `json:"repo"`
	Query    CensusQuery `json:"query"`
	ProdRefs []string    `json:"prodRefs"`
	Fetched  bool        `json:"fetched"`
	PRs      []CensusPR  `json:"prs"`
	Notes    []string    `json:"notes"`
}

var (
	taskRef  = regexp.MustCompile(`\b[vV][tT]-\d+\b`)
	titleRef = regexp.MustCompile(`#\d+\b`)
	closeRef = regexp.MustCompile(`(?i)\b(?:fix(?:es|ed)?|close[sd]?|resolve[sd]?)\s+#\d+\b`)
)

// refsOf lists the task and issue references a PR carries: vt- ids anywhere,
// #N in the title, closing keywords in the body; first-seen order, at most 12.
func refsOf(title, body string) []string {
	out := []string{}
	add := func(r string) {
		if !slices.Contains(out, r) && len(out) < 12 {
			out = append(out, r)
		}
	}
	for _, r := range taskRef.FindAllString(title+"\n"+body, -1) {
		add(strings.ToLower(r))
	}
	for _, r := range titleRef.FindAllString(title, -1) {
		add(r)
	}
	for _, m := range closeRef.FindAllString(body, -1) {
		add(m[strings.LastIndex(m, "#"):])
	}
	return out
}

// ciOf reads the head commit's rollup; a check counts as failing on a
// failing conclusion, never on cancelled or skipped.
func ciOf(pr ghPR) CI {
	ci := CI{State: "none", Failing: []string{}}
	if len(pr.Commits.Nodes) == 0 || pr.Commits.Nodes[0].Commit.StatusCheckRollup == nil {
		return ci
	}
	rollup := pr.Commits.Nodes[0].Commit.StatusCheckRollup
	ci.State = strings.ToLower(rollup.State)
	for _, c := range rollup.Contexts.Nodes {
		switch {
		case c.Typename == "CheckRun" && slices.Contains([]string{"FAILURE", "TIMED_OUT", "STARTUP_FAILURE", "ACTION_REQUIRED"}, c.Conclusion):
			ci.Failing = append(ci.Failing, c.Name)
		case c.Typename == "StatusContext" && (c.State == "FAILURE" || c.State == "ERROR"):
			ci.Failing = append(ci.Failing, c.Context)
		}
	}
	return ci
}

func reviewOf(pr ghPR) Review {
	r := Review{Decision: "none"}
	if pr.ReviewDecision != nil && *pr.ReviewDecision != "" {
		r.Decision = strings.ToLower(*pr.ReviewDecision)
	}
	for _, n := range pr.LatestOpinionatedReviews.Nodes {
		if n.State == "APPROVED" {
			r.Approvals++
		}
	}
	for _, t := range pr.ReviewThreads.Nodes {
		if !t.IsResolved {
			r.UnresolvedThreads++
		}
	}
	return r
}

// censusRow is a PR's GitHub facts; the git, local and activity facts are
// filled by the caller.
func censusRow(pr ghPR, now time.Time) CensusPR {
	row := CensusPR{
		Number: pr.Number, URL: pr.URL, Title: pr.Title, State: strings.ToLower(pr.State), Draft: pr.IsDraft,
		Base: pr.BaseRefName, Head: pr.HeadRefName, HeadSHA: pr.HeadRefOid,
		CreatedAt: pr.CreatedAt, UpdatedAt: pr.UpdatedAt, Refs: refsOf(pr.Title, pr.Body),
		Size:    Lines{Files: pr.ChangedFiles, Additions: pr.Additions, Deletions: pr.Deletions},
		Touches: []string{}, CI: ciOf(pr), Review: reviewOf(pr),
		Activity: Activity{State: "idle", Holders: []Holder{}}, Notes: []string{},
	}
	if pr.Author != nil {
		row.Author = pr.Author.Login
	}
	if len(pr.Commits.Nodes) > 0 {
		row.LastCommitAt = pr.Commits.Nodes[0].Commit.CommittedDate
	}
	if created, err := time.Parse(time.RFC3339, pr.CreatedAt); err == nil {
		row.AgeDays = int(now.Sub(created).Hours() / 24)
	}
	return row
}

// gitFacts fills split, drift and merge from the local object store.
func gitFacts(row *CensusPR, root string, split splitConfig) {
	opts := execOpts{dir: root, timeout: 2 * time.Minute, maxBuffer: 64 << 20}
	base := "refs/remotes/origin/" + row.Base
	if _, err := execFile(opts, "git", "cat-file", "-e", row.HeadSHA+"^{commit}"); err != nil {
		row.Notes = append(row.Notes, "head "+row.HeadSHA+" is not in the local object store (fetch it, or drop --no-fetch)")
		return
	}
	mb, err := execFile(opts, "git", "merge-base", base, row.HeadSHA)
	if err != nil {
		row.Notes = append(row.Notes, "no merge base with "+base)
		return
	}
	mb = strings.TrimSpace(mb)
	numstat, err := execFile(opts, "git", "diff", "--numstat", "-z", "-M", mb, row.HeadSHA)
	if err != nil {
		row.Notes = append(row.Notes, "diff failed: "+firstLine(err.Error()))
		return
	}
	s, touches := split.splitDiff(parseNumstatZ(numstat))
	row.Split, row.Touches = &s, touches
	if row.State != "open" {
		return
	}
	drift := &Drift{MergeBase: mb}
	if counts, err := execFile(opts, "git", "rev-list", "--left-right", "--count", base+"..."+row.HeadSHA); err == nil {
		if f := strings.Fields(counts); len(f) == 2 {
			drift.Behind, _ = strconv.Atoi(f[0])
			drift.Ahead, _ = strconv.Atoi(f[1])
		}
	}
	if date, err := execFile(opts, "git", "show", "-s", "--format=%cI", mb); err == nil {
		drift.MergeBaseDate = strings.TrimSpace(date)
	}
	row.Drift = drift
	row.Merge = mergeFacts(opts, base, row.HeadSHA, &row.Notes)
}

// mergeFacts runs the read-only merge probe (git >= 2.38) and git cherry.
func mergeFacts(opts execOpts, base, head string, notes *[]string) *Merge {
	out, err := execFile(opts, "git", "merge-tree", "--write-tree", "--name-only", "--no-messages", base, head)
	m := &Merge{ConflictFiles: []string{}}
	var exit *exec.ExitError
	switch {
	case err == nil:
		m.Clean = true
	case errors.As(err, &exit) && exit.ExitCode() == 1:
	default:
		*notes = append(*notes, "merge probe failed: "+firstLine(err.Error()))
		return nil
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for _, f := range lines[1:] {
		if f != "" && !slices.Contains(m.ConflictFiles, f) {
			m.ConflictFiles = append(m.ConflictFiles, f)
		}
	}
	m.Conflicts = len(m.ConflictFiles)
	if len(m.ConflictFiles) > 20 {
		m.ConflictFiles = m.ConflictFiles[:20]
	}
	if m.Clean {
		if tree, err := execFile(opts, "git", "rev-parse", base+"^{tree}"); err == nil && strings.TrimSpace(tree) == strings.TrimSpace(lines[0]) {
			m.EmptyMerge = true
		}
	}
	if cherry, err := execFile(opts, "git", "cherry", base, head); err == nil {
		m.PatchOnBase = patchOnBase(cherry)
	} else {
		*notes = append(*notes, "git cherry failed: "+firstLine(err.Error()))
	}
	return m
}

// patchOnBase reads `git cherry`: true when it lists commits and every one
// is "-" (an equivalent patch is on the base).
func patchOnBase(cherry string) bool {
	seen := false
	for line := range strings.SplitSeq(cherry, "\n") {
		switch {
		case strings.HasPrefix(line, "+"):
			return false
		case strings.HasPrefix(line, "-"):
			seen = true
		}
	}
	return seen
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// localFacts reads the worktree holding the head branch, nil when none.
func localFacts(path, headSHA string) *Local {
	if path == "" {
		return nil
	}
	opts := execOpts{dir: path, timeout: 30 * time.Second, maxBuffer: 16 << 20}
	local := &Local{Worktree: path}
	if status, err := execFile(opts, "git", "status", "--porcelain"); err == nil {
		var newest time.Time
		for _, p := range parsePorcelain(status) {
			local.Dirty++
			if info, err := os.Lstat(filepath.Join(path, p)); err == nil && info.ModTime().After(newest) {
				newest = info.ModTime()
			}
		}
		if !newest.IsZero() {
			local.LastWriteAt = newest.UTC().Format(time.RFC3339)
		}
	}
	if count, err := execFile(opts, "git", "rev-list", "--count", headSHA+"..HEAD"); err == nil {
		local.Unpushed, _ = strconv.Atoi(strings.TrimSpace(count))
	}
	return local
}

// fetchCensus brings the bases' remote-tracking refs and the PR heads in:
// one call, then head by head when the batch fails, so one vanished PR ref
// costs only itself.
func fetchCensus(root string, bases []string, numbers []int) []string {
	opts := execOpts{dir: root, timeout: 5 * time.Minute, maxBuffer: 16 << 20}
	var notes []string
	refspecs := []string{"fetch", "--no-tags", "--quiet", "origin"}
	for _, b := range bases {
		refspecs = append(refspecs, "+refs/heads/"+b+":refs/remotes/origin/"+b)
	}
	if _, err := execFile(opts, "git", refspecs...); err != nil {
		notes = append(notes, "fetching the bases failed: "+firstLine(err.Error()))
	}
	heads := []string{"fetch", "--no-tags", "--quiet", "origin"}
	for _, n := range numbers {
		heads = append(heads, fmt.Sprintf("refs/pull/%d/head", n))
	}
	if len(numbers) == 0 {
		return notes
	}
	if _, err := execFile(opts, "git", heads...); err != nil {
		for _, n := range numbers {
			if _, err := execFile(opts, "git", "fetch", "--no-tags", "--quiet", "origin", fmt.Sprintf("refs/pull/%d/head", n)); err != nil {
				notes = append(notes, fmt.Sprintf("fetching #%d's head failed: %s", n, firstLine(err.Error())))
			}
		}
	}
	return notes
}

// listCensusPRs runs the search (paged, capped) or the by-number query.
func listCensusPRs(opts execOpts, owner, name string, q CensusQuery) ([]ghPR, []string, error) {
	if len(q.Numbers) > 0 {
		// A number that is no PR makes gh exit 1 with the other aliases'
		// data still printed: keep the data, report the miss as a note.
		out, err := execFile(opts, "gh", "api", "graphql", "-f", "query="+numbersQuery(q.Numbers), "-f", "owner="+owner, "-f", "repo="+name)
		var resp struct {
			Data *struct {
				Repository map[string]*ghPR `json:"repository"`
			} `json:"data"`
		}
		if jerr := json.Unmarshal([]byte(out), &resp); jerr != nil || resp.Data == nil {
			if err == nil {
				err = errors.New("graphql response carries no repository data")
			}
			return nil, nil, err
		}
		var prs []ghPR
		var notes []string
		for _, n := range q.Numbers {
			if pr := resp.Data.Repository[fmt.Sprintf("pr%d", n)]; pr != nil {
				prs = append(prs, *pr)
			} else {
				notes = append(notes, fmt.Sprintf("#%d is not a pull request in %s/%s", n, owner, name))
			}
		}
		return prs, notes, nil
	}
	var prs []ghPR
	var notes []string
	cursor := ""
	for {
		args := []string{"api", "graphql", "-f", "query=" + searchQuery, "-f", "q=" + q.search(owner+"/"+name)}
		if cursor != "" {
			args = append(args, "-f", "after="+cursor)
		}
		out, err := execFile(opts, "gh", args...)
		if err != nil {
			return nil, nil, err
		}
		var resp struct {
			Data struct {
				Search struct {
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []ghPR `json:"nodes"`
				} `json:"search"`
			} `json:"data"`
		}
		if err := json.Unmarshal([]byte(out), &resp); err != nil {
			return nil, nil, err
		}
		for _, pr := range resp.Data.Search.Nodes {
			if pr.Number != 0 {
				prs = append(prs, pr)
			}
		}
		page := resp.Data.Search.PageInfo
		if !page.HasNextPage {
			return prs, notes, nil
		}
		if len(prs) >= censusCap {
			notes = append(notes, fmt.Sprintf("listing capped at %d PRs; narrow the selection to see the rest", censusCap))
			return prs[:censusCap], notes, nil
		}
		cursor = page.EndCursor
	}
}

// prodRefsOf is PROD_BRANCHES, else the default branch.
func prodRefsOf(cfg gitConfig, root string) []string {
	if refs := listItems(cfg["PROD_BRANCHES"]); len(refs) > 0 {
		return refs
	}
	if head, err := execFile(execOpts{dir: root}, "git", "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD"); err == nil {
		return []string{strings.TrimPrefix(strings.TrimSpace(head), "refs/remotes/origin/")}
	}
	return []string{}
}

func runPRCensus(args []string, stdout, stderr io.Writer) int {
	flags, pos, err := prCensusArgs.parse("pr-census", args)
	if err != nil {
		return fail(stderr, err)
	}
	now := censusNow()
	q, err := parseCensusQuery(flags, pos, now)
	if err != nil {
		return fail(stderr, fmt.Errorf("pr-census: %w\n%s", err, prCensusArgs.usage))
	}
	root, err := repoRoot(repoAnchor(flags))
	if err != nil {
		return fail(stderr, err)
	}
	opts := execOpts{dir: root, echo: stderr, timeout: 2 * time.Minute, maxBuffer: 64 << 20}
	owner, name, err := ghRepo(opts)
	if err != nil {
		return fail(stderr, err)
	}
	slug := owner + "/" + name
	list, err := execFile(opts, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return fail(stderr, err)
	}
	worktrees := parseWorktreeList(list)
	mainClone := mainCloneOf(worktrees)
	cfg, _, err := readGitConfig(mainClone)
	if err != nil {
		return fail(stderr, err)
	}
	prs, notes, err := listCensusPRs(opts, owner, name, q)
	if err != nil {
		return fail(stderr, err)
	}
	census := Census{Repo: slug, Query: q, ProdRefs: prodRefsOf(cfg, root), PRs: []CensusPR{}, Notes: append([]string{}, notes...)}
	if _, noFetch := flags["no-fetch"]; !noFetch && len(prs) > 0 {
		var bases []string
		var numbers []int
		for _, pr := range prs {
			if !slices.Contains(bases, pr.BaseRefName) {
				bases = append(bases, pr.BaseRefName)
			}
			numbers = append(numbers, pr.Number)
		}
		census.Notes = append(census.Notes, fetchCensus(root, bases, numbers)...)
		census.Fetched = true
	}

	home, _ := os.UserHomeDir()
	var logs []agentLog
	if ps, err := execFile(execOpts{maxBuffer: 16 << 20}, "ps", "-axo", "pid=,lstart=,args="); err == nil {
		logs = claudeLogs(filepath.Join(home, ".claude"), parseClaudeProcs(ps), os.Getenv("CLAUDE_CODE_SESSION_ID"), now.Add(-claudeWindow))
	} else {
		census.Notes = append(census.Notes, "ps failed, Claude sessions not read: "+firstLine(err.Error()))
	}
	logs = append(logs, codexLogs(home, now.Add(-codexWindow))...)
	repoDirs := []string{}
	for _, w := range worktrees {
		repoDirs = append(repoDirs, w.Path)
	}

	split := newSplitConfig(cfg)
	rows := make([]CensusPR, len(prs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, pr := range prs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			row := censusRow(pr, now)
			gitFacts(&row, root, split)
			worktree := ""
			if pr.HeadRepository != nil && strings.EqualFold(pr.HeadRepository.NameWithOwner, slug) {
				worktree = findWorktreeForBranch(worktrees, pr.HeadRefName)
			}
			row.Local = localFacts(worktree, pr.HeadRefOid)
			if worktree == mainClone {
				worktree = "" // every session in the main clone would read as a holder
			}
			row.Activity.Holders = holdersOf(prTarget{number: pr.Number, slug: slug, worktree: worktree, repoDirs: repoDirs, home: home}, logs)
			if len(row.Activity.Holders) == 0 && row.Local != nil && row.Local.LastWriteAt != "" {
				if at, err := time.Parse(time.RFC3339, row.Local.LastWriteAt); err == nil && now.Sub(at) < writeWindow {
					row.Activity.Holders = append(row.Activity.Holders, Holder{Kind: "writes", Name: "uncommitted edits",
						LastEventAt: row.Local.LastWriteAt, Evidence: strconv.Itoa(row.Local.Dirty) + " dirty files"})
				}
			}
			if len(row.Activity.Holders) > 0 {
				row.Activity.State = "active"
			}
			rows[i] = row
		}()
	}
	wg.Wait()
	census.PRs = rows
	if err := writeJSON(stdout, census); err != nil {
		return fail(stderr, err)
	}
	return 0
}
