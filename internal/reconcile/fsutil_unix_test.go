//go:build unix

package reconcile

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func inode(t *testing.T, p string) uint64 {
	t.Helper()
	fi, err := os.Stat(p)
	mustT(t, err)
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no Stat_t for %s", p)
	}
	return st.Ino
}

// A bind-mounted single file (compose `./x.ini:/etc/x.ini`) is pinned to its
// inode: converging over it must rewrite in place, not rename a temp file
// over it, or the container keeps the old content.
func TestApplyFileKeepsTheInodeOfAnExistingDestination(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.ini")
	dest := filepath.Join(dir, "live", "x.ini")
	mustT(t, os.MkdirAll(filepath.Dir(dest), 0o755))
	mustT(t, os.WriteFile(src, []byte("v2\n"), 0o755))
	mustT(t, os.WriteFile(dest, []byte("v1 much longer content\n"), 0o644))
	before := inode(t, dest)

	mustT(t, applyFile(src, dest))

	if got := inode(t, dest); got != before {
		t.Fatalf("inode changed %d -> %d: live write must stay in place", before, got)
	}
	b, err := os.ReadFile(dest)
	mustT(t, err)
	if string(b) != "v2\n" {
		t.Fatalf("content = %q, want truncated rewrite", b)
	}
	fi, err := os.Stat(dest)
	mustT(t, err)
	if fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode = %o, want the repo file's exec bit applied", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(dest))
	mustT(t, err)
	if len(entries) != 1 {
		t.Fatalf("no temp file may be left beside a live rewrite, got %d entries", len(entries))
	}
}

// A destination that does not exist yet still lands atomically (temp +
// rename), so a reader never opens a half-written new file.
func TestApplyFileCreatesMissingDestinationAtomically(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.conf")
	dest := filepath.Join(dir, "new", "deep", "x.conf")
	mustT(t, os.WriteFile(src, []byte("fresh\n"), 0o644))

	mustT(t, applyFile(src, dest))

	b, err := os.ReadFile(dest)
	mustT(t, err)
	if string(b) != "fresh\n" {
		t.Fatalf("content = %q", b)
	}
	entries, err := os.ReadDir(filepath.Dir(dest))
	mustT(t, err)
	if len(entries) != 1 {
		t.Fatalf("expected only the new file, got %d entries", len(entries))
	}
}
