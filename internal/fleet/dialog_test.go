package fleet

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func screenFixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "screens", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestParseDialogReadsAPermissionPrompt(t *testing.T) {
	d := ParseDialog(screenFixture(t, "permission-bash.txt"))
	if d == nil {
		t.Fatal("no dialog parsed")
	}
	if d.Kind != DialogPermission || d.Title != "Bash command" || d.Question != "Do you want to proceed?" {
		t.Fatalf("frame = %q %q %q", d.Kind, d.Title, d.Question)
	}
	if !strings.Contains(d.Detail, "touch /tmp/fleet-5092/scratch/x.txt && echo done") || strings.Contains(d.Detail, "Tip:") {
		t.Fatalf("detail = %q", d.Detail)
	}
	if len(d.Options) != 4 || !d.Options[0].Selected || d.Options[3].Label != "No" || !d.Answerable {
		t.Fatalf("options = %+v", d.Options)
	}
	for _, o := range d.Options {
		if o.Kind != OptionAnswer {
			t.Fatalf("option %s is %s", o.Key, o.Kind)
		}
	}
}

func TestParseDialogReadsAQuestionWithTabsAndOptionKinds(t *testing.T) {
	d := ParseDialog(screenFixture(t, "question-tabs.txt"))
	if d == nil || d.Kind != DialogQuestion {
		t.Fatalf("dialog = %+v", d)
	}
	if d.Question != "When should the acceptance run happen? It loads the staging box for ~15 min, and the rig is still up." {
		t.Fatalf("question = %q", d.Question)
	}
	if len(d.Tabs) != 2 || d.Tabs[0].Label != "Run window" || d.Tabs[0].Done {
		t.Fatalf("tabs = %+v", d.Tabs)
	}
	want := []OptionKind{OptionAnswer, OptionAnswer, OptionOther, OptionChat}
	if len(d.Options) != len(want) {
		t.Fatalf("options = %+v", d.Options)
	}
	for i, kind := range want {
		if d.Options[i].Kind != kind {
			t.Fatalf("option %d kind %s, want %s", i+1, d.Options[i].Kind, kind)
		}
	}
	if d.Options[1].Description != "Run the acceptance in the off-peak window." {
		t.Fatalf("description = %q", d.Options[1].Description)
	}
	if _, ok := d.Option("3"); ok {
		t.Fatal(`"Type something." must not be an answer key`)
	}
}

func TestParseDialogNeedsTheFooterAtTheBottom(t *testing.T) {
	// An answered dialog in scrollback, the prompt below it: nothing is open.
	if d := ParseDialog(screenFixture(t, "idle-prompt.txt")); d != nil {
		t.Fatalf("idle screen parsed as %+v", d)
	}
	if d := ParseDialog(screenFixture(t, "question-multiselect.txt")); d == nil || d.Answerable {
		t.Fatalf("multi-select = %+v, want unanswerable", d)
	}
}

func TestDialogFingerprintNamesTheExactPrompt(t *testing.T) {
	screen := screenFixture(t, "permission-bash.txt")
	a := ParseDialog(screen)
	b := ParseDialog(strings.Replace(screen, " touch /tmp/fleet-5092/scratch/x.txt && echo done\n╌", " rm -rf /tmp/fleet-5092/scratch\n╌", 1))
	if a.Fingerprint == "" || a.Fingerprint == b.Fingerprint {
		t.Fatalf("fingerprints %q %q must differ for another command", a.Fingerprint, b.Fingerprint)
	}
}

func TestAtPromptAcceptsOnlyTheConversationsOwnInputBox(t *testing.T) {
	if !AtPrompt(screenFixture(t, "idle-prompt.txt")) {
		t.Fatal("the idle prompt is a prompt")
	}
	// The agents view draws the same box, but its input starts a new session.
	if AtPrompt(screenFixture(t, "agents-view.txt")) {
		t.Fatal("the agents view must not take a reply")
	}
	if AtPrompt(screenFixture(t, "permission-bash.txt")) {
		t.Fatal("a dialog is not a prompt")
	}
}

func TestDialogFingerprintCoversALongCommandWhole(t *testing.T) {
	long := strings.Repeat(" echo step\n", 30)
	screen := strings.Replace(screenFixture(t, "permission-bash.txt"), "\n touch /tmp/fleet-5092/scratch/x.txt && echo done\n╌", "\n rm -rf /tmp/a\n"+long+"╌", 1)
	edited := strings.Replace(screen, " rm -rf /tmp/a\n", " rm -rf /tmp/b\n", 1)
	a, b := ParseDialog(screen), ParseDialog(edited)
	if a == nil || b == nil || a.Fingerprint == b.Fingerprint {
		t.Fatalf("an edit 30 lines above the question must change the fingerprint (%v %v)", a, b)
	}
	if !strings.Contains(a.Detail, "rm -rf /tmp/a") {
		t.Fatalf("detail cut the command's first line: %q", a.Detail[:60])
	}
}
