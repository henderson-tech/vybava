package doctor

import (
	"errors"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/henderson-tech/vybava/internal/state"
)

func TestModChecksReportEachInstalledModByValidateOutcome(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	installed := []state.Installed{
		{ItemID: "good", Kind: "mod", Destination: home},
		{ItemID: "broken", Kind: "mod", Destination: home},
		{ItemID: "unchecked", Kind: "mod", Destination: home},
		{ItemID: "gone", Kind: "mod", Destination: filepath.Join(home, "missing")},
		{ItemID: "memo", Kind: "applet", Destination: home},
	}
	failure := exec.Command("false").Run()
	outcomes := map[string]error{"good": nil, "broken": failure, "unchecked": errNoClaude}
	index := 0
	order := []string{"good", "broken", "unchecked"}
	checks := modChecks(installed, func(string) ([]byte, error) {
		id := order[index]
		index++
		return []byte("\n  ✘ hooks/register.ts: unknown event 'tool.cal'\n"), outcomes[id]
	})

	want := map[string]Status{"mod:good": StatusPass, "mod:broken": StatusFail, "mod:unchecked": StatusWarn}
	if len(checks) != len(want) {
		t.Fatalf("modChecks() = %+v, want %d checks (none for a missing folder or an applet)", checks, len(want))
	}
	for _, check := range checks {
		if check.Status != want[check.ID] {
			t.Errorf("%s status = %s, want %s", check.ID, check.Status, want[check.ID])
		}
	}
	if broken := checks[1]; broken.Message != "claude plugin validate failed: ✘ hooks/register.ts: unknown event 'tool.cal'" {
		t.Errorf("failed check message = %q, want the validate output's first line", broken.Message)
	}
	var exit *exec.ExitError
	if !errors.As(failure, &exit) {
		t.Fatal("fixture: `false` did not produce an exit error")
	}
}
