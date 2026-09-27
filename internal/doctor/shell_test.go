package doctor

import (
	"strings"
	"testing"
)

// The row warns only when the probe proves `nomatch` is set in the shell the
// harness uses, names the one-line fix, and passes when zsh cannot be asked.
func TestAgentShellGlobCheck(t *testing.T) {
	for name, tc := range map[string]struct {
		on, ok bool
		status Status
		remedy string
	}{
		"nomatch set":    {on: true, ok: true, status: StatusWarn, remedy: shellGlobRemedy},
		"nonomatch":      {on: false, ok: true, status: StatusPass},
		"zsh not probed": {ok: false, status: StatusPass},
	} {
		got := agentShellGlobCheck(func() (bool, bool) { return tc.on, tc.ok })
		if got.ID != "agent shell globs" || got.Status != tc.status || got.Remedy != tc.remedy {
			t.Errorf("%s: %+v", name, got)
		}
		if tc.status == StatusWarn && !strings.Contains(got.Message, "--include=*.ts") {
			t.Errorf("%s: the message should show the shape that fails: %q", name, got.Message)
		}
	}
}
