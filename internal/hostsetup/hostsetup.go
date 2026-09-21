// Package hostsetup applies the idempotent per-machine settings a Mac needs
// to stay healthy under many parallel agent sessions. Each Step is a check
// and an apply: `vybava setup mac` runs them all, prints what changed, and
// leaves a machine already set up untouched (nothing rewritten, no mtime
// churn). Steps are incident-born: the first one caps Gradle's daemon idle
// timeout after 10 GB of Gradle/Kotlin daemons sat idle for hours on
// 2026-09-19.
package hostsetup

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Step is one idempotent host setting. Check reports whether it already
// holds; Apply makes it hold. Both receive the home directory so tests can
// run against a temp one.
type Step struct {
	Name  string
	Why   string
	Check func(home string) (bool, error)
	Apply func(home string) error
}

// Steps is every host setting `vybava setup mac` owns, in apply order.
var Steps = []Step{gradleDaemonTimeout}

// Apply runs every step: a step whose Check holds is reported as ok and
// skipped, otherwise its Apply runs (or is announced with dryRun).
func Apply(home string, dryRun bool, w io.Writer) error {
	for _, s := range Steps {
		ok, err := s.Check(home)
		if err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
		switch {
		case ok:
			fmt.Fprintf(w, "ok      %s\n", s.Name)
		case dryRun:
			fmt.Fprintf(w, "would   %s — %s\n", s.Name, s.Why)
		default:
			if err := s.Apply(home); err != nil {
				return fmt.Errorf("%s: %w", s.Name, err)
			}
			fmt.Fprintf(w, "applied %s — %s\n", s.Name, s.Why)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// gradle daemon idle timeout — Gradle keeps its daemons (and Kotlin's) alive
// 3 h after a build by default; ten minutes is plenty for an agent's
// build-fix-build loop.
// ---------------------------------------------------------------------------

const (
	gradleKey   = "org.gradle.daemon.idletimeout"
	gradleValue = "600000"
)

var gradleDaemonTimeout = Step{
	Name: "gradle daemon idle timeout 10 min",
	Why:  "~/.gradle/gradle.properties " + gradleKey + "=" + gradleValue + " (default 3 h parked 10 GB of idle daemons on 2026-09-19)",
	Check: func(home string) (bool, error) {
		lines, err := readLines(gradlePath(home))
		if err != nil {
			return false, err
		}
		_, value := findProperty(lines, gradleKey)
		return value == gradleValue, nil
	},
	Apply: func(home string) error {
		path := gradlePath(home)
		lines, err := readLines(path)
		if err != nil {
			return err
		}
		lines = setProperty(lines, gradleKey, gradleValue)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	},
}

func gradlePath(home string) string { return filepath.Join(home, ".gradle", "gradle.properties") }

// readLines returns the file's lines, or nil when it does not exist yet.
func readLines(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	text := strings.TrimSuffix(string(raw), "\n")
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

// findProperty returns the index and value of a `key=value` line (Java
// properties syntax: `=` or `:` separator, surrounding spaces ignored,
// `#`/`!` comments skipped), or -1.
func findProperty(lines []string, key string) (int, string) {
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, "!") {
			continue
		}
		sep := strings.IndexAny(t, "=:")
		if sep < 0 || strings.TrimSpace(t[:sep]) != key {
			continue
		}
		return i, strings.TrimSpace(t[sep+1:])
	}
	return -1, ""
}

// setProperty edits the key's line in place or appends one; every other
// line is kept byte-for-byte.
func setProperty(lines []string, key, value string) []string {
	if i, _ := findProperty(lines, key); i >= 0 {
		out := append([]string(nil), lines...)
		out[i] = key + "=" + value
		return out
	}
	return append(append([]string(nil), lines...), key+"="+value)
}
