package claudeguards

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// denyLiteral matches a complete id; a table prefix (`deny("destructive:"+…`)
// ends in a colon and is expanded from its table instead.
var denyLiteral = regexp.MustCompile(`deny\("([a-z0-9:-]*[a-z0-9])"`)

// sourceRuleIDs collects every rule id the package can emit: literal deny()
// ids from the non-test sources, plus the table-driven destructive:* and
// secrets:* families expanded from their runtime tables.
func sourceRuleIDs(t *testing.T) map[string]bool {
	t.Helper()
	ids := map[string]bool{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range denyLiteral.FindAllStringSubmatch(string(src), -1) {
			ids[m[1]] = true
		}
	}
	for _, r := range destructiveRules {
		ids["destructive:"+r.name] = true
	}
	for _, cmd := range []string{"env", "docker inspect -f '{{.Config.Env}}' app", "cat /proc/1/environ"} {
		name := envDumpMatch(cmd)
		if name == "" {
			t.Fatalf("envDumpMatch(%q) matched nothing; the secrets table changed shape", cmd)
		}
		ids["secrets:"+name] = true
	}
	for _, cmd := range []string{"printenv MAIL_PASSWORD", "echo ${#MAIL_PASSWORD}", `echo "$API_TOKEN"`} {
		name := secretPrintMatch(cmd)
		if name == "" {
			t.Fatalf("secretPrintMatch(%q) matched nothing; the secrets table changed shape", cmd)
		}
		ids["secrets:"+name] = true
	}
	return ids
}

func TestRegistryMatchesSources(t *testing.T) {
	want := sourceRuleIDs(t)
	got := map[string]bool{}
	for _, r := range Rules {
		if got[r.ID] {
			t.Errorf("duplicate registry id %q", r.ID)
		}
		got[r.ID] = true
		if r.Family != strings.SplitN(r.ID, ":", 2)[0] {
			t.Errorf("%s: family %q does not match the id prefix", r.ID, r.Family)
		}
		if r.Summary == "" || r.Escape == "" || r.Event == "" {
			t.Errorf("%s: every column must be filled", r.ID)
		}
	}
	for id := range want {
		if !got[id] {
			t.Errorf("source emits %q but the registry lacks it", id)
		}
	}
	for id := range got {
		if !want[id] {
			t.Errorf("registry lists %q but no source emits it", id)
		}
	}
	ids := make([]string, 0, len(Rules))
	for _, r := range Rules {
		ids = append(ids, r.ID)
	}
	if !sort.StringsAreSorted(ids) {
		t.Error("Rules must stay sorted by id")
	}
}

func TestRenderRules(t *testing.T) {
	var text bytes.Buffer
	if err := RenderRules(&text, "machine", false); err != nil {
		t.Fatal(err)
	}
	out := text.String()
	for _, want := range []string{"RULE", "machine:test-worker-cap", "CLAUDE_GUARDS_ALLOW_TEST_WORKERS=1", "machine:devbox-only"} {
		if !strings.Contains(out, want) {
			t.Errorf("text output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "destructive:") {
		t.Errorf("family filter leaked another family:\n%s", out)
	}
	var js bytes.Buffer
	if err := RenderRules(&js, "", true); err != nil {
		t.Fatal(err)
	}
	var rows []Rule
	if err := json.Unmarshal(js.Bytes(), &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(Rules) {
		t.Errorf("json rows %d != %d rules", len(rows), len(Rules))
	}
	if err := RenderRules(&text, "nope", false); err == nil || !strings.Contains(err.Error(), "machine") {
		t.Errorf("unknown family must name the known ones, got %v", err)
	}
}
