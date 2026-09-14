package claudeguards

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture writes an n-line file under a non-throwaway root and points the
// transcript root at the same tree so every rule is exercisable offline.
func fixture(t *testing.T) (root string, small, big, transcript string) {
	t.Helper()
	root = t.TempDir()
	saveRoots, saveTranscript := throwawayRoots, transcriptRoot
	throwawayRoots, transcriptRoot = nil, filepath.Join(root, "projects")
	t.Cleanup(func() { throwawayRoots, transcriptRoot = saveRoots, saveTranscript })
	small = filepath.Join(root, "small.ts")
	big = filepath.Join(root, "big.ts")
	transcript = filepath.Join(root, "projects", "slug", "abc.jsonl")
	if err := os.MkdirAll(filepath.Dir(transcript), 0o755); err != nil {
		t.Fatal(err)
	}
	for p, n := range map[string]int{small: 50, big: 995, transcript: 3} {
		if err := os.WriteFile(p, []byte(strings.Repeat("line\n", n)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, small, big, transcript
}

func TestContextBashMatch(t *testing.T) {
	root, small, big, transcript := fixture(t)
	cases := []struct {
		name string
		cmd  string
		want string // rule, "" = pass
	}{
		// --- inline-script-write ---
		{"python heredoc patch", "python3 - <<'EOF'\np='a.ts'\ns=open(p).read()\nopen(p,'w').write(s.replace('a','b'))\nEOF", "context:inline-script-write"},
		{"python -c write_text", `python3 -c "from pathlib import Path; Path('x').write_text('y')"`, "context:inline-script-write"},
		{"node -e writeFileSync", `node -e "require('fs').writeFileSync('x.json', JSON.stringify({}))"`, "context:inline-script-write"},
		{"python heredoc read-only ok", "python3 - <<'EOF'\nimport json\nprint(len(json.load(open('cs.json'))))\nEOF", ""},
		{"node -e read-only ok", `node -e "console.log(require('./package.json').version)"`, ""},
		{"python script by path ok", "python3 /tmp/patch.py", ""},
		{"escape hatch", "CLAUDE_ALLOW_SHELL_EDIT=1 python3 - <<'EOF'\nopen('x','w').write('y')\nEOF", ""},

		// --- heredoc-overwrite ---
		{"cat > existing", "cat > " + small + " <<'EOF'\nnew\nEOF", "context:heredoc-overwrite"},
		{"cat heredoc then redirect existing", "cat <<'EOF' > " + small + "\nnew\nEOF", "context:heredoc-overwrite"},
		{"tee existing", "echo x | tee " + small, "context:heredoc-overwrite"},
		{"tee -a ok", "echo x | tee -a " + small, ""},
		{"append ok", "cat >> " + small + " <<'EOF'\nmore\nEOF", ""},
		{"new file ok", "cat > " + filepath.Join(root, "fresh.ts") + " <<'EOF'\nnew\nEOF", ""},
		{"tmp target ok", "cat > /tmp/scratch.js <<'EOF'\nnew\nEOF", ""},
		{"escape hatch", "CLAUDE_ALLOW_SHELL_EDIT=1 cat > " + small + " <<'EOF'\nnew\nEOF", ""},

		// --- whole-file-dump ---
		{"cat big", "cat " + big, "context:whole-file-dump"},
		{"cat small ok", "cat " + small, ""},
		{"cat several small over budget", "cat " + small + " " + small + " " + small + " " + small + " " + small, "context:whole-file-dump"},
		{"cat big piped ok", "cat " + big + " | grep foo", ""},
		{"cat big redirected ok", "cat " + big + " > /tmp/copy.ts", ""},
		{"cat big then chained", "cat " + big + " && echo done", "context:whole-file-dump"},
		{"sed range in budget ok", "sed -n '120,180p' " + big, ""},
		{"sed range over budget", "sed -n '1,400p' " + big, "context:whole-file-dump"},
		{"sed to end", "sed -n '500,$p' " + big, "context:whole-file-dump"},
		{"sed several ranges ok", "sed -n '1,60p;400,460p' " + big, ""},
		{"sed -e ranges", "sed -n -e '1,150p' -e '300,400p' " + big, "context:whole-file-dump"},
		{"sed substitute prints nothing counted", "sed -n 's/a/b/p' " + big, ""},
		{"sed -i ok", "sed -i '' 's/a/b/' " + big, ""},
		{"sed range without -n is a whole-file read", "sed '1,60p' " + big, "context:whole-file-dump"},
		{"head default ok", "head " + big, ""},
		{"head -n 300", "head -n 300 " + big, "context:whole-file-dump"},
		{"head -300", "head -300 " + big, "context:whole-file-dump"},
		{"head -n 100 ok", "head -n 100 " + big, ""},
		{"tail -n 500", "tail -n 500 " + big, "context:whole-file-dump"},
		{"missing file ok", "cat " + filepath.Join(root, "nope.ts"), ""},
		{"escape hatch", "CLAUDE_ALLOW_CONTEXT_DUMP=1 cat " + big, ""},
		{"cat in quoted heredoc body ok", "cat > /tmp/x.sh <<'EOF'\ncat " + big + "\nEOF", ""},
		{"echo mentions cat ok", "echo cat " + big, ""},

		// --- transcript-dump ---
		{"cat transcript", "cat " + transcript, "context:transcript-dump"},
		{"sed transcript", "sed -n '1,5p' " + transcript, "context:transcript-dump"},
		{"transcript piped into jq ok", "cat " + transcript + " | jq -c .type", ""},
		{"node over transcript ok", "node /tmp/agg.js " + transcript, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ""
			if d := contextBashMatch(c.cmd, root); d != nil {
				got = d.Rule
			}
			if got != c.want {
				t.Fatalf("cmd %q: got %q want %q", c.cmd, got, c.want)
			}
		})
	}
}

func TestContextReadMatch(t *testing.T) {
	root, small, big, transcript := fixture(t)
	cases := []struct {
		name  string
		path  string
		limit int
		want  string
	}{
		{"big without limit", big, 0, "context:whole-file-dump"},
		{"big with limit", big, 120, ""},
		{"small", small, 0, ""},
		{"relative path", "big.ts", 0, "context:whole-file-dump"},
		{"missing", filepath.Join(root, "nope.ts"), 0, ""},
		{"png ignored", filepath.Join(root, "shot.png"), 0, ""},
		{"transcript", transcript, 0, "context:transcript-dump"},
		{"transcript even with limit", transcript, 10, "context:transcript-dump"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ""
			if d := contextReadMatch(c.path, c.limit, root); d != nil {
				got = d.Rule
			}
			if got != c.want {
				t.Fatalf("got %q want %q", got, c.want)
			}
		})
	}
}

func TestDenialText(t *testing.T) {
	d := deny("x:y", "why", "")
	if !strings.HasPrefix(d.Text(), "🚨 BLOCKED by claude-guards (x:y)") || strings.Contains(d.Text(), "\n\n\n") {
		t.Fatalf("unexpected text %q", d.Text())
	}
}

func TestLokCatalogRule(t *testing.T) {
	root, _, _, _ := fixture(t)
	cat := filepath.Join(root, "locales", "cs.json")
	if err := os.MkdirAll(filepath.Dir(cat), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cat, []byte("{\n  \"a\": \"b\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vybava.config.json"), []byte(`{"lok":{"catalogs":{"m":{"style":"english-as-key","files":"locales/{locale}.json","locales":["cs"]}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if d := contextBashMatch("cat "+cat, root); d == nil || d.Rule != "context:locale-catalog" {
		t.Fatalf("cat of a 3-line catalog must still block, got %v", d)
	}
	if d := contextBashMatch("sed -n '1,2p' "+cat, root); d == nil || d.Rule != "context:locale-catalog" {
		t.Fatalf("ranged read must block, got %v", d)
	}
	if d := contextReadMatch(cat, 50, root); d == nil || d.Rule != "context:locale-catalog" {
		t.Fatalf("Read with limit must block, got %v", d)
	}
	if d := contextBashMatch("cat "+cat+" | jq keys", root); d != nil {
		t.Fatalf("piped read stays allowed, got %v", d)
	}
	if d := contextBashMatch("CLAUDE_ALLOW_CONTEXT_DUMP=1 cat "+cat, root); d != nil {
		t.Fatalf("escape hatch, got %v", d)
	}
}

// Files past the 4 MiB read cap report a sentinel line count. A sentinel near
// maxInt summed to a negative total once three of them appeared on one command
// line, so the guard allowed exactly the dump it exists to stop.
func TestDumpBudgetSumsUnmeasuredFilesWithoutWrapping(t *testing.T) {
	root := t.TempDir()
	var paths []string
	for _, name := range []string{"a.log", "b.log", "c.log"} {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte(strings.Repeat("x\n", 2<<20)), 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	d := contextBashMatch("cat "+strings.Join(paths, " "), root)
	if d == nil || d.Rule != "context:whole-file-dump" {
		t.Fatalf("three unmeasured files must stay denied, got %v", d)
	}
	if strings.Contains(d.Message, "4611686018427387903") {
		t.Fatalf("sentinel leaked into the message: %s", d.Message)
	}
}

// A pipe is only an exemption when the downstream command shrinks its input.
func TestPipeExemptionNeedsAReducingSink(t *testing.T) {
	root := t.TempDir()
	big := filepath.Join(root, "big.txt")
	if err := os.WriteFile(big, []byte(strings.Repeat("x\n", 500)), 0600); err != nil {
		t.Fatal(err)
	}
	if d := contextBashMatch("cat "+big+" | jq .", root); d != nil {
		t.Fatalf("a reducing sink stays allowed, got %v", d)
	}
	if d := contextBashMatch("cat "+big+" | cat", root); d == nil || d.Rule != "context:whole-file-dump" {
		t.Fatalf("| cat reproduces the file whole and must be denied, got %v", d)
	}
	// None of these bound anything — `sort` and `sed -n p` reproduce every
	// input line, and an interpreter can do whatever it likes.
	for _, sink := range []string{"sort", "uniq", "awk '{print}'", "sed -n p", "tee /tmp/x", "python3 -", "xargs -I{} echo {}"} {
		if d := contextBashMatch("cat "+big+" | "+sink, root); d == nil {
			t.Fatalf("| %s does not bound its input and must not exempt the read", sink)
		}
	}
	for _, sink := range []string{"head -20", "tail -5", "wc -l", "grep needle", "jq ."} {
		if d := contextBashMatch("cat "+big+" | "+sink, root); d != nil {
			t.Fatalf("| %s bounds or queries and must stay allowed, got %v", sink, d)
		}
	}
	// head and tail are sinks only when their own limit says so.
	for _, sink := range []string{
		"head -1000", "head -n 1000000", "tail -n +1", "tail -n 5000",
		"head -c 100000000", "head -c100M", "tail -c +1", // a byte cap is only a cap if it is small
	} {
		if d := contextBashMatch("cat "+big+" | "+sink, root); d == nil {
			t.Fatalf("| %s delivers more than the budget and must not exempt the read", sink)
		}
	}
	for _, sink := range []string{"head -c 2000", "head -c4k"} {
		if d := contextBashMatch("cat "+big+" | "+sink, root); d != nil {
			t.Fatalf("| %s is a genuine cap and must stay allowed, got %v", sink, d)
		}
	}
}

// An inclusive `A,A+200` range delivers 201 lines, so the round window an agent
// reaches for when told to read in 200-line slices used to be denied. Field
// audit of post-2026-09-13 transcripts: 84 firings on already-bounded sed reads,
// 13 at exactly 201 lines, 12 of those this shape.
func TestSedWindowGetsTheInclusiveEndpoint(t *testing.T) {
	_, _, big, _ := fixture(t) // 995 lines
	root := filepath.Dir(big)
	// The round windows an agent actually writes: 201 lines each.
	for _, cmd := range []string{"sed -n 200,400p ", "sed -n 400,600p ", "sed -n 100,300p "} {
		if d := contextBashMatch(cmd+big, root); d != nil {
			t.Fatalf("%s is one stated 200-line window and must be allowed, got %v", cmd, d)
		}
	}
	// The allowance is one line per read, not one per range or per file.
	for _, cmd := range []string{
		"sed -n 1,900p " + big,             // genuinely oversized
		"sed -n '1,201p;400,600p' " + big,  // two ranges in one read
		"sed -n 1,202p " + big,             // one line past the window
		"sed -n 1,201p " + big + " " + big, // one window, two files
	} {
		if d := contextBashMatch(cmd, root); d == nil || d.Rule != "context:whole-file-dump" {
			t.Fatalf("%s exceeds the window allowance and must be denied, got %v", cmd, d)
		}
	}
	// An unbounded read is still measured at exactly maxDumpLines.
	if d := contextBashMatch("head -201 "+big, root); d == nil {
		t.Fatalf("the allowance is for stated sed windows only, not every reader")
	}
	// Separate commands are budgeted separately, as they always have been: the
	// allowance does not change that, it only shifts one read's own ceiling.
	if d := contextBashMatch("sed -n 200,400p "+big+"; sed -n 400,600p "+big, root); d != nil {
		t.Fatalf("two separate windowed reads stay two reads, got %v", d)
	}
}

// A denial is what teaches the next attempt, so it has to describe the read
// that was actually made. Both shapes below were wrong in the field: a ranged
// Read was told it passed no offset/limit, and was quoted the tail length as
// the file's length.
func TestReadDenialDescribesTheReadAttempted(t *testing.T) {
	root, _, big, _ := fixture(t) // big is 995 lines
	d := contextReadMatchCfg(big, 400, 596, root, guardConfig(root))
	if d == nil {
		t.Fatal("596 lines from offset 400 is over budget and must be denied")
	}
	if strings.Contains(d.Message, "without offset/limit") {
		t.Fatalf("a Read that passed both must not be told it passed neither: %q", d.Message)
	}
	for _, want := range []string{"995 lines", "offset=400, limit=596", "596 of them"} {
		if !strings.Contains(d.Message, want) {
			t.Fatalf("message must state %q, got %q", want, d.Message)
		}
	}
	// A limit smaller than the tail is what lands, and is the number to quote.
	d = contextReadMatchCfg(big, 2, 400, root, guardConfig(root))
	if d == nil || !strings.Contains(d.Message, "400 of them") {
		t.Fatalf("the limit bounds what lands and must be quoted: %v", d)
	}
	// The plain case keeps its original wording.
	d = contextReadMatchCfg(big, 0, 0, root, guardConfig(root))
	if d == nil || !strings.Contains(d.Message, "without offset/limit") {
		t.Fatalf("a bare Read is still described as one: %v", d)
	}
}

// A near-miss window should be answered with the arithmetic, not just "read a
// range" — otherwise the next attempt overshoots the same way.
func TestNearMissWindowNamesTheFittingWindow(t *testing.T) {
	root, _, big, _ := fixture(t)
	d := contextBashMatch("sed -n 200,401p "+big, root) // 202 lines: one past the allowance
	if d == nil {
		t.Fatal("202 lines is over the allowance and must be denied")
	}
	if !strings.Contains(d.Message, "sed -n '200,400p'") {
		t.Fatalf("denial must name the fitting window, got %q", d.Message)
	}
	// A read that is not a single stated window gets no arithmetic hint.
	for _, cmd := range []string{"cat " + big, "sed -n 1,900p " + big, "sed -n '1,150p;400,600p' " + big} {
		if d := contextBashMatch(cmd, root); d == nil || strings.Contains(d.Message, "is the 201-line window") {
			t.Fatalf("%s is not a near-miss window and must get no hint: %v", cmd, d)
		}
	}
}

// A bound may sit at any stage of a pipeline, and a `-n` print-range sed is a
// bound. Field audit of post-2026-09-13 transcripts: all three shapes below
// were denied, and agents answered with CLAUDE_ALLOW_CONTEXT_DUMP=1.
func TestPipeExemptionScansWholePipeline(t *testing.T) {
	root := t.TempDir()
	big := filepath.Join(root, "big.txt")
	if err := os.WriteFile(big, []byte(strings.Repeat("x\n", 500)), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tail := range []string{
		"sed -n 1,60p",                // a print-range sed caps like head -60
		"awk '{print $1}' | head -20", // the bound is two stages downstream
		"tee /tmp/x | head -20",       // tee passes through; head still bounds
	} {
		if d := contextBashMatch("cat "+big+" | "+tail, root); d != nil {
			t.Fatalf("| %s bounds the read and must stay allowed, got %v", tail, d)
		}
	}
	// sed caps only when `-n` suppresses the default print and the script is
	// nothing but print ranges; an open or oversized range is no cap at all.
	for _, tail := range []string{
		"sed -n p",        // prints every line
		"sed '1,60p'",     // no -n: whole file, range repeated
		"sed -n 1,500p",   // wider than the budget
		"sed -n '60,$p'",  // open-ended
		"sed s/a/b/",      // not a print-range script
		"sed -n 1,60p x1", // a file operand: this sed reads, it does not sink
	} {
		if d := contextBashMatch("cat "+big+" | "+tail, root); d == nil {
			t.Fatalf("| %s does not bound its input and must not exempt the read", tail)
		}
	}
	// The walk stops at the pipeline boundary: a later command's bound must not
	// launder an earlier dump.
	for _, cmd := range []string{
		"cat " + big + " ; echo done | head -20",
		"cat " + big + " && git log --oneline | head -20",
	} {
		if d := contextBashMatch(cmd, root); d == nil || d.Rule != "context:whole-file-dump" {
			t.Fatalf("a `| head` in a later command must not launder %q, got %v", cmd, d)
		}
	}
}
