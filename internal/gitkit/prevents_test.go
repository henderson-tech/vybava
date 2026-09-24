package gitkit

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func reviewsOf(pairs ...string) *reviewStates {
	r := &reviewStates{}
	for i := 0; i+1 < len(pairs); i += 2 {
		r.set(pairs[i], pairs[i+1])
	}
	return r
}

func boolPtr(b bool) *bool { return &b }

func TestComputeEvents(t *testing.T) {
	open := func(ci string, ids ...int) Snapshot { return Snapshot{State: "OPEN", CI: ci, CommentIDs: ids} }
	with := func(s Snapshot, edit func(*Snapshot)) Snapshot { edit(&s); return s }
	for name, tc := range map[string]struct {
		prev   *Snapshot
		curr   Snapshot
		events []string
		done   bool
	}{
		// the baseline poll is silent unless the PR is already terminal
		"baseline open":   {nil, open("PENDING", 1, 2), []string{}, false},
		"baseline merged": {nil, Snapshot{State: "MERGED", CI: "SUCCESS"}, []string{"merged"}, true},
		"new comments":    {&Snapshot{State: "OPEN", CI: "PENDING", CommentIDs: []int{1}}, open("PENDING", 1, 2, 3), []string{"comment 2", "comment 3"}, false},
		"ci flip":         {&Snapshot{State: "OPEN", CI: "PENDING", CommentIDs: []int{1}}, open("SUCCESS", 1), []string{"ci PENDING->SUCCESS"}, false},
		"merged":          {&Snapshot{State: "OPEN", CI: "SUCCESS"}, Snapshot{State: "MERGED", CI: "SUCCESS"}, []string{"merged"}, true},
		"closed":          {&Snapshot{State: "OPEN", CI: "PENDING"}, Snapshot{State: "CLOSED", CI: "PENDING"}, []string{"closed"}, true},
		"no change":       {&Snapshot{State: "OPEN", CI: "SUCCESS", CommentIDs: []int{1, 2}}, open("SUCCESS", 1, 2), []string{}, false},
		"comments first":  {&Snapshot{State: "OPEN", CI: "PENDING", CommentIDs: []int{1}}, open("FAILURE", 1, 9), []string{"comment 9", "ci PENDING->FAILURE"}, false},
		// an approval with no comments must still fire
		"review approved": {&Snapshot{State: "OPEN", CI: "SUCCESS", Reviews: reviewsOf()}, with(open("SUCCESS"), func(s *Snapshot) { s.Reviews = reviewsOf("eve-bot-lovinka", "APPROVED") }), []string{"review APPROVED by eve-bot-lovinka"}, false},
		"review same":     {&Snapshot{State: "OPEN", CI: "SUCCESS", Reviews: reviewsOf("a", "CHANGES_REQUESTED")}, with(open("SUCCESS"), func(s *Snapshot) { s.Reviews = reviewsOf("a", "CHANGES_REQUESTED") }), []string{}, false},
		"review flip":     {&Snapshot{State: "OPEN", CI: "SUCCESS", Reviews: reviewsOf("a", "CHANGES_REQUESTED")}, with(open("SUCCESS"), func(s *Snapshot) { s.Reviews = reviewsOf("a", "APPROVED") }), []string{"review APPROVED by a"}, false},
		"push":            {&Snapshot{State: "OPEN", CI: "SUCCESS", HeadSha: ptr("aaaaaaa1111")}, with(open("SUCCESS"), func(s *Snapshot) { s.HeadSha = ptr("bbbbbbb2222") }), []string{"push bbbbbbb"}, false},
		"mergeable flip":  {&Snapshot{State: "OPEN", CI: "SUCCESS", Mergeable: ptr("MERGEABLE")}, with(open("SUCCESS"), func(s *Snapshot) { s.Mergeable = ptr("CONFLICTING") }), []string{"mergeable MERGEABLE->CONFLICTING"}, false},
		// fields absent from an older snapshot never fire
		"mergeable absent": {&Snapshot{State: "OPEN", CI: "SUCCESS"}, with(open("SUCCESS"), func(s *Snapshot) { s.Mergeable = ptr("CONFLICTING") }), []string{}, false},
		"draft":            {&Snapshot{State: "OPEN", CI: "SUCCESS", IsDraft: boolPtr(false)}, with(open("SUCCESS"), func(s *Snapshot) { s.IsDraft = boolPtr(true) }), []string{"draft"}, false},
		"ready":            {&Snapshot{State: "OPEN", CI: "SUCCESS", IsDraft: boolPtr(true)}, with(open("SUCCESS"), func(s *Snapshot) { s.IsDraft = boolPtr(false) }), []string{"ready"}, false},
	} {
		events, done := computeEvents(tc.prev, tc.curr)
		if !slices.Equal(events, tc.events) || done != tc.done {
			t.Errorf("%s: %q %v, want %q %v", name, events, done, tc.events, tc.done)
		}
	}
}

// Reviews iterate in JavaScript object order: integer-like logins first.
func TestReviewStatesOrder(t *testing.T) {
	if got := reviewsOf("zed", "APPROVED", "42", "APPROVED", "amy", "DISMISSED", "7", "APPROVED", "zed", "CHANGES_REQUESTED").ordered(); !slices.Equal(got, []string{"7", "42", "zed", "amy"}) {
		t.Errorf("ordered = %v", got)
	}
}

func TestParseRepoFlag(t *testing.T) {
	for _, v := range []string{"/Users/x/Work/repo", "./repo", ""} {
		if name, strip, err := parseRepoFlag(v); name != "" || strip || err != nil {
			t.Errorf("path %q: %q %v %v", v, name, strip, err)
		}
	}
	if name, strip, err := parseRepoFlag("acme/app"); name != "acme/app" || !strip || err != nil {
		t.Errorf("owner/name: %q %v %v", name, strip, err)
	}
	for _, v := range []string{"a/b/c", "just-a-name"} {
		if _, _, err := parseRepoFlag(v); err == nil || !strings.Contains(err.Error(), "owner/name or an absolute repo path") {
			t.Errorf("%q: %v", v, err)
		}
	}
}

func TestOpenThreadCommentIDs(t *testing.T) {
	var threads []threadNode
	json.Unmarshal([]byte(`[
		{"isResolved":false,"comments":{"nodes":[{"databaseId":1,"author":{"login":"reviewer"}},{"databaseId":2,"author":{"login":"me"}},{"databaseId":3,"author":{"login":"reviewer"}}]}},
		{"isResolved":true,"comments":{"nodes":[{"databaseId":4,"author":{"login":"reviewer"}}]}}]`), &threads)
	if got := openThreadCommentIDs(threads, "me"); !slices.Equal(got, []int{1, 3}) {
		t.Errorf("openThreadCommentIDs = %v", got)
	}
}

func TestShapeSnapshot(t *testing.T) {
	raw := `{"data":{"viewer":{"login":"me"},"repository":{"pullRequest":{
		"state":"OPEN","commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"PENDING"}}}]},
		"reviewThreads":{"nodes":[{"isResolved":false,"comments":{"nodes":[{"databaseId":5,"author":{"login":"alice"}},{"databaseId":6,"author":{"login":"me"}}]}}]},
		"latestReviews":{"nodes":[{"state":"COMMENTED","author":{"login":"bob"}},{"state":"APPROVED","author":{"login":"eve"}}]},
		"headRefOid":"abc1234def","mergeable":"UNKNOWN","isDraft":false,
		"comments":{"nodes":[{"databaseId":90}]}}}}}`
	s, err := shapeSnapshot([]byte(raw), ptr("CONFLICTING"))
	if err != nil {
		t.Fatal(err)
	}
	// UNKNOWN carries the last stored value; COMMENTED reviews are not states.
	if s.CI != "PENDING" || !slices.Equal(s.CommentIDs, []int{5, 90}) || *s.Mergeable != "CONFLICTING" ||
		*s.HeadSha != "abc1234def" || *s.IsDraft || !slices.Equal(s.Reviews.ordered(), []string{"eve"}) {
		t.Errorf("snapshot = %+v", s)
	}
	if s, _ := shapeSnapshot([]byte(raw), nil); *s.Mergeable != "MERGEABLE" {
		t.Errorf("UNKNOWN with no history = %s", *s.Mergeable)
	}
}

func TestPollInterval(t *testing.T) {
	for args, want := range map[string]time.Duration{
		"":                     30 * time.Second,
		"--every-seconds 60":   60 * time.Second,
		"--every-seconds 5":    30 * time.Second, // 30 s floor
		"--every-seconds abc":  30 * time.Second, // never a millisecond spin
		"--every-seconds 1e12": 30 * time.Second,
	} {
		if got := pollInterval(strings.Fields(args)); got != want {
			t.Errorf("pollInterval(%q) = %s, want %s", args, got, want)
		}
	}
}

func TestPREventsUsage(t *testing.T) {
	var stderr strings.Builder
	if code := runPREvents([]string{"--repo=/nowhere"}, &strings.Builder{}, &stderr); code != 1 || !strings.HasPrefix(stderr.String(), "error: usage: pr-events.ts <pr>") {
		t.Errorf("usage: %d %q", code, stderr.String())
	}
}
