package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/henderson-tech/vybava/internal/runx"
)

// TestPerflabEnvelopeSurface walks every perflab verb through the multicall
// entry with an empty lab and no adapter: each prints exactly one envelope,
// its arrays are never null, a failure carries a diagnostic, and the exit
// code matches the outcome. No path here reaches a device or a build tool.
func TestPerflabEnvelopeSurface(t *testing.T) {
	t.Setenv("PERFLAB_STATE_DIR", t.TempDir())
	t.Setenv("PERFLAB_CACHE_DIR", t.TempDir())
	project := t.TempDir()
	src := filepath.Join(project, "app")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "Pulse.tsx"), []byte("v.value = withRepeat(withTiming(1), -1);"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		args []string
		ok   bool
		code string // the first diagnostic's code when !ok
	}{
		{[]string{"adapter", "check"}, false, "CONFIG_MISSING"},
		{[]string{"device", "list", "--no-live"}, true, ""},
		{[]string{"device", "show", "s20"}, false, "DEVICE_UNKNOWN"},
		{[]string{"device", "remove", "s20", "--yes"}, false, "DEVICE_UNKNOWN"},
		{[]string{"device", "shell", "s20", "--lease", "plt_x", "shell", "ls"}, false, "USAGE"},
		{[]string{"device", "shell", "s20", "--lease", "plt_x", "--", "shell", "ls"}, false, "DEVICE_UNKNOWN"},
		{[]string{"device", "screencap", "s20", "--lease", "plt_x"}, false, "USAGE"},
		{[]string{"device", "screencap", "s20", "--lease", "plt_x", "--out", "x.png"}, false, "DEVICE_UNKNOWN"},
		{[]string{"device", "pull", "s20", "/sdcard/x", "x", "--lease", "plt_x"}, false, "DEVICE_UNKNOWN"},
		// --device names the device as on every other verb (the FixIt sweep's
		// `device screencap --device s20` was an unknown flag).
		{[]string{"device", "screencap", "--device", "s20", "--lease", "plt_x", "--out", "x.png"}, false, "DEVICE_UNKNOWN"},
		{[]string{"device", "shell", "--device", "s20", "--lease", "plt_x", "--", "shell", "ls"}, false, "DEVICE_UNKNOWN"},
		{[]string{"device", "pull", "--device", "s20", "/sdcard/x", "x", "--lease", "plt_x"}, false, "DEVICE_UNKNOWN"},
		{[]string{"device", "show", "--device", "s20"}, false, "DEVICE_UNKNOWN"},
		{[]string{"device", "screencap", "s20", "--device", "iphone11", "--lease", "plt_x", "--out", "x.png"}, false, "USAGE"},
		{[]string{"device", "screencap", "--lease", "plt_x", "--out", "x.png"}, false, "USAGE"},
		{[]string{"lease", "status"}, true, ""},
		{[]string{"lease", "acquire", "s20", "--purpose", "surface test"}, false, "DEVICE_UNKNOWN"},
		{[]string{"lease", "renew", "s20", "--lease", "plt_x"}, false, "DEVICE_UNKNOWN"},
		{[]string{"lease", "release", "s20", "--lease", "plt_x"}, false, "DEVICE_UNKNOWN"},
		{[]string{"lease", "reap", "--dry-run"}, true, ""},
		{[]string{"lease", "break", "s20", "--reason", "x"}, false, "DEVICE_UNKNOWN"},
		{[]string{"doctor", "--for", "nope"}, false, "USAGE"},
		{[]string{"fingerprint", "--platform", "ios"}, false, "CONFIG_MISSING"},
		{[]string{"build", "find", "--platform", "android"}, false, "CONFIG_MISSING"},
		{[]string{"build", "import", "/nope.apk", "--platform", "android"}, false, "CONFIG_MISSING"},
		{[]string{"build", "list"}, true, ""},
		{[]string{"build", "gc", "--dry-run"}, true, ""},
		{[]string{"build", "gc", "--max-size", "lots"}, false, "USAGE"},
		{[]string{"wda", "list"}, true, ""},
		{[]string{"wda", "find"}, false, "CONFIG_MISSING"},
		// HOST_BUSY_BUILDING's retry line for a WDA build: the flag must parse.
		{[]string{"wda", "build", "--json", "--wait", "60m"}, false, "CONFIG_MISSING"},
		{[]string{"bundle", "list"}, true, ""},
		{[]string{"bundle", "export", "--platform", "ios"}, false, "CONFIG_MISSING"},
		{[]string{"pack"}, false, "USAGE"},
		{[]string{"install", "pf1-x", "--device", "s20", "--lease", "plt_x"}, false, "DEVICE_UNKNOWN"},
		{[]string{"net", "status", "--device", "s20", "--lease", "plt_x"}, false, "CONFIG_MISSING"},
		{[]string{"app", "launch", "--device", "s20", "--lease", "plt_x"}, false, "DEVICE_UNKNOWN"},
		{[]string{"app", "link", "--device", "s20", "--lease", "plt_x", "--account", "a@b.c"}, false, "CONFIG_MISSING"},
		{[]string{"run", "calendar", "--device", "s20", "--lease", "plt_x", "--variant", "x"}, false, "CONFIG_MISSING"},
		{[]string{"probe", "sideways", "--device", "s20", "--lease", "plt_x"}, false, "USAGE"},
		{[]string{"probe", "rest", "--device", "s20", "--lease", "plt_x"}, false, "DEVICE_UNKNOWN"},
		{[]string{"analyze"}, false, "USAGE"},
		{[]string{"compare", "/nope"}, false, "USAGE"},
		{[]string{"report"}, true, ""},
		{[]string{"report", "--gate"}, false, "NOTHING_MEASURED"},
		{[]string{"hazards", src}, true, ""},
		{[]string{"crashes", "--device", "s20", "--lease", "plt_x"}, false, "DEVICE_UNKNOWN"},
		{[]string{"teleport"}, false, "USAGE"},
		{[]string{"run", "--bogus"}, false, "USAGE"},
	}
	for _, c := range cases {
		name := strings.Join(c.args, " ")
		t.Run(name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			cmd, err := (App{Stdout: &out, Stderr: &errOut}).Command("perflab")
			if err != nil {
				t.Fatal(err)
			}
			// Global flags go before a passthrough's "--", as a caller must put them.
			args := append([]string{}, c.args...)
			at := len(args)
			for i, a := range args {
				if a == "--" {
					at = i
					break
				}
			}
			args = append(append(append([]string{}, args[:at]...), "--json", "--project", project), args[at:]...)
			cmd.SetArgs(args)
			err = cmd.Execute()
			dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
			var env struct {
				V           int               `json:"v"`
				OK          bool              `json:"ok"`
				Verb        string            `json:"verb"`
				Diagnostics []runx.Diagnostic `json:"diagnostics"`
				Next        []string          `json:"next"`
			}
			if derr := dec.Decode(&env); derr != nil {
				t.Fatalf("no envelope: %v\nstdout: %s\nstderr: %s", derr, out.String(), errOut.String())
			}
			if dec.More() {
				t.Fatalf("more than one envelope: %s", out.String())
			}
			raw := out.String()
			if env.V != runx.EnvelopeVersion || env.Verb == "" || strings.Contains(raw, `"diagnostics": null`) || strings.Contains(raw, `"next": null`) {
				t.Fatalf("malformed envelope: %s", raw)
			}
			exit := ExitCode(err)
			if env.OK != c.ok {
				t.Fatalf("ok = %v, want %v: %s", env.OK, c.ok, raw)
			}
			if c.ok {
				if exit != 0 {
					t.Fatalf("ok envelope with exit %d", exit)
				}
				return
			}
			if len(env.Diagnostics) == 0 || exit != 2 {
				t.Fatalf("a failure needs a diagnostic and exit 2 (exit %d): %s", exit, raw)
			}
			if env.Diagnostics[len(env.Diagnostics)-1].Code != c.code && env.Diagnostics[0].Code != c.code {
				t.Fatalf("code = %s, want %s: %s", env.Diagnostics[0].Code, c.code, raw)
			}
		})
	}
}

// A misused sub-verb's USAGE fix is its own usage line, never the help dump:
// `wda build --wait` once answered "perflab --help".
func TestPerflabUsageFixNamesTheSubVerb(t *testing.T) {
	for _, tc := range []struct{ verb, prefix string }{
		{"wda build", "perflab wda find|build "},
		{"build find", "perflab build find|native "},
		{"device shell", "perflab device shell "},
		{"pack", "perflab pack "},
		{"nope", "perflab --help"},
	} {
		if got := usageFix(tc.verb); !strings.HasPrefix(got, tc.prefix) {
			t.Errorf("usageFix(%q) = %q, want prefix %q", tc.verb, got, tc.prefix)
		}
	}
}
