package gitkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// pr-events — the self-terminating PR watcher behind prm's native Monitor:
// `pr-events <pr> [--every-seconds N] [--repo <abs path>|owner/name]` polls
// the PR and prints ONE event line per state delta, then EXITS when the PR is
// merged or closed. Event vocabulary (the prm loop maps each to an action):
//
//	comment <id>              new review-thread OR conversation comment
//	ci <prev>-><curr>         CI rollup flip
//	review <STATE> by <login> reviewer verdict change
//	push <sha7>               the PR head moved (incl. force-push)
//	mergeable <prev>-><curr>  MERGEABLE <-> CONFLICTING (UNKNOWN suppressed)
//	draft / ready             draft state flips
//	merged / closed           terminal — the watcher exits
//
// Durability: every gh call is timeout-bounded, startup retries, and poll
// failures back off exponentially (rate-limit-looking ones jump to the max).

// Snapshot is one poll's state. CommentIDs covers unresolved review-thread
// comments AND conversation comments — distinct id spaces, one stream. The
// optional fields are nil in older snapshots and then never fire.
type Snapshot struct {
	State      string
	CI         string
	CommentIDs []int
	Reviews    *reviewStates // latest non-COMMENTED review state per login
	HeadSha    *string
	Mergeable  *string // MERGEABLE | CONFLICTING (UNKNOWN is carried over, never stored)
	IsDraft    *bool
}

// reviewStates is a login → state map iterated in JavaScript object order:
// integer-like keys ascending, then the rest in insertion order.
type reviewStates struct {
	keys  []string
	state map[string]string
}

var arrayIndex = regexp.MustCompile(`^(0|[1-9]\d*)$`)

func (r *reviewStates) set(login, state string) {
	if r.state == nil {
		r.state = map[string]string{}
	}
	if _, ok := r.state[login]; !ok {
		r.keys = append(r.keys, login)
	}
	r.state[login] = state
}

func (r *reviewStates) ordered() []string {
	if r == nil {
		return nil
	}
	isIndex := func(k string) bool {
		n, err := strconv.ParseUint(k, 10, 64)
		return arrayIndex.MatchString(k) && err == nil && n < math.MaxUint32
	}
	var indices, rest []string
	for _, k := range r.keys {
		if isIndex(k) {
			indices = append(indices, k)
		} else {
			rest = append(rest, k)
		}
	}
	slices.SortFunc(indices, func(a, b string) int {
		x, _ := strconv.ParseUint(a, 10, 64)
		y, _ := strconv.ParseUint(b, 10, 64)
		return int(x) - int(y)
	})
	return append(indices, rest...)
}

func (r *reviewStates) get(login string) (string, bool) {
	if r == nil {
		return "", false
	}
	s, ok := r.state[login]
	return s, ok
}

type threadNode struct {
	IsResolved bool `json:"isResolved"`
	Comments   *struct {
		Nodes []struct {
			DatabaseID *int   `json:"databaseId"`
			Author     *login `json:"author"`
		} `json:"nodes"`
	} `json:"comments"`
}

// openThreadCommentIDs is every comment in an unresolved thread, not just
// its root: a reviewer replying inside an open thread changes nothing else in
// the snapshot. The watcher's own replies are round output, not new work.
func openThreadCommentIDs(threads []threadNode, self string) []int {
	ids := []int{}
	for _, t := range threads {
		if t.IsResolved || t.Comments == nil {
			continue
		}
		for _, c := range t.Comments.Nodes {
			if c.DatabaseID != nil && (c.Author == nil || c.Author.Login != self) {
				ids = append(ids, *c.DatabaseID)
			}
		}
	}
	return ids
}

// computeEvents diffs two polls. The baseline poll (prev nil) is silent —
// the session already handled the backlog — unless the PR is terminal.
func computeEvents(prev *Snapshot, curr Snapshot) ([]string, bool) {
	events := []string{}
	terminal := strings.ToUpper(curr.State) != "OPEN"
	if prev == nil {
		if terminal {
			return []string{strings.ToLower(curr.State)}, true
		}
		return events, false
	}
	for _, id := range curr.CommentIDs {
		if !slices.Contains(prev.CommentIDs, id) {
			events = append(events, fmt.Sprintf("comment %d", id))
		}
	}
	if prev.CI != curr.CI {
		events = append(events, fmt.Sprintf("ci %s->%s", prev.CI, curr.CI))
	}
	// An APPROVED review with no inline comments used to produce NO event and
	// the watch stayed silent on the exact "ready to merge" signal.
	for _, who := range curr.Reviews.ordered() {
		state, _ := curr.Reviews.get(who)
		if was, ok := prev.Reviews.get(who); !ok || was != state {
			events = append(events, fmt.Sprintf("review %s by %s", state, who))
		}
	}
	if prev.HeadSha != nil && curr.HeadSha != nil && *prev.HeadSha != *curr.HeadSha {
		events = append(events, "push "+string(jsSlice(*curr.HeadSha, 7).s))
	}
	// Only real MERGEABLE <-> CONFLICTING flips: fetchSnapshot carries the
	// previous value over GitHub's post-push UNKNOWN.
	if prev.Mergeable != nil && curr.Mergeable != nil && *prev.Mergeable != *curr.Mergeable {
		events = append(events, fmt.Sprintf("mergeable %s->%s", *prev.Mergeable, *curr.Mergeable))
	}
	if prev.IsDraft != nil && curr.IsDraft != nil && *prev.IsDraft != *curr.IsDraft {
		if *curr.IsDraft {
			events = append(events, "draft")
		} else {
			events = append(events, "ready")
		}
	}
	if terminal {
		return append(events, strings.ToLower(curr.State)), true
	}
	return events, false
}

const eventsQuery = `
query($owner:String!, $repo:String!, $pr:Int!) {
  viewer { login }
  repository(owner:$owner, name:$repo) {
    pullRequest(number:$pr) {
      state
      commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }
      reviewThreads(first: 100) {
        nodes { isResolved comments(last: 50) { nodes { databaseId author { login } } } }
      }
      latestReviews(first: 50) { nodes { state author { login } } }
      headRefOid
      mergeable
      isDraft
      comments(last: 50) { nodes { databaseId } }
    }
  }
}`

// shapeSnapshot turns the GraphQL answer into a Snapshot. prevMergeable is
// the last stored mergeability: UNKNOWN (GitHub recomputing after every
// push) carries it forward so computeEvents only sees real flips.
func shapeSnapshot(out []byte, prevMergeable *string) (Snapshot, error) {
	var resp struct {
		Data *struct {
			Viewer *login `json:"viewer"`
			Repo   *struct {
				PR *struct {
					State   string `json:"state"`
					Commits *struct {
						Nodes []struct {
							Commit *struct {
								StatusCheckRollup *struct {
									State *string `json:"state"`
								} `json:"statusCheckRollup"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"commits"`
					ReviewThreads *struct {
						Nodes []threadNode `json:"nodes"`
					} `json:"reviewThreads"`
					LatestReviews *struct {
						Nodes []struct {
							State  any `json:"state"`
							Author *struct {
								Login any `json:"login"`
							} `json:"author"`
						} `json:"nodes"`
					} `json:"latestReviews"`
					HeadRefOid any     `json:"headRefOid"`
					Mergeable  *string `json:"mergeable"`
					IsDraft    any     `json:"isDraft"`
					Comments   *struct {
						Nodes []struct {
							DatabaseID *int `json:"databaseId"`
						} `json:"nodes"`
					} `json:"comments"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return Snapshot{}, err
	}
	if resp.Data == nil || resp.Data.Repo == nil || resp.Data.Repo.PR == nil {
		return Snapshot{}, errors.New("graphql response carries no pullRequest")
	}
	d := resp.Data.Repo.PR
	s := Snapshot{State: d.State, CI: "NONE", Reviews: &reviewStates{}}
	if d.Commits != nil && len(d.Commits.Nodes) > 0 {
		if c := d.Commits.Nodes[0].Commit; c != nil && c.StatusCheckRollup != nil && c.StatusCheckRollup.State != nil {
			s.CI = *c.StatusCheckRollup.State
		}
	}
	self := ""
	if resp.Data.Viewer != nil {
		self = resp.Data.Viewer.Login
	}
	s.CommentIDs = []int{}
	if d.ReviewThreads != nil {
		s.CommentIDs = openThreadCommentIDs(d.ReviewThreads.Nodes, self)
	}
	if d.Comments != nil {
		for _, c := range d.Comments.Nodes {
			if c.DatabaseID != nil {
				s.CommentIDs = append(s.CommentIDs, *c.DatabaseID)
			}
		}
	}
	if d.LatestReviews != nil {
		for _, r := range d.LatestReviews.Nodes {
			// COMMENTED reviews already surface through their comment ids.
			state, stateOK := r.State.(string)
			if r.Author == nil || !stateOK || state == "COMMENTED" {
				continue
			}
			if who, ok := r.Author.Login.(string); ok {
				s.Reviews.set(who, state)
			}
		}
	}
	mergeable := "MERGEABLE"
	switch {
	case d.Mergeable != nil && *d.Mergeable == "UNKNOWN":
		if prevMergeable != nil {
			mergeable = *prevMergeable
		}
	case d.Mergeable != nil:
		mergeable = *d.Mergeable
	}
	s.Mergeable = &mergeable
	if sha, ok := d.HeadRefOid.(string); ok {
		s.HeadSha = &sha
	}
	draft := d.IsDraft == true
	s.IsDraft = &draft
	return s, nil
}

var ownerName = regexp.MustCompile(`^[^/\s]+/[^/\s]+$`)

// parseRepoFlag accepts BOTH --repo forms: a path (absolute, ./-relative or
// empty) is the anchor for repoRoot; `owner/name` pins the API target and
// must be stripped from the argv repoRoot reads — it would reject a
// non-directory and starve every poll. Anything else fails at startup.
func parseRepoFlag(value string) (nameWithOwner string, strip bool, err error) {
	if value == "" || strings.HasPrefix(value, "/") || strings.HasPrefix(value, ".") {
		return "", false, nil
	}
	if ownerName.MatchString(value) {
		return value, true, nil
	}
	return "", false, fmt.Errorf("--repo expects owner/name or an absolute repo path, got: %s", value)
}

// pollInterval is `--every-seconds N` with a 30 s floor. A value that is not
// a number, or past what a timer can hold, is the default — Node's NaN or
// overflowing setTimeout would poll every millisecond.
func pollInterval(args []string) time.Duration {
	seconds := 30.0
	if i := slices.Index(args, "--every-seconds"); i >= 0 && i+1 < len(args) {
		if n, ok := jsNumber(args[i+1]); ok && n > seconds && n*1000 <= math.MaxInt32 {
			seconds = n
		}
	}
	return time.Duration(seconds * float64(time.Second))
}

var rateLimitish = regexp.MustCompile(`(?i)rate limit|429|403`)

// sleep is the watcher's clock; tests replace it.
var sleep = time.Sleep

func runPREvents(args []string, stdout, stderr io.Writer) int {
	prArg := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			prArg = a
			break
		}
	}
	pr, ok := positiveInt(prArg)
	if !ok || prArg == "" {
		return fail(stderr, errors.New("usage: pr-events.ts <pr> [--repo <abs repo path>|owner/name] [--every-seconds N]"))
	}
	every := pollInterval(args)

	// --repo owner/name pins the target. Without it the repo is derived from
	// the anchor — a cross-repo watch from another project's directory once
	// polled the WRONG repo's PR #340 and exited on a bogus `closed`.
	repoValue := ""
	if i := slices.Index(args, "--repo"); i >= 0 && i+1 < len(args) {
		repoValue = args[i+1]
	}
	nameWithOwner, strip, err := parseRepoFlag(repoValue)
	if err != nil {
		return fail(stderr, err)
	}
	anchorArgs := args
	if strip {
		i := slices.Index(args, "--repo")
		anchorArgs = slices.Delete(slices.Clone(args), i, min(i+2, len(args)))
	}
	// The anchor resolves lazily and sticks once found, as every call would
	// otherwise re-resolve it; a failure is retried with the call.
	root := ""
	gh := func(a ...string) (string, error) {
		if root == "" {
			r, err := repoRoot(anchorArgs)
			if err != nil {
				return "", err
			}
			root = r
		}
		return execFile(execOpts{dir: root, echo: stderr, timeout: 120 * time.Second, maxBuffer: 32 << 20}, "gh", a...)
	}

	// Startup is retried: a transient gh hiccup at launch must not kill a
	// watch meant to run for hours.
	for attempt := 1; nameWithOwner == ""; attempt++ {
		out, err := gh("repo", "view", "--json", "nameWithOwner")
		if err == nil {
			var repo struct {
				NameWithOwner string `json:"nameWithOwner"`
			}
			if err = json.Unmarshal([]byte(out), &repo); err == nil {
				nameWithOwner = repo.NameWithOwner
				break
			}
		}
		if attempt >= 5 {
			return fail(stderr, err)
		}
		fmt.Fprintf(stderr, "warn: startup repo lookup failed (attempt %d/5); retrying\n", attempt)
		sleep(time.Duration(min(attempt*15, 60)) * time.Second)
	}
	owner, repo, _ := strings.Cut(nameWithOwner, "/")

	var prev *Snapshot
	var prevMergeable *string
	failures := 0
	for {
		out, err := gh("api", "graphql", "-f", "query="+eventsQuery, "-f", "owner="+owner, "-f", "repo="+repo, "-F", "pr="+strconv.Itoa(pr))
		var curr Snapshot
		if err == nil {
			curr, err = shapeSnapshot([]byte(out), prevMergeable)
		}
		if err != nil {
			// One failed poll must not kill the watch: log to stderr (never the
			// event stream) and back off.
			failures++
			factor := min(1<<min(failures, 3), 8)
			if rateLimitish.MatchString(err.Error()) {
				factor = 8
			}
			fmt.Fprintf(stderr, "warn: poll failed (%s); backoff x%d\n", err, factor)
			sleep(every * time.Duration(factor))
			continue
		}
		failures = 0
		prevMergeable = curr.Mergeable
		events, done := computeEvents(prev, curr)
		for _, e := range events {
			fmt.Fprintln(stdout, e)
		}
		prev = &curr
		if done {
			return 0
		}
		sleep(every)
	}
}
