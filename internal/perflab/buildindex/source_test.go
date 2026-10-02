package buildindex

import (
	"os"
	"path/filepath"
	"testing"
)

// A --ref tree is fingerprinted under the project's rule files and restored
// byte for byte after: the 2026-10-02 acceptance's "before" ref predated
// FixIt's fingerprint.config.js (testdata, the real file), so identical
// native inputs keyed differently and pack refused the bundle.
func TestWithProjectRulesJudgesTheRefByTheProjectsRules(t *testing.T) {
	rules, err := os.ReadFile(filepath.Join("testdata", "fingerprint.config.js"))
	if err != nil {
		t.Fatal(err)
	}
	old := []byte("module.exports = {};\n")
	for _, tc := range []struct {
		name          string
		project, tree []byte // nil: the file is absent
	}{
		{"the ref predates the project's config", rules, nil},
		{"the ref carries an older config", rules, old},
		{"only the ref has a config", nil, old},
		{"both carry the same config", rules, rules},
		{"neither has one", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project, tree := t.TempDir(), t.TempDir()
			put := func(dir string, b []byte) {
				if b != nil {
					if err := os.WriteFile(filepath.Join(dir, "fingerprint.config.js"), b, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			put(project, tc.project)
			put(tree, tc.tree)
			read := func() []byte {
				b, err := os.ReadFile(filepath.Join(tree, "fingerprint.config.js"))
				if err != nil {
					return nil
				}
				return b
			}
			var during []byte
			if err := withProjectRules(project, tree, func() error { during = read(); return nil }); err != nil {
				t.Fatal(err)
			}
			if string(during) != string(tc.project) || (during == nil) != (tc.project == nil) {
				t.Fatalf("during the fingerprint the tree held %q, want the project's %q", during, tc.project)
			}
			if after := read(); string(after) != string(tc.tree) || (after == nil) != (tc.tree == nil) {
				t.Fatalf("after: %q, want the ref's own %q restored", after, tc.tree)
			}
		})
	}
}
