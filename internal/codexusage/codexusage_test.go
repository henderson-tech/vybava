package codexusage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rollout writes a fixture thread: a session_meta line followed by raw event
// lines, under the dated layout Codex uses.
func rollout(t *testing.T, home, id, cwd, started string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(home, ".codex", "sessions", "2026", "09", "09")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fmt.Sprintf("rollout-%s-%s.jsonl", strings.ReplaceAll(started[:19], ":", "-"), id))
	body := []string{fmt.Sprintf(
		`{"timestamp":%q,"type":"session_meta","payload":{"id":%q,"timestamp":%q,"cwd":%q,"cli_version":"0.153.4","git":{"branch":"main"},"base_instructions":{"text":%q}}}`,
		started, id, started, cwd, strings.Repeat("padding ", 60000))}
	body = append(body, lines...)
	if err := os.WriteFile(path, []byte(strings.Join(body, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// call renders a token_count event. total is the running session total, which
// is what deduplication keys on.
func call(at string, total, last, cached int64, plan string, percent float64, resets int64) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{`+
		`"total_token_usage":{"input_tokens":%d,"cached_input_tokens":0,"cache_write_input_tokens":0,"output_tokens":0,"reasoning_output_tokens":0,"total_tokens":%d},`+
		`"last_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"cache_write_input_tokens":0,"output_tokens":0,"reasoning_output_tokens":0,"total_tokens":%d},`+
		`"model_context_window":258400},"rate_limits":{"limit_id":"codex","plan_type":%q,"primary":{"used_percent":%g,"window_minutes":10080,"resets_at":%d}}}}`,
		at, total, total, last, cached, last, plan, percent, resets)
}

// bulk is a transcript line far larger than the read buffer — the shape that
// makes a rollout hundreds of megabytes.
func bulk(at string) string {
	return fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"message","text":%q}}`, at, strings.Repeat("x", 600_000))
}

func env(t *testing.T, home string, exec func(context.Context, string, ...string) ([]byte, error)) Env {
	t.Helper()
	now, err := time.Parse(time.RFC3339, "2026-09-09T14:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	return Env{Home: home, Now: now.Local(), Exec: exec}
}

func since(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		t.Fatal(err)
	}
	return at.Local()
}

// A token_count event that repeats an unchanged session total is a rate-limit
// refresh, not a new call. Billing it again would double-count the call it
// echoes, so it must contribute no tokens — but its fresher percentage still
// has to reach the plan report.
func TestRepeatEventsAreNotBilledButRefreshTheLimit(t *testing.T) {
	home := t.TempDir()
	rollout(t, home, "aaaa", "/work/one", "2026-09-09T09:00:00Z",
		call("2026-09-09T10:00:00Z", 100, 100, 0, "pro", 10, 1789437323),
		call("2026-09-09T10:01:00Z", 100, 100, 0, "pro", 12, 1789437323),
		call("2026-09-09T10:02:00Z", 300, 200, 0, "pro", 14, 1789437323),
	)
	report, err := Run(context.Background(), env(t, home, nil), Options{Since: since(t, "2026-09-09T00:00:00Z")})
	if err != nil {
		t.Fatal(err)
	}
	if report.Calls != 2 {
		t.Fatalf("calls = %d, want 2 (the repeat must not bill)", report.Calls)
	}
	if got := report.Usage.Input; got != 300 {
		t.Fatalf("input = %d, want 300", got)
	}
	if len(report.Plans) != 1 || report.Plans[0].EndPercent != 14 || report.Plans[0].StartPercent != 10 {
		t.Fatalf("plan span = %+v, want 10 -> 14", report.Plans)
	}
}

// Calls before the window belong to an earlier question. They must not be
// billed, but the thread's start time still has to survive so a resumed
// transcript is recognisable as one.
func TestWindowExcludesEarlierCallsButKeepsThreadStart(t *testing.T) {
	home := t.TempDir()
	rollout(t, home, "bbbb", "/work/one", "2026-09-07T08:00:00Z",
		call("2026-09-07T09:00:00Z", 100, 100, 0, "pro", 5, 1789437323),
		call("2026-09-09T10:00:00Z", 900, 800, 0, "pro", 20, 1789437323),
	)
	report, err := Run(context.Background(), env(t, home, nil), Options{Since: since(t, "2026-09-09T07:00:00Z")})
	if err != nil {
		t.Fatal(err)
	}
	if report.Calls != 1 || report.Usage.Input != 800 {
		t.Fatalf("calls=%d input=%d, want 1 call of 800", report.Calls, report.Usage.Input)
	}
	if !report.Sessions[0].Resumed() {
		t.Fatal("a thread started two days before its first billed call must read as resumed")
	}
}

// Compacting rolls the process onto a fresh rollout file. Both files are the
// same terminal tab, so they must report as one row carrying the original
// thread's name — otherwise the biggest spender looks like several smaller ones.
func TestProcessRollupMergesRolloverFiles(t *testing.T) {
	home := t.TempDir()
	first := rollout(t, home, "cccc", "/work/one", "2026-09-08T08:00:00Z",
		call("2026-09-09T10:00:00Z", 500, 500, 0, "pro", 20, 1789437323))
	second := rollout(t, home, "dddd", "/work/one", "2026-09-09T11:00:00Z",
		call("2026-09-09T11:30:00Z", 200, 200, 0, "pro", 24, 1789437323))
	index := fmt.Sprintf("{\"id\":\"cccc\",\"thread_name\":\"the long one\"}\n")
	if err := os.WriteFile(filepath.Join(home, ".codex", "session_index.jsonl"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Run(context.Background(), env(t, home, fakeProcs(map[int]string{4242: first + "\n" + second})), Options{Since: since(t, "2026-09-09T00:00:00Z")})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Sessions) != 1 {
		t.Fatalf("sessions = %d, want 1 merged row", len(report.Sessions))
	}
	row := report.Sessions[0]
	if row.PID != 4242 || row.Threads != 2 || row.Calls != 2 || row.Usage.Input != 700 {
		t.Fatalf("row = pid %d threads %d calls %d input %d, want 4242/2/2/700", row.PID, row.Threads, row.Calls, row.Usage.Input)
	}
	if row.Name != "the long one" {
		t.Fatalf("name = %q, want the oldest rollout's name", row.Name)
	}
}

// Switching accounts mid-window means two quota windows with different
// allowances. A thread's percentage cost has to be charged at its own window's
// rate; charging everything at one rate mis-ranks exactly the threads the
// report exists to find.
func TestPointsAreChargedPerQuotaWindow(t *testing.T) {
	home := t.TempDir()
	// Cheap window: 1000 tokens per point. Expensive window: 100 per point.
	rollout(t, home, "eeee", "/work/cheap", "2026-09-09T09:00:00Z",
		call("2026-09-09T10:00:00Z", 1000, 1000, 0, "pro", 10, 1789437323),
		call("2026-09-09T10:30:00Z", 11000, 10000, 0, "pro", 20, 1789437323),
	)
	rollout(t, home, "ffff", "/work/small", "2026-09-09T11:00:00Z",
		call("2026-09-09T11:10:00Z", 100, 100, 0, "prolite", 1, 1789549216),
		call("2026-09-09T11:20:00Z", 1100, 1000, 0, "prolite", 11, 1789549216),
	)
	report, err := Run(context.Background(), env(t, home, nil), Options{Since: since(t, "2026-09-09T00:00:00Z")})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Plans) != 2 {
		t.Fatalf("plans = %d, want one per quota window", len(report.Plans))
	}
	byCWD := map[string]SessionReport{}
	for _, s := range report.Sessions {
		byCWD[s.CWD] = s
	}
	// 11000 tokens at 1100/pp = 10pp; 1100 tokens at 110/pp = 10pp. The bigger
	// thread is 10x the tokens and exactly the same share of its own limit.
	if got := byCWD["/work/cheap"].Points; got < 9.5 || got > 10.5 {
		t.Fatalf("cheap-window points = %.2f, want ~10", got)
	}
	if got := byCWD["/work/small"].Points; got < 9.5 || got > 10.5 {
		t.Fatalf("small-window points = %.2f, want ~10", got)
	}
}

// A rollout is mostly transcript, and single lines run past any sane read
// buffer. Usage events sitting after one of those lines must still be found.
func TestOversizedTranscriptLinesDoNotHideLaterCalls(t *testing.T) {
	home := t.TempDir()
	rollout(t, home, "gggg", "/work/one", "2026-09-09T09:00:00Z",
		bulk("2026-09-09T09:59:00Z"),
		call("2026-09-09T10:00:00Z", 100, 100, 0, "pro", 10, 1789437323),
		bulk("2026-09-09T10:00:30Z"),
		call("2026-09-09T10:01:00Z", 400, 300, 0, "pro", 12, 1789437323),
	)
	report, err := Run(context.Background(), env(t, home, nil), Options{Since: since(t, "2026-09-09T00:00:00Z")})
	if err != nil {
		t.Fatal(err)
	}
	if report.Calls != 2 || report.Usage.Input != 400 {
		t.Fatalf("calls=%d input=%d, want 2 calls of 400", report.Calls, report.Usage.Input)
	}
}

// A cached Run reads only appended bytes yet must answer what a fresh Run
// would: the dedupe total carries across reads, a later window drops cached
// calls, and a replaced file is read anew.
func TestCachedRunsMatchFreshRuns(t *testing.T) {
	home := t.TempDir()
	path := rollout(t, home, "hhhh", "/work/one", "2026-09-09T09:00:00Z",
		call("2026-09-09T10:00:00Z", 100, 100, 0, "pro", 10, 1789437323))
	e := env(t, home, nil)
	e.Cache = &Cache{}
	run := func(from string) Report {
		t.Helper()
		report, err := Run(context.Background(), e, Options{Since: since(t, from)})
		if err != nil {
			t.Fatal(err)
		}
		return report
	}
	if r := run("2026-09-09T09:00:00Z"); r.Calls != 1 {
		t.Fatalf("first read: calls=%d, want 1", r.Calls)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString(call("2026-09-09T10:05:00Z", 400, 300, 0, "pro", 12, 1789437323) + "\n" +
		call("2026-09-09T10:06:00Z", 400, 300, 0, "pro", 13, 1789437323) + "\n")
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if r := run("2026-09-09T09:00:00Z"); r.Calls != 2 || r.Usage.Input != 400 {
		t.Fatalf("after append: calls=%d input=%d, want 2 calls of 400 (the repeat must not bill)", r.Calls, r.Usage.Input)
	}
	if r := run("2026-09-09T10:02:00Z"); r.Calls != 1 || r.Usage.Input != 300 {
		t.Fatalf("later window: calls=%d input=%d, want 1 call of 300", r.Calls, r.Usage.Input)
	}
	rollout(t, home, "hhhh", "/work/two", "2026-09-09T09:00:00Z",
		call("2026-09-09T11:00:00Z", 50, 50, 0, "pro", 14, 1789437323))
	if r := run("2026-09-09T09:00:00Z"); r.Calls != 1 || r.Usage.Input != 50 || r.Sessions[0].CWD != "/work/two" {
		t.Fatalf("replaced file: calls=%d input=%d sessions=%+v, want the new file's one call", r.Calls, r.Usage.Input, r.Sessions)
	}
	// Replaced again by a file whose first record is still being written:
	// Scan hands over no line at all, yet the old calls must not survive.
	if err := os.WriteFile(path, []byte(`{"timestamp":"2026-09-09T12:00:00Z","type":"sess`), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := run("2026-09-09T09:00:00Z"); r.Calls != 0 {
		t.Fatalf("unfinished replacement: calls=%d, want 0", r.Calls)
	}
}

// session_meta carries the base instructions and can outgrow any record
// limit; the thread must still be identified.
func TestOversizedSessionMetaStillIdentifiesTheThread(t *testing.T) {
	home := t.TempDir()
	path := rollout(t, home, "iiii", "/work/one", "2026-09-09T09:00:00Z",
		call("2026-09-09T10:00:00Z", 100, 100, 0, "pro", 10, 1789437323))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	padded := strings.Replace(string(raw), "padding ", strings.Repeat("padding ", 2_200_000), 1)
	if err := os.WriteFile(path, []byte(padded), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := Run(context.Background(), env(t, home, nil), Options{Since: since(t, "2026-09-09T00:00:00Z")})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Sessions) != 1 || report.Sessions[0].ID != "iiii" || report.Sessions[0].CWD != "/work/one" || report.Calls != 1 {
		t.Fatalf("sessions=%+v calls=%d, want thread iiii in /work/one with its call", report.Sessions, report.Calls)
	}
}

// Process inspection is an enrichment. When it fails the spend numbers still
// have to land, because a burning limit is answerable from files alone.
func TestBrokenProcessInspectionStillReportsSpend(t *testing.T) {
	home := t.TempDir()
	rollout(t, home, "hhhh", "/work/one", "2026-09-09T09:00:00Z",
		call("2026-09-09T10:00:00Z", 500, 500, 0, "pro", 10, 1789437323))
	broken := func(context.Context, string, ...string) ([]byte, error) { return nil, fmt.Errorf("lsof: not found") }

	report, err := Run(context.Background(), env(t, home, broken), Options{Since: since(t, "2026-09-09T00:00:00Z")})
	if err != nil {
		t.Fatalf("process failure must not fail the report: %v", err)
	}
	if report.Usage.Input != 500 {
		t.Fatalf("input = %d, want 500", report.Usage.Input)
	}
	if len(report.Warnings) == 0 {
		t.Fatal("a degraded report must say so")
	}
}

// The derived figures are the point of the tool: Codex publishes a percentage,
// never an allowance or a deadline.
func TestPlanDerivesAllowanceAndRunway(t *testing.T) {
	plan := PlanReport{
		Usage:        Usage{Input: 48_000_000, Output: 2_000_000},
		StartPercent: 20, EndPercent: 40, Points: 20,
		Elapsed: 2 * time.Hour,
	}
	if got := plan.TokensPerPoint(); got != 2_500_000 {
		t.Fatalf("tokens/pp = %d, want 2.5M", got)
	}
	if got := plan.Allowance(); got != 250_000_000 {
		t.Fatalf("allowance = %d, want 250M", got)
	}
	if got := plan.PointsPerHour(); got != 10 {
		t.Fatalf("pp/h = %v, want 10", got)
	}
	runway, ok := plan.Runway()
	if !ok || runway != 6*time.Hour {
		t.Fatalf("runway = %v (%v), want 6h", runway, ok)
	}
	if _, ok := (PlanReport{EndPercent: 100, Points: 20, Elapsed: time.Hour}).Runway(); ok {
		t.Fatal("an exhausted quota has no runway to report")
	}
}

// Cache reads are a subset of input, never a sibling; adding them to input
// double-counts every hit and inflates exactly the well-cached threads.
func TestCachedTokensAreASubsetOfInput(t *testing.T) {
	u := Usage{Input: 1000, Cached: 900, Output: 100}
	if u.Fresh() != 100 {
		t.Fatalf("fresh = %d, want 100", u.Fresh())
	}
	if u.Total() != 1100 {
		t.Fatalf("total = %d, want 1100 (input already contains cached)", u.Total())
	}
	if u.CacheRate() != 90 {
		t.Fatalf("cache rate = %v, want 90", u.CacheRate())
	}
}

func TestParseSinceAcceptsTheWaysPeopleSayIt(t *testing.T) {
	now := time.Date(2026, 9, 9, 14, 30, 0, 0, time.Local)
	cases := map[string]time.Time{
		"":           time.Date(2026, 9, 9, 0, 0, 0, 0, time.Local),
		"today":      time.Date(2026, 9, 9, 0, 0, 0, 0, time.Local),
		"yesterday":  time.Date(2026, 9, 8, 0, 0, 0, 0, time.Local),
		"7am":        time.Date(2026, 9, 9, 7, 0, 0, 0, time.Local),
		"07:00":      time.Date(2026, 9, 9, 7, 0, 0, 0, time.Local),
		"3h":         time.Date(2026, 9, 9, 11, 30, 0, 0, time.Local),
		"2026-09-01": time.Date(2026, 9, 1, 0, 0, 0, 0, time.Local),
	}
	for input, want := range cases {
		got, err := ParseSince(input, now)
		if err != nil {
			t.Fatalf("%q: %v", input, err)
		}
		if !got.Equal(want) {
			t.Fatalf("%q = %v, want %v", input, got, want)
		}
	}
	if _, err := ParseSince("half past whenever", now); err == nil {
		t.Fatal("an unparseable window must be refused, not silently defaulted")
	}
}

// fakeProcs answers ps and lsof the way macOS does, so process rollup can be
// tested without live Codex sessions.
func fakeProcs(byPID map[int]string) func(context.Context, string, ...string) ([]byte, error) {
	return func(_ context.Context, name string, _ ...string) ([]byte, error) {
		var out strings.Builder
		switch name {
		case "ps":
			for pid := range byPID {
				fmt.Fprintf(&out, "%d ttys004 Wed Sep 9 11:07:02 2026 codex --dangerously-bypass-approvals-and-sandbox\n", pid)
			}
			// A ChatGPT.app helper must not be mistaken for the CLI.
			out.WriteString("999 ?? Wed Sep 9 10:00:00 2026 /Applications/ChatGPT.app/Contents/MacOS/Codex (Service) --type=utility\n")
		case "lsof":
			for pid, files := range byPID {
				fmt.Fprintf(&out, "p%d\n", pid)
				for _, file := range strings.Split(files, "\n") {
					fmt.Fprintf(&out, "f3\nn%s\n", file)
				}
			}
		}
		return []byte(out.String()), nil
	}
}

// A live thread that spent nothing is worth naming — it is a tab to close
// before it wakes up — but only when asked for, and only while its process
// lives. A thread whose process is gone and whose spend is outside the window
// is not news and must not pad the report.
func TestIdleThreadsAreListedOnlyWhenLiveAndRequested(t *testing.T) {
	home := t.TempDir()
	spending := rollout(t, home, "iiii", "/work/busy", "2026-09-09T09:00:00Z",
		call("2026-09-09T10:00:00Z", 500, 500, 0, "pro", 20, 1789437323))
	parked := rollout(t, home, "jjjj", "/work/parked", "2026-09-08T09:00:00Z",
		call("2026-09-08T10:00:00Z", 500, 500, 0, "pro", 5, 1789437323))
	rollout(t, home, "kkkk", "/work/ended", "2026-09-08T09:00:00Z",
		call("2026-09-08T11:00:00Z", 500, 500, 0, "pro", 6, 1789437323))

	procs := fakeProcs(map[int]string{4242: spending, 4343: parked})
	opts := Options{Since: since(t, "2026-09-09T00:00:00Z")}

	report, err := Run(context.Background(), env(t, home, procs), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Idle) != 0 {
		t.Fatalf("idle rows = %d, want none until --idle is asked for", len(report.Idle))
	}

	opts.IncludeIdle = true
	report, err = Run(context.Background(), env(t, home, procs), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Sessions) != 1 || report.Sessions[0].CWD != "/work/busy" {
		t.Fatalf("spending rows = %+v, want only /work/busy", report.Sessions)
	}
	if len(report.Idle) != 1 || report.Idle[0].CWD != "/work/parked" {
		t.Fatalf("idle rows = %+v, want only the live parked thread", report.Idle)
	}
	if report.Usage.Total() != 500 {
		t.Fatalf("total = %d, want 500 — idle threads must not add spend", report.Usage.Total())
	}
}

// --top exists to keep the report readable on a busy day, so it has to cut the
// cheap tail. Truncating before ranking would drop exactly the threads worth
// seeing, which is the failure the report exists to prevent.
func TestTopKeepsTheBiggestSpenders(t *testing.T) {
	home := t.TempDir()
	rollout(t, home, "llll", "/work/small", "2026-09-09T09:00:00Z",
		call("2026-09-09T10:00:00Z", 100, 100, 0, "pro", 10, 1789437323))
	rollout(t, home, "mmmm", "/work/huge", "2026-09-09T09:10:00Z",
		call("2026-09-09T10:10:00Z", 9000, 9000, 0, "pro", 20, 1789437323))
	rollout(t, home, "nnnn", "/work/middle", "2026-09-09T09:20:00Z",
		call("2026-09-09T10:20:00Z", 3000, 3000, 0, "pro", 24, 1789437323))

	report, err := Run(context.Background(), env(t, home, nil), Options{Since: since(t, "2026-09-09T00:00:00Z"), Top: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Sessions) != 2 {
		t.Fatalf("rows = %d, want 2", len(report.Sessions))
	}
	if report.Sessions[0].CWD != "/work/huge" || report.Sessions[1].CWD != "/work/middle" {
		t.Fatalf("rows = %q, %q; want the two biggest, ranked", report.Sessions[0].CWD, report.Sessions[1].CWD)
	}
	// The headline still accounts for every thread, including the cut tail.
	if report.Usage.Total() != 12100 || report.Calls != 3 {
		t.Fatalf("totals = %d tokens / %d calls, want 12100/3 — truncation must not lose spend", report.Usage.Total(), report.Calls)
	}
}
