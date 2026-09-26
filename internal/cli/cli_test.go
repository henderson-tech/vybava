package cli

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMulticallDispatch pins the multicall contract: invoking the binary under
// an applet's name yields that applet's command, anything else the vybava root.
func TestMulticallDispatch(t *testing.T) {
	cases := []struct{ invokedAs, wantName string }{
		{"memorylint", "memorylint"},
		{"fontfreeze", "fontfreeze"},
		{"perfrig", "perfrig"},
		{"shrt", "shrt"},
		{"press", "press"},
		{"ingressgen", "ingressgen"},
		{"reconcile", "reconcile"},
		{"handoffs", "handoffs"},
		{"operator", "operator"},
		{"plaud", "plaud"},
		{"claude-guards", "claude-guards"},
		{"lok", "lok"},
		{"merge-assist", "merge-assist"},
		{"memo", "memo"},
		{"tokentime", "tokentime"},
		{"redact", "redact"},
		{"/usr/local/bin/perfrig", "perfrig"}, // dispatch is on the basename
		{"vybava", "vybava"},
	}
	for _, c := range cases {
		cmd, err := (App{}).Command(c.invokedAs)
		if err != nil {
			t.Fatalf("Command(%q) error: %v", c.invokedAs, err)
		}
		if got := cmd.Name(); got != c.wantName {
			t.Errorf("Command(%q).Name() = %q, want %q", c.invokedAs, got, c.wantName)
		}
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ in, want string }{
		{"~", home},
		{"~/Exports/FixIt/perf/", filepath.Join(home, "Exports/FixIt/perf")},
		{"/abs/path", "/abs/path"},
		{"relative/path", "relative/path"},
		{"~user/x", "~user/x"}, // only the current user's ~ is expanded
	}
	for _, c := range cases {
		got, err := expandHome(c.in)
		if err != nil {
			t.Fatalf("expandHome(%q) error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("expandHome(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
