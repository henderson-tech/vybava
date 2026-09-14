package claudeguards

import "testing"

func TestE2EReadGuardPatterns(t *testing.T) {
	block := []string{"/x/.e2e/shot.png", "/x/.e2e/a/b/c/shot.png", "/x/proj.e2e-run/shot.png"}
	pass := []string{"/x/.e2e/shot.jpg", "/x/assets/logo.png", "/x/.e2e/notes.md"}
	for _, p := range block {
		// guardE2ERead exits the process on block; test its predicates instead.
		if !(containsE2E(p) && hasPNG(p)) {
			t.Errorf("should block Read of %q", p)
		}
	}
	for _, p := range pass {
		if containsE2E(p) && hasPNG(p) {
			t.Errorf("should pass Read of %q", p)
		}
	}
}

func TestE2EScreenshotPatterns(t *testing.T) {
	cases := map[string]string{
		"xcrun simctl io booted screenshot /tmp/a.png":     "raw-screenshot",
		"sleep 1; xcrun simctl io booted screenshot x.png": "raw-screenshot",
		"screencapture -x /tmp/s.png":                      "screencapture",
		"man screencapture-notes":                          "",
		// Downsized shots are the sanctioned path, even across a pipe.
		"xcrun simctl io booted screenshot - | sips -Z 800 > x.jpg": "",
	}
	for cmd, want := range cases {
		if got := e2eScreenshotMatch(cmd); got != want {
			t.Errorf("e2eScreenshotMatch(%q) = %q, want %q", cmd, got, want)
		}
	}
}

// A quoted mention can never execute (B2); a leading environment assignment
// must not walk past the rule (F1).
func TestE2EScreenshotSegmentScoped(t *testing.T) {
	cases := map[string]string{
		`echo "screencapture -x /tmp/s.png"`:                              "",
		`git commit -m "guards: ban xcrun simctl io booted screenshot"`:   "",
		"FOO=1 screencapture -x /tmp/s.png":                               "screencapture",
		"SIMCTL_CHILD_FOO=1 xcrun simctl io booted screenshot /tmp/a.png": "raw-screenshot",
	}
	for cmd, want := range cases {
		if got := e2eScreenshotMatch(cmd); got != want {
			t.Errorf("e2eScreenshotMatch(%q) = %q, want %q", cmd, got, want)
		}
	}
}
