package journeys

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivatePathRejectsRepositoryThroughSymlinks(t *testing.T) {
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "outside-looking-link")
	if err := os.Symlink(repo, alias); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{repo, filepath.Join(repo, "plan.json"), filepath.Join(alias, "new", "plan.json")} {
		if _, err := PrivatePath(repo, target); err == nil {
			t.Fatalf("accepted repository-private path %s", target)
		}
	}
	if _, err := PrivatePath(alias, filepath.Join(repo, "plan.json")); err == nil {
		t.Fatal("accepted private path through an aliased repository root")
	}
	if _, err := PrivatePath(repo, filepath.Join(base, "exports", "new", "plan.json")); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(base, "broken")
	if err := os.Symlink(filepath.Join(repo, "missing"), broken); err != nil {
		t.Fatal(err)
	}
	if _, err := PrivatePath(repo, filepath.Join(broken, "plan.json")); err == nil {
		t.Fatal("accepted unresolved symlink")
	}
}
