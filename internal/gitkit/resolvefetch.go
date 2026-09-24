package gitkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// resolve-fetch — deterministic PR resolution + review-comment fetch and
// bucketing: `resolve-fetch [PR selector] [flags] [--repo <abs>]`.

// Selector is a parsed PR selector.
type Selector struct {
	Kind   string // current | number | numberInRepo | url | latestByAuthor
	PR     int
	Owner  string
	Repo   string
	Author string
}

// FetchFlags are resolve-fetch's own flags. Once and Every are accepted and
// inert, exactly as in the TypeScript: they exist so a prm command line
// carrying them never leaks `--every 5m` into the PR selector. The watch
// cadence lives in pr-events --every-seconds.
type FetchFlags struct {
	Once            bool
	IncludeResolved bool
	// NoConversation skips review summaries and PR conversation comments.
	NoConversation bool
	Every          string
}

var (
	selectorURL    = regexp.MustCompile(`^https?://github\.com/([^/]+)/([^/]+)/pull/(\d+)`)
	selectorInRepo = regexp.MustCompile(`^#?(\d+)\s+in\s+([^/\s]+)/([^/\s]+)$`)
	selectorLatest = regexp.MustCompile(`(?i)^latest\s+by\s+@?(\S+)$`)
	selectorNumber = regexp.MustCompile(`^#?(\d+)$`)
)

func parsePrArgs(argv []string) (Selector, FetchFlags, error) {
	var flags FetchFlags
	positional := []string{}
	for i := 0; i < len(argv); i++ {
		switch a := argv[i]; {
		case a == "--once":
			flags.Once = true
		case a == "--include-resolved":
			flags.IncludeResolved = true
		case a == "--no-conversation":
			flags.NoConversation = true
		case a == "--every":
			i++
			if i < len(argv) {
				flags.Every = argv[i]
			}
		// --repo belongs to the repo anchor; never let the path leak into
		// the PR selector.
		case a == "--repo":
			i++
		case strings.HasPrefix(a, "--repo="):
		default:
			positional = append(positional, a)
		}
	}
	joined := strings.TrimSpace(strings.Join(positional, " "))
	num := func(s string) int { n, _ := strconv.Atoi(s); return n }
	switch {
	case joined == "":
		return Selector{Kind: "current"}, flags, nil
	case selectorURL.MatchString(joined):
		m := selectorURL.FindStringSubmatch(joined)
		return Selector{Kind: "url", Owner: m[1], Repo: m[2], PR: num(m[3])}, flags, nil
	case selectorInRepo.MatchString(joined):
		m := selectorInRepo.FindStringSubmatch(joined)
		return Selector{Kind: "numberInRepo", PR: num(m[1]), Owner: m[2], Repo: m[3]}, flags, nil
	case selectorLatest.MatchString(joined):
		return Selector{Kind: "latestByAuthor", Author: selectorLatest.FindStringSubmatch(joined)[1]}, flags, nil
	case selectorNumber.MatchString(joined):
		return Selector{Kind: "number", PR: num(selectorNumber.FindStringSubmatch(joined)[1])}, flags, nil
	}
	return Selector{}, flags, fmt.Errorf("Unrecognized PR selector: \"%s\"", joined)
}

var filteredBots = []string{"dependabot", "renovate", "codecov", "vercel", "netlify", "github-actions"}

func isFilteredBot(login string) bool {
	l := strings.ToLower(login)
	if strings.HasSuffix(l, "[bot]") {
		return true
	}
	for _, b := range filteredBots {
		if l == b {
			return true
		}
	}
	return false
}

type login struct {
	Login string `json:"login"`
}

type threadComment struct {
	DatabaseID   int     `json:"databaseId"`
	Path         *string `json:"path"`
	Line         *int    `json:"line"`
	OriginalLine *int    `json:"originalLine"`
	Body         *string `json:"body"`
	URL          string  `json:"url"`
	Author       *login  `json:"author"`
	ReplyTo      *struct {
		DatabaseID int `json:"databaseId"`
	} `json:"replyTo"`
}

type reviewThread struct {
	ID         string `json:"id"`
	IsResolved bool   `json:"isResolved"`
	IsOutdated bool   `json:"isOutdated"`
	Comments   *struct {
		Nodes []threadComment `json:"nodes"`
	} `json:"comments"`
}

// Finding is one actionable review item, keys in wire order. Surface says
// where it lives; only Resolvable decides closure — inline and review-thread
// findings resolve their thread, review-summary and conversation findings
// have none and close with a reply plus a reaction.
type Finding struct {
	ID         int      `json:"id"`
	By         string   `json:"by"`
	File       string   `json:"file"`
	Surface    string   `json:"surface"`
	Body       jsString `json:"body"`
	Thread     string   `json:"thread"`
	ThreadID   *string  `json:"threadId"`
	Resolvable bool     `json:"resolvable"`
	URL        string   `json:"url"`
}

// Skipped counts what bucketing set aside, keys in wire order.
// Informational: non-thread bodies with no ask (empty, or bot status).
type Skipped struct {
	Bots          int `json:"bots"`
	Self          int `json:"self"`
	Resolved      int `json:"resolved"`
	Outdated      int `json:"outdated"`
	Informational int `json:"informational"`
}

// Bot bodies that are STATUS, not a request. Surfacing them opens every round
// with fake work: a review bot posts a walkthrough plus "Actionable comments
// posted: N" on every push while its real findings arrive as inline threads.
// Matched on the body, not the author, so a bot that does ask still reaches
// the round. The eve patterns were found on FixIt #702 (2026-07-26): without
// them a normal eve-reviewed PR opened every round with 8 fake findings.
var botStatusPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^<!--\s*walkthrough`),
	regexp.MustCompile(`(?i)actionable comments posted:`),
	regexp.MustCompile(`(?im)^\s*##\s*walkthrough`),
	regexp.MustCompile(`(?i)review (?:status|details)\s*<!--`),
	regexp.MustCompile(`(?is)<summary>.*(?:walkthrough|files? (?:selected )?for processing|review details).*</summary>`),
	regexp.MustCompile(`(?im)^#{1,3}\s*(?:🐉\s*)?eve review\b`),
	regexp.MustCompile(`(?i)\beve review\s*[—–-]\s*(?:✅|🟡|🔴|⚪|approve|request|review comments|no findings)`),
	regexp.MustCompile(`(?i)\bdelta review for\b[\s\S]*\bcompleted and posted\b`),
	regexp.MustCompile(`(?i)\breviewed and \*{0,2}approved\*{0,2}\b`),
}

func isBotStatusBody(body string) bool {
	for _, re := range botStatusPatterns {
		if re.MatchString(body) {
			return true
		}
	}
	return false
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func bucketFindings(threads []reviewThread, prAuthor string, includeResolved bool) ([]Finding, Skipped) {
	findings := []Finding{}
	var skipped Skipped
	for _, t := range threads {
		if t.IsResolved && !includeResolved {
			skipped.Resolved++
			continue
		}
		var nodes []threadComment
		if t.Comments != nil {
			nodes = t.Comments.Nodes
		}
		if len(nodes) == 0 {
			continue
		}
		root := nodes[0]
		for _, c := range nodes {
			if c.ReplyTo == nil {
				root = c
				break
			}
		}
		if root.Author == nil {
			continue
		}
		who := root.Author.Login
		if who == prAuthor {
			skipped.Self++
			continue
		}
		if isFilteredBot(who) {
			skipped.Bots++
			continue
		}
		body := deref(root.Body)
		if t.IsOutdated && !includeResolved && strings.TrimSpace(body) == "" {
			skipped.Outdated++
			continue
		}
		thread := "no replies"
		if replies := len(nodes) - 1; replies > 0 {
			lastBy := "unknown"
			if last := nodes[len(nodes)-1].Author; last != nil {
				lastBy = last.Login
			}
			thread = fmt.Sprintf("%d replies — last by @%s", replies, lastBy)
		}
		// A pathless review thread is a FILE- or PR-level thread: it still has
		// a threadId, so it is resolvable — never the conversation surface.
		file, surface := "(review thread)", "review-thread"
		if root.Path != nil && *root.Path != "" {
			line := "?"
			if root.Line != nil {
				line = strconv.Itoa(*root.Line)
			} else if root.OriginalLine != nil {
				line = strconv.Itoa(*root.OriginalLine)
			}
			file, surface = *root.Path+":"+line, "inline"
		}
		id := t.ID
		findings = append(findings, Finding{
			ID: root.DatabaseID, By: "@" + who, File: file, Surface: surface,
			Body: jsSlice(body, 400), Thread: thread, ThreadID: &id, Resolvable: true, URL: root.URL,
		})
	}
	return findings, skipped
}

type nonThreadComment struct {
	DatabaseID int     `json:"databaseId"`
	Body       *string `json:"body"`
	URL        string  `json:"url"`
	Author     *login  `json:"author"`
	State      string  `json:"state"`
}

// bucketNonThread buckets the surfaces that are NOT review threads: review
// summaries (an ask there has no thread, so nothing ever marks it done) and
// PR conversation comments (where a human asks for cross-file work). Neither
// is resolvable. Until 2026-07-26 both were counted and discarded, so a
// conversation ask was silently ignored while inline nits got fixed.
func bucketNonThread(reviews, comments []nonThreadComment, prAuthor string) ([]Finding, Skipped) {
	findings := []Finding{}
	var skipped Skipped
	for _, src := range []struct {
		surface, file string
		items         []nonThreadComment
	}{{"review-summary", "(review summary)", reviews}, {"conversation", "(PR conversation)", comments}} {
		for _, c := range src.items {
			body := strings.TrimSpace(deref(c.Body))
			if body == "" { // an approval with no prose is the commonest review
				skipped.Informational++
				continue
			}
			if c.Author == nil {
				continue
			}
			who := c.Author.Login
			switch {
			case who == prAuthor:
				skipped.Self++
			case isFilteredBot(who):
				skipped.Bots++
			case isBotStatusBody(body):
				skipped.Informational++
			default:
				findings = append(findings, Finding{
					ID: c.DatabaseID, By: "@" + who, File: src.file, Surface: src.surface,
					Body: jsSlice(body, 400), Thread: "not a thread — reply + react to close", URL: c.URL,
				})
			}
		}
	}
	return findings, skipped
}

const fetchQuery = `
query($owner:String!, $repo:String!, $pr:Int!, $threadCursor:String) {
  repository(owner:$owner, name:$repo) {
    pullRequest(number:$pr) {
      number title state url isDraft merged
      headRefName headRefOid baseRefName
      author { login }
      reviewThreads(first: 100, after: $threadCursor) {
        pageInfo { hasNextPage endCursor }
        nodes {
          id isResolved isOutdated
          comments(first: 50) {
            nodes {
              databaseId path line originalLine body url
              author { login }
              replyTo { databaseId }
            }
          }
        }
      }
    }
  }
  rateLimit { remaining cost resetAt }
}`

// Review summaries and conversation comments page separately from threads:
// `first: N` alone keeps the OLDEST, dropping the newest ask on a busy PR.
const nonThreadQuery = `
query($owner:String!, $repo:String!, $pr:Int!, $reviewCursor:String, $commentCursor:String) {
  repository(owner:$owner, name:$repo) {
    pullRequest(number:$pr) {
      reviews(first: 100, after: $reviewCursor) {
        pageInfo { hasNextPage endCursor }
        nodes { databaseId body url state author { login } }
      }
      comments(first: 100, after: $commentCursor) {
        pageInfo { hasNextPage endCursor }
        nodes { databaseId body url author { login } }
      }
    }
  }
}`

type pageInfo struct {
	HasNextPage bool    `json:"hasNextPage"`
	EndCursor   *string `json:"endCursor"`
}

type prMeta struct {
	Title       jsString
	State       json.RawMessage
	Merged      json.RawMessage
	HeadRefName json.RawMessage
	HeadRefOid  json.RawMessage
	BaseRefName json.RawMessage
	Author      string
}

// fetcher runs the gh/git calls one resolve-fetch invocation makes.
type fetcher struct {
	root   string
	stderr io.Writer
}

func (f fetcher) sh(name string, args ...string) (string, error) {
	return execFile(execOpts{dir: f.root, echo: f.stderr, timeout: 120 * time.Second, maxBuffer: 64 << 20}, name, args...)
}

// graphqlPR runs a query and returns data.repository.pullRequest.
func (f fetcher) graphqlPR(query string, vars ...string) (json.RawMessage, *json.RawMessage, error) {
	out, err := f.sh("gh", append([]string{"api", "graphql", "-f", "query=" + query}, vars...)...)
	if err != nil {
		return nil, nil, err
	}
	var resp struct {
		Data *struct {
			Repository *struct {
				PullRequest json.RawMessage `json:"pullRequest"`
			} `json:"repository"`
			RateLimit json.RawMessage `json:"rateLimit"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		return nil, nil, err
	}
	if resp.Data == nil || resp.Data.Repository == nil || string(resp.Data.Repository.PullRequest) == "null" || resp.Data.Repository.PullRequest == nil {
		return nil, nil, errors.New("graphql response carries no pullRequest")
	}
	return resp.Data.Repository.PullRequest, &resp.Data.RateLimit, nil
}

func (f fetcher) fetchThreads(owner, repo string, pr int) (*prMeta, []reviewThread, json.RawMessage, error) {
	var meta *prMeta
	var rateLimit json.RawMessage
	threads := []reviewThread{}
	cursor := ""
	for {
		vars := []string{"-f", "owner=" + owner, "-f", "repo=" + repo, "-F", "pr=" + strconv.Itoa(pr)}
		if cursor != "" {
			vars = append(vars, "-f", "threadCursor="+cursor)
		}
		raw, rl, err := f.graphqlPR(fetchQuery, vars...)
		if err != nil {
			return nil, nil, nil, err
		}
		rateLimit = *rl
		var data struct {
			Title         *string         `json:"title"`
			State         json.RawMessage `json:"state"`
			Merged        json.RawMessage `json:"merged"`
			HeadRefName   json.RawMessage `json:"headRefName"`
			HeadRefOid    json.RawMessage `json:"headRefOid"`
			BaseRefName   json.RawMessage `json:"baseRefName"`
			Author        *login          `json:"author"`
			ReviewThreads struct {
				PageInfo pageInfo       `json:"pageInfo"`
				Nodes    []reviewThread `json:"nodes"`
			} `json:"reviewThreads"`
		}
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, nil, nil, err
		}
		if meta == nil {
			meta = &prMeta{Title: jsString{s: deref(data.Title)}, State: data.State, Merged: data.Merged,
				HeadRefName: data.HeadRefName, HeadRefOid: data.HeadRefOid, BaseRefName: data.BaseRefName}
			if data.Author != nil {
				meta.Author = data.Author.Login
			}
		}
		threads = append(threads, data.ReviewThreads.Nodes...)
		if !data.ReviewThreads.PageInfo.HasNextPage {
			break
		}
		cursor = deref(data.ReviewThreads.PageInfo.EndCursor)
	}
	return meta, threads, rateLimit, nil
}

func (f fetcher) fetchNonThread(owner, repo string, pr int) ([]nonThreadComment, []nonThreadComment, error) {
	reviews, comments := []nonThreadComment{}, []nonThreadComment{}
	reviewCursor, commentCursor := "", ""
	reviewsDone, commentsDone := false, false
	for !reviewsDone || !commentsDone {
		vars := []string{"-f", "owner=" + owner, "-f", "repo=" + repo, "-F", "pr=" + strconv.Itoa(pr)}
		if reviewCursor != "" {
			vars = append(vars, "-f", "reviewCursor="+reviewCursor)
		}
		if commentCursor != "" {
			vars = append(vars, "-f", "commentCursor="+commentCursor)
		}
		raw, _, err := f.graphqlPR(nonThreadQuery, vars...)
		if err != nil {
			return nil, nil, err
		}
		type page struct {
			PageInfo *pageInfo          `json:"pageInfo"`
			Nodes    []nonThreadComment `json:"nodes"`
		}
		var data struct {
			Reviews  *page `json:"reviews"`
			Comments *page `json:"comments"`
		}
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, nil, err
		}
		advance := func(p *page, items *[]nonThreadComment, cursor *string, done *bool) {
			if *done {
				return
			}
			if p != nil {
				*items = append(*items, p.Nodes...)
			}
			*done = p == nil || p.PageInfo == nil || !p.PageInfo.HasNextPage
			*cursor = ""
			if p != nil && p.PageInfo != nil {
				*cursor = deref(p.PageInfo.EndCursor)
			}
		}
		advance(data.Reviews, &reviews, &reviewCursor, &reviewsDone)
		advance(data.Comments, &comments, &commentCursor, &commentsDone)
	}
	return reviews, comments, nil
}

// mirrorPr fetches the PR head into refs/pr/<N> and returns its SHA. The
// fetch WRITES refs, so it is anchored to the resolved root. origin may be
// absent or elsewhere (fork): fall back to the canonical URL, whose failure
// surfaces rather than being swallowed.
func (f fetcher) mirrorPr(owner, repo string, pr int) (string, error) {
	ref := fmt.Sprintf("pull/%d/head:refs/pr/%d", pr, pr)
	quiet := execOpts{dir: f.root, inherit: io.Discard, timeout: 120 * time.Second}
	if _, err := execFile(quiet, "git", "fetch", "origin", ref); err != nil {
		if _, err := execFile(quiet, "git", "fetch", fmt.Sprintf("https://github.com/%s/%s.git", owner, repo), ref); err != nil {
			return "", err
		}
	}
	out, err := execFile(execOpts{dir: f.root, echo: f.stderr}, "git", "rev-parse", fmt.Sprintf("refs/pr/%d", pr))
	return strings.TrimSpace(out), err
}

type repoInfo struct{ owner, repo, defaultBranch string }

// currentRepo reads owner/repo from gh and DEFAULT_BRANCH from the MAIN
// clone's config: the per-machine .local overlay is gitignored, so it only
// exists there — never in a worktree.
func (f fetcher) currentRepo() (repoInfo, error) {
	out, err := f.sh("gh", "repo", "view", "--json", "nameWithOwner,defaultBranchRef")
	if err != nil {
		return repoInfo{}, err
	}
	var j struct {
		NameWithOwner    string `json:"nameWithOwner"`
		DefaultBranchRef *struct {
			Name string `json:"name"`
		} `json:"defaultBranchRef"`
	}
	if err := json.Unmarshal([]byte(out), &j); err != nil {
		return repoInfo{}, err
	}
	owner, repo, _ := strings.Cut(j.NameWithOwner, "/")
	commonDir, err := f.sh("git", "-C", f.root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return repoInfo{}, err
	}
	cfg, _, err := readGitConfig(filepath.Dir(strings.TrimSpace(commonDir)))
	if err != nil {
		return repoInfo{}, err
	}
	githubDefault := ""
	if j.DefaultBranchRef != nil {
		githubDefault = j.DefaultBranchRef.Name
	}
	return repoInfo{owner, repo, resolveDefaultBranch(cfg, githubDefault)}, nil
}

// prNumber reads a `--jq .[0].number` answer as Number() would: 0 = none.
func prNumber(out string) int {
	n, ok := jsNumber(strings.TrimSpace(out))
	if !ok || n != float64(int(n)) {
		return 0
	}
	return int(n)
}

// NoPR is the envelope for a checkout whose branch has no PR yet.
type NoPR struct {
	NoPR            bool   `json:"noPr"`
	Owner           string `json:"owner"`
	Repo            string `json:"repo"`
	Branch          string `json:"branch"`
	DefaultBranch   string `json:"defaultBranch"`
	OnDefaultBranch bool   `json:"onDefaultBranch"`
}

// resolveTarget turns a selector into owner/repo/pr, or a NoPR envelope.
// "No PR number" means this checkout's PR, from any branch or worktree; on
// a detached HEAD the lookup goes by commit SHA so `gh pr list --head ""`
// never grabs an arbitrary open PR.
func (f fetcher) resolveTarget(sel Selector) (string, string, int, *NoPR, error) {
	switch sel.Kind {
	case "url", "numberInRepo":
		return sel.Owner, sel.Repo, sel.PR, nil, nil
	case "number":
		r, err := f.currentRepo()
		return r.owner, r.repo, sel.PR, nil, err
	case "latestByAuthor":
		r, err := f.currentRepo()
		if err != nil {
			return "", "", 0, nil, err
		}
		out, err := f.sh("gh", "pr", "list", "--author", sel.Author, "--limit", "1", "--json", "number", "--jq", ".[0].number")
		if err != nil {
			return "", "", 0, nil, err
		}
		pr := prNumber(out)
		if pr == 0 {
			return "", "", 0, nil, fmt.Errorf("No open PR by @%s in %s/%s", sel.Author, r.owner, r.repo)
		}
		return r.owner, r.repo, pr, nil, nil
	}
	r, err := f.currentRepo()
	if err != nil {
		return "", "", 0, nil, err
	}
	branchOut, err := f.sh("git", "branch", "--show-current")
	if err != nil {
		return "", "", 0, nil, err
	}
	branch := strings.TrimSpace(branchOut)
	var out string
	if branch != "" {
		out, err = f.sh("gh", "pr", "list", "--head", branch, "--json", "number", "--jq", ".[0].number")
	} else {
		var sha string
		if sha, err = f.sh("git", "rev-parse", "HEAD"); err == nil {
			out, err = f.sh("gh", "api", fmt.Sprintf("repos/%s/%s/commits/%s/pulls", r.owner, r.repo, strings.TrimSpace(sha)), "--jq", ".[0].number")
		}
	}
	if err != nil {
		return "", "", 0, nil, err
	}
	if pr := prNumber(out); pr != 0 {
		return r.owner, r.repo, pr, nil, nil
	}
	if branch == "" {
		return "", "", 0, nil, errors.New("Detached HEAD with no PR for this commit. Checkout a branch or pass a PR number explicitly.")
	}
	return "", "", 0, &NoPR{NoPR: true, Owner: r.owner, Repo: r.repo, Branch: branch, DefaultBranch: r.defaultBranch, OnDefaultBranch: branch == r.defaultBranch}, nil
}

// FetchEnvelope is resolve-fetch's output, keys in wire order.
type FetchEnvelope struct {
	Owner     string          `json:"owner"`
	Repo      string          `json:"repo"`
	PR        int             `json:"pr"`
	Title     jsString        `json:"title"`
	State     json.RawMessage `json:"state"`
	Merged    json.RawMessage `json:"merged"`
	HeadRef   json.RawMessage `json:"headRef"`
	HeadSha   json.RawMessage `json:"headSha"`
	BaseRef   json.RawMessage `json:"baseRef"`
	Findings  []Finding       `json:"findings"`
	Skipped   Skipped         `json:"skipped"`
	RateLimit json.RawMessage `json:"rateLimit"`
	Counts    FetchCounts     `json:"counts"`
}

// FetchCounts splits the findings so a round sees at a glance whether it
// owes reply-and-react closures on top of the resolvable threads.
type FetchCounts struct {
	Total      int `json:"total"`
	Resolvable int `json:"resolvable"`
	NonThread  int `json:"nonThread"`
}

func runResolveFetch(args []string, stdout, stderr io.Writer) int {
	sel, flags, err := parsePrArgs(args)
	if err != nil {
		return fail(stderr, err)
	}
	root, err := repoRoot(args)
	if err != nil {
		return fail(stderr, err)
	}
	f := fetcher{root: root, stderr: stderr}
	owner, repo, pr, noPR, err := f.resolveTarget(sel)
	if err != nil {
		return fail(stderr, err)
	}
	if noPR != nil {
		if err := writeJSON(stdout, noPR); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	meta, threads, rateLimit, err := f.fetchThreads(owner, repo, pr)
	if err != nil {
		return fail(stderr, err)
	}
	findings, inline := bucketFindings(threads, meta.Author, flags.IncludeResolved)
	// Review summaries + conversation comments; resolvable findings first —
	// a round that runs out of budget should have spent it on those.
	// --no-conversation skips their paginated fetch entirely.
	nonThread, other := []Finding{}, Skipped{}
	if !flags.NoConversation {
		reviews, comments, err := f.fetchNonThread(owner, repo, pr)
		if err != nil {
			return fail(stderr, err)
		}
		nonThread, other = bucketNonThread(reviews, comments, meta.Author)
	}
	findings = append(findings, nonThread...)

	headSha := meta.HeadRefOid
	if sha, err := f.mirrorPr(owner, repo, pr); err != nil {
		fmt.Fprintf(stderr, "warn: could not mirror PR locally (%s); using GraphQL head SHA\n", err)
	} else {
		headSha, _ = json.Marshal(sha)
	}
	if flags.NoConversation {
		fmt.Fprintln(stderr, "note: --no-conversation — review summaries and PR conversation comments were not fetched")
	} else if n := len(nonThread); n > 0 {
		plural := "s"
		if n == 1 {
			plural = ""
		}
		fmt.Fprintf(stderr, "note: %d non-thread finding%s (review summary / PR conversation) — "+
			"these have NO resolvable thread: close each with a reply + reaction, never a resolve call\n", n, plural)
	}
	resolvable := 0
	for _, f := range findings {
		if f.Resolvable {
			resolvable++
		}
	}
	envelope := FetchEnvelope{
		Owner: owner, Repo: repo, PR: pr, Title: meta.Title, State: meta.State, Merged: meta.Merged,
		HeadRef: meta.HeadRefName, HeadSha: headSha, BaseRef: meta.BaseRefName, Findings: findings,
		Skipped: Skipped{
			Bots: inline.Bots + other.Bots, Self: inline.Self + other.Self,
			Resolved: inline.Resolved, Outdated: inline.Outdated, Informational: other.Informational,
		},
		RateLimit: rateLimit,
		Counts:    FetchCounts{Total: len(findings), Resolvable: resolvable, NonThread: len(nonThread)},
	}
	if err := writeJSON(stdout, envelope); err != nil {
		return fail(stderr, err)
	}
	return 0
}
