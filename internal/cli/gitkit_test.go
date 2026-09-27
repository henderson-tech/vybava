package cli

import (
	"bytes"
	"strings"
	"testing"
)

// --help is the one question a strict argv must always answer: usage on
// stdout, exit 0, never a GITKIT_BAD_ARGS refusal (108 of them in the three
// days to 2026-09-27). github-io answers with its subcommand's own line.
func TestGitkitHelpIsNeverARefusal(t *testing.T) {
	for _, tc := range []struct{ args, want []string }{
		{[]string{"merge-precheck", "--help"}, []string{"usage: vybava gitkit merge-precheck"}},
		{[]string{"before-review", "-h"}, []string{"usage: vybava gitkit before-review"}},
		{[]string{"github-io", "find-run", "--help"}, []string{"usage: vybava gitkit github-io find-run", "--repo /abs/checkout"}},
		{[]string{"github-io", "--help"}, []string{"github-io — mechanical GitHub I/O"}},
	} {
		var stdout, stderr bytes.Buffer
		rt := &runtime{stdout: &stdout, stderr: &stderr}
		cmd := rt.gitkitCommand("gitkit")
		cmd.SetArgs(tc.args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("%v: %v (stderr %q)", tc.args, err, stderr.String())
		}
		for _, w := range tc.want {
			if !strings.Contains(stdout.String(), w) {
				t.Errorf("%v: stdout %q lacks %q", tc.args, stdout.String(), w)
			}
		}
		if stderr.Len() != 0 {
			t.Errorf("%v: stderr %q, want empty", tc.args, stderr.String())
		}
	}
}
