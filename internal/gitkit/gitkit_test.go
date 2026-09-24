package gitkit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestStripsTypes(t *testing.T) {
	for version, want := range map[string]bool{
		"v24.17.0": true, "v23.6.0": true, "v23.5.1": false,
		"v22.18.0": true, "v22.17.9": false, "v20.19.0": false, "garbage": false,
	} {
		if got := StripsTypes(version); got != want {
			t.Errorf("StripsTypes(%q) = %v, want %v", version, got, want)
		}
	}
}

func TestResolveNodeDiagnostics(t *testing.T) {
	missing := func(string) (string, error) { return "", errors.New("not found") }
	found := func(string) (string, error) { return "/usr/bin/node", nil }
	old := func(string) (string, error) { return "v20.11.0\n", nil }

	var diag NodeError
	if _, err := ResolveNode(missing, old); !errors.As(err, &diag) || diag.Code != DiagNodeMissing {
		t.Fatalf("missing node: %v", err)
	}
	if _, err := ResolveNode(found, old); !errors.As(err, &diag) || diag.Code != DiagNodeTooOld {
		t.Fatalf("old node: %v", err)
	}
}

func TestMaterializeIsContentAddressedAndIdempotent(t *testing.T) {
	root := t.TempDir()
	bin, err := Materialize(root)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(bin) != filepath.Join(root, Digest()) {
		t.Fatalf("bin %s is not under the digest directory", bin)
	}
	for _, name := range Scripts() {
		if _, err := os.Stat(filepath.Join(bin, name+".ts")); err != nil {
			t.Fatalf("script %s not materialized: %v", name, err)
		}
	}
	again, err := Materialize(root)
	if err != nil || again != bin {
		t.Fatalf("second materialize = %q, %v; want %q", again, err, bin)
	}
}
