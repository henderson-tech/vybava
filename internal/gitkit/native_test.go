package gitkit

import (
	"strings"
	"testing"
	"time"
)

// execFile fails exactly as Node's execFileSync does, including the
// maxBuffer cap and a timeout that a pipe-holding grandchild cannot stall.
func TestExecFileFailuresMatchNode(t *testing.T) {
	var echo strings.Builder
	for _, tc := range []struct {
		opts execOpts
		name string
		args []string
		want string
	}{
		{execOpts{echo: &echo}, "sh", []string{"-c", "echo oops >&2; exit 3"}, "Command failed: sh -c echo oops >&2; exit 3\noops\n"},
		{execOpts{}, "definitely-not-a-command", nil, "spawnSync definitely-not-a-command ENOENT"},
		{execOpts{maxBuffer: 16}, "sh", []string{"-c", "printf '%040d' 0"}, "spawnSync sh ENOBUFS"},
		{execOpts{timeout: 200 * time.Millisecond}, "sh", []string{"-c", "sleep 30 & wait"}, "spawnSync sh ETIMEDOUT"},
	} {
		start := time.Now()
		_, err := execFile(tc.opts, tc.name, tc.args...)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s %v: err = %v, want %q", tc.name, tc.args, err, tc.want)
		}
		if time.Since(start) > 10*time.Second {
			t.Errorf("%s %v took %s", tc.name, tc.args, time.Since(start))
		}
	}
	if echo.String() != "oops\n" {
		t.Errorf("child stderr not echoed: %q", echo.String())
	}
}

func TestJSNumberAndPositiveInt(t *testing.T) {
	for s, want := range map[string]int{"7": 7, " 7 ": 7, "0x7": 7, "7.0": 7, "1e2": 100} {
		if got, ok := positiveInt(s); !ok || got != want {
			t.Errorf("positiveInt(%q) = %d, %v", s, got, ok)
		}
	}
	for _, s := range []string{"0", "-3", "7.5", "abc", "1_0", "Infinity"} {
		if _, ok := positiveInt(s); ok {
			t.Errorf("positiveInt(%q) must be rejected", s)
		}
	}
}
