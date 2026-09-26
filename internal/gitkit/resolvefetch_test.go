package gitkit

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestParsePrArgs(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want Selector
	}{
		{nil, Selector{Kind: "current"}},
		{[]string{"142"}, Selector{Kind: "number", PR: 142}},
		{[]string{"#142"}, Selector{Kind: "number", PR: 142}},
		{[]string{"142", "in", "acme/app"}, Selector{Kind: "numberInRepo", PR: 142, Owner: "acme", Repo: "app"}},
		{[]string{"https://github.com/acme/app/pull/142"}, Selector{Kind: "url", Owner: "acme", Repo: "app", PR: 142}},
		{[]string{"latest", "by", "@alice"}, Selector{Kind: "latestByAuthor", Author: "alice"}},
		// --repo never leaks into the selector, in either spelling
		{[]string{"142", "--repo", "/abs/app"}, Selector{Kind: "number", PR: 142}},
		{[]string{"142", "--repo=/abs/app"}, Selector{Kind: "number", PR: 142}},
	} {
		if got, _, _, err := parsePrArgs(tc.argv); err != nil || got != tc.want {
			t.Errorf("parsePrArgs(%q) = %+v, %v", tc.argv, got, err)
		}
	}
	sel, flags, _, _ := parsePrArgs([]string{"142", "--once", "--every", "5m", "--include-resolved", "--no-conversation"})
	if sel.PR != 142 || flags != (FetchFlags{Once: true, IncludeResolved: true, NoConversation: true, Every: "5m"}) {
		t.Errorf("flags = %+v", flags)
	}
	if _, _, _, err := parsePrArgs([]string{"garble", "garble"}); err == nil || !strings.HasPrefix(err.Error(), `Unrecognized PR selector: "garble garble"`) {
		t.Errorf("garble: %v", err)
	}
}

// resolve-fetch takes the documented shapes and refuses what it does not
// take, instead of reading it into the selector or dropping it.
func TestResolveFetchRefusesWhatItDoesNotTake(t *testing.T) {
	for _, tc := range []struct {
		argv   []string
		pr     int
		anchor []string
	}{
		{[]string{"42", "--repo", "/abs"}, 42, []string{"--repo", "/abs"}},
		{[]string{"--repo", "/abs", "42", "--include-resolved", "--no-conversation"}, 42, []string{"--repo", "/abs"}},
		{[]string{"https://github.com/acme/app/pull/42/files", "--every=5m", "--once"}, 42, nil},
		{[]string{"https://github.com/acme/app/pull/42."}, 42, nil}, // pasted from a sentence
		{[]string{"https://github.com/acme/app/pull/42)"}, 42, nil},
		{nil, 0, nil},
	} {
		sel, _, anchor, err := parsePrArgs(tc.argv)
		if err != nil || sel.PR != tc.pr || !slices.Equal(anchor, tc.anchor) {
			t.Errorf("parsePrArgs(%q) = %+v %q %v", tc.argv, sel, anchor, err)
		}
	}
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"42", "--no-cr"}, "unknown argument --no-cr"},
		{[]string{"42", "--repo", "/a", "--repo=/b"}, "--repo is given twice"},
		{[]string{"42", "--auto"}, "unknown argument --auto"},
		{[]string{"42", "43"}, `Unrecognized PR selector: "42 43"`},
		{[]string{"https://github.com/acme/app/pull/42", "43"}, "Unrecognized PR selector"},
		{[]string{"https://github.com/acme/app/pull/42abc"}, "Unrecognized PR selector"},
		{[]string{"42", "in", "acme/app", "43"}, `unexpected argument "43"`},
	} {
		var out, errb strings.Builder
		if code := runResolveFetch(tc.argv, &out, &errb); code != 1 || !strings.Contains(errb.String(), tc.want) ||
			!strings.Contains(errb.String(), resolveFetchArgs.usage) || out.Len() != 0 {
			t.Errorf("resolve-fetch %q: exit %d, stderr %q, want %q + usage", tc.argv, code, errb.String(), tc.want)
		}
	}
}

func TestIsFilteredBot(t *testing.T) {
	for login, want := range map[string]bool{"dependabot[bot]": true, "renovate": true, "olivia": false, "eve-bot-lovinka": false} {
		if isFilteredBot(login) != want {
			t.Errorf("isFilteredBot(%s) != %v", login, want)
		}
	}
}

func threads(t *testing.T, raw string) []reviewThread {
	t.Helper()
	var out []reviewThread
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

const humanThread = `[{"id":"RT_1","isResolved":%s,"isOutdated":false,"comments":{"nodes":[
	{"databaseId":11,"path":%s,"line":5,"originalLine":5,"body":"use a guard here","url":"https://x/11","author":{"login":"%s"},"replyTo":null}]}}]`

func thread(t *testing.T, resolved, path, author string) []reviewThread {
	return threads(t, sprintf(humanThread, resolved, path, author))
}

func sprintf(format string, a ...string) string {
	for _, s := range a {
		format = strings.Replace(format, "%s", s, 1)
	}
	return format
}

func TestBucketFindings(t *testing.T) {
	f, _ := bucketFindings(thread(t, "false", `"src/a.ts"`, "alice"), "olivia", false)
	if len(f) != 1 || f[0].File != "src/a.ts:5" || f[0].By != "@alice" || f[0].Surface != "inline" || !f[0].Resolvable {
		t.Errorf("human finding = %+v", f)
	}
	if f, s := bucketFindings(thread(t, "false", `"src/a.ts"`, "alice"), "alice", false); len(f) != 0 || s.Self != 1 {
		t.Errorf("own thread must be self: %+v %+v", f, s)
	}
	if f, s := bucketFindings(thread(t, "true", `"src/a.ts"`, "alice"), "olivia", false); len(f) != 0 || s.Resolved != 1 {
		t.Errorf("resolved thread must be skipped: %+v", f)
	}
	if f, _ := bucketFindings(thread(t, "true", `"src/a.ts"`, "alice"), "olivia", true); len(f) != 1 {
		t.Errorf("includeResolved must keep it")
	}
	// A GitHub App is filtered; eve posts as a User and still reaches the round.
	if f, s := bucketFindings(thread(t, "false", `"src/b.ts"`, "some-app[bot]"), "olivia", false); len(f) != 0 || s.Bots != 1 {
		t.Errorf("[bot] thread must be filtered: %+v", f)
	}
	// A pathless review thread stays resolvable and is NOT the conversation surface.
	f, _ = bucketFindings(thread(t, "false", `null`, "alice"), "bob", false)
	if len(f) != 1 || f[0].Surface != "review-thread" || !f[0].Resolvable || *f[0].ThreadID != "RT_1" || f[0].File != "(review thread)" {
		t.Errorf("pathless thread = %+v", f)
	}
}

func comment(id int, body *string, author string) nonThreadComment {
	return nonThreadComment{DatabaseID: id, Body: body, URL: "https://x/#r", Author: &login{Login: author}}
}

func TestBucketNonThread(t *testing.T) {
	f, _ := bucketNonThread([]nonThreadComment{comment(1, ptr("LGTM but please also cover the null case"), "alice")}, nil, "bob")
	if len(f) != 1 || f[0].Surface != "review-summary" || f[0].Resolvable || f[0].ThreadID != nil || f[0].By != "@alice" {
		t.Errorf("review summary = %+v", f)
	}
	// an approval with no prose is informational, not work
	if f, s := bucketNonThread([]nonThreadComment{comment(2, ptr(""), "alice"), comment(3, nil, "alice")}, nil, "bob"); len(f) != 0 || s.Informational != 2 {
		t.Errorf("empty approvals = %+v %+v", f, s)
	}
	f, _ = bucketNonThread(nil, []nonThreadComment{comment(4, ptr("Can this also handle the sk-SK locale?"), "alice")}, "bob")
	if len(f) != 1 || f[0].Surface != "conversation" || f[0].File != "(PR conversation)" {
		t.Errorf("conversation = %+v", f)
	}
	for _, tc := range []struct {
		body, author string
		want         Skipped
	}{
		{"note to self", "bob", Skipped{Self: 1}},
		{"**Actionable comments posted: 3**", "eve-bot-lovinka", Skipped{Informational: 1}},
		{"Deploy preview ready", "vercel[bot]", Skipped{Bots: 1}},
	} {
		if f, s := bucketNonThread(nil, []nonThreadComment{comment(5, &tc.body, tc.author)}, "bob"); len(f) != 0 || s != tc.want {
			t.Errorf("%q: %+v %+v", tc.body, f, s)
		}
	}
	// a real bot ask in the conversation still surfaces
	if f, _ := bucketNonThread(nil, []nonThreadComment{comment(7, ptr("Please rebase — main moved."), "eve-bot-lovinka")}, "bob"); len(f) != 1 {
		t.Error("a real bot ask must surface")
	}
}

func TestIsBotStatusBody(t *testing.T) {
	for _, body := range []string{
		"<!-- walkthrough_start -->\nsome html", "**Actionable comments posted: 0**",
		"## Walkthrough\nThis change does…", "<details><summary>Review details</summary>stuff</details>",
		// eve status shapes, found live on FixIt #702
		"## 🐉 eve review — 🟡 Review comments\n\ndetails…", "### eve review — ✅ APPROVE",
		"🐉 **eve review — ✅ APPROVE · 8 findings**", "Delta review for **PR #702** completed and posted.",
		"Delta review for **acme/app #702** completed and posted.", "PR #702 reviewed and **approved**.",
	} {
		if !isBotStatusBody(body) {
			t.Errorf("should be status: %q", body)
		}
	}
	// NEGATIVE CONTROL: a real ask is never filtered as status.
	for _, body := range []string{
		"Can this also handle the sk-SK locale?", "Please rebase — main moved.",
		"LGTM but the null case needs a test before I approve.",
		"I reviewed this locally and the migration ordering looks wrong.",
		"One more thing: the eve review flagged something you didn't answer.",
	} {
		if isBotStatusBody(body) {
			t.Errorf("must NOT be status: %q", body)
		}
	}
}

// Bodies are cut at 400 UTF-16 units, as JavaScript's slice does — a cut
// through a surrogate pair keeps the lone high half, encoded as JSON.stringify
// writes it.
func TestJSSliceAndJSString(t *testing.T) {
	cut := jsSlice(strings.Repeat("a", 399)+"🟡tail", 400)
	b, _ := json.Marshal(cut)
	if want := `"` + strings.Repeat("a", 399) + `\ud83d"`; string(b) != want {
		t.Errorf("surrogate cut = %s", b)
	}
	if got := jsSlice("é🟡x", 3).String(); got != "é🟡" {
		t.Errorf("whole pair kept = %q", got)
	}
	var out strings.Builder
	if err := writeJSON(&out, jsString{s: "a<b>&\u2028\b\f\x01\"\\"}); err != nil {
		t.Fatal(err)
	}
	if want := "\"a<b>&\u2028\\b\\f\\u0001\\\"\\\\\"\n"; out.String() != want {
		t.Errorf("jsString = %q, want %q", out.String(), want)
	}
}
