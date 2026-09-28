//go:build unix

package reconcile

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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

// within fails the test instead of hanging when fn blocks — a FIFO opened
// without O_NONBLOCK never returns.
func within(t *testing.T, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("blocked on the destination (a FIFO opened without O_NONBLOCK)")
	}
}

// swapIn arms the seam between writeLive's type check and its open: when
// the write reaches dest, replace runs first — the attacker's swap, made
// deterministic.
func swapIn(t *testing.T, dest string, replace func()) {
	t.Helper()
	beforeInPlaceOpen = func(d string) {
		if d == dest {
			replace()
		}
	}
	t.Cleanup(func() { beforeInPlaceOpen = nil })
}

// The in-place rewrite never writes THROUGH the final component: a symlink
// or a non-regular file there — also one swapped in after writeLive checked
// the type — is refused with nothing written anywhere, while a regular file
// keeps its inode (the bind-mount fix).
func TestApplyFileNeverWritesThroughTheFinalComponent(t *testing.T) {
	regular := func(t *testing.T, dest, _ string) {
		mustT(t, os.WriteFile(dest, []byte("v1 much longer content\n"), 0o644))
	}
	symlink := func(t *testing.T, dest, victim string) {
		mustT(t, os.Remove(dest))
		mustT(t, os.Symlink(victim, dest))
	}
	fifo := func(t *testing.T, dest, _ string) {
		mustT(t, os.Remove(dest))
		mustT(t, syscall.Mkfifo(dest, 0o644))
	}
	// with a reader the O_WRONLY open succeeds: only the fstat refuses it
	fifoWithReader := func(t *testing.T, dest, victim string) {
		fifo(t, dest, victim)
		r, err := os.OpenFile(dest, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		mustT(t, err)
		t.Cleanup(func() { r.Close() })
	}
	dir := func(t *testing.T, dest, _ string) {
		mustT(t, os.Remove(dest))
		mustT(t, os.Mkdir(dest, 0o755))
	}
	cases := []struct {
		name     string
		live     []func(t *testing.T, dest, victim string) // before applyFile
		swap     func(t *testing.T, dest, victim string)   // inside the check→open window
		wantType fs.FileMode                               // the live type after the call
		wantKind string                                    // "" = the write lands in place
	}{
		{name: "regular file is rewritten in place", live: steps(regular)},
		{name: "symlink at the final component", live: steps(regular, symlink), wantType: fs.ModeSymlink, wantKind: "symlink"},
		{name: "symlink swapped in after the type check", live: steps(regular), swap: symlink, wantType: fs.ModeSymlink, wantKind: "symlink"},
		{name: "named pipe", live: steps(regular, fifo), wantType: fs.ModeNamedPipe, wantKind: "write"},
		{name: "named pipe swapped in after the type check", live: steps(regular), swap: fifo, wantType: fs.ModeNamedPipe, wantKind: "write"},
		{name: "named pipe with a reader swapped in", live: steps(regular), swap: fifoWithReader, wantType: fs.ModeNamedPipe, wantKind: "write"},
		{name: "directory", live: steps(regular, dir), wantType: fs.ModeDir, wantKind: "write"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			src := filepath.Join(root, "src.conf")
			dest := filepath.Join(root, "live", "x.conf")
			victim := filepath.Join(root, "victim.conf")
			mustT(t, os.MkdirAll(filepath.Dir(dest), 0o755))
			mustT(t, os.WriteFile(src, []byte("v2\n"), 0o644))
			mustT(t, os.WriteFile(victim, []byte("victim\n"), 0o600))
			for _, step := range tc.live {
				step(t, dest, victim)
			}
			before := inode(t, dest)
			if tc.swap != nil {
				swapIn(t, dest, func() { tc.swap(t, dest, victim) })
			}

			var err error
			within(t, func() { err = applyFile(src, dest) })

			if b, rerr := os.ReadFile(victim); rerr != nil || string(b) != "victim\n" {
				t.Fatalf("symlink target written through: %q (%v)", b, rerr)
			}
			if fi, serr := os.Stat(victim); serr != nil || fi.Mode().Perm() != 0o600 {
				t.Fatalf("symlink target chmodded through: %v (%v)", fi.Mode().Perm(), serr)
			}
			fi, lerr := os.Lstat(dest)
			mustT(t, lerr)
			if got := fi.Mode().Type(); got != tc.wantType {
				t.Fatalf("live type = %v, want %v: a refusal replaces nothing", got, tc.wantType)
			}
			if tc.wantKind == "" {
				mustT(t, err)
				if got := inode(t, dest); got != before {
					t.Fatalf("inode changed %d -> %d: live write must stay in place", before, got)
				}
				if b, _ := os.ReadFile(dest); string(b) != "v2\n" {
					t.Fatalf("content = %q, want the truncated rewrite", b)
				}
				return
			}
			if err == nil {
				t.Fatal("write through a non-regular final component was not refused")
			}
			is := classifyWriteError("nginx/x.conf", dest, "", err)
			if is.Kind != tc.wantKind || is.Path != "nginx/x.conf" || !strings.HasSuffix(is.Message, "— refused") {
				t.Fatalf("issue = %+v, want kind %q on the repo path, a refusal", is, tc.wantKind)
			}
		})
	}
}

func steps(fns ...func(t *testing.T, dest, victim string)) []func(t *testing.T, dest, victim string) {
	return fns
}

// An nginx rollback snapshot reads the live file like every live read: a
// symlink or FIFO swapped in after the sweep's checks is refused — never
// read through into the snapshot, never blocked on while the lock is held.
func TestCopyPreserveNeverReadsThroughTheFinalComponent(t *testing.T) {
	cases := []struct {
		name string
		live func(t *testing.T, src, victim string)
	}{
		{"symlink", func(t *testing.T, src, victim string) { mustT(t, os.Symlink(victim, src)) }},
		{"named pipe", func(t *testing.T, src, _ string) { mustT(t, syscall.Mkfifo(src, 0o644)) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			src, snap, victim := filepath.Join(root, "live.conf"), filepath.Join(root, "snap"), filepath.Join(root, "victim")
			mustT(t, os.WriteFile(victim, []byte("victim\n"), 0o600))
			mustT(t, os.WriteFile(snap, nil, 0o600))
			tc.live(t, src, victim)

			var err error
			within(t, func() { err = copyPreserve(src, snap) })

			var refused *refusedDest
			if !errors.As(err, &refused) {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if b, _ := os.ReadFile(snap); len(b) != 0 {
				t.Fatalf("snapshot read through the %s: %q", tc.name, b)
			}
		})
	}
}

// End to end: a destination swapped for a symlink after the sweep's
// canonical check lands under the tick's `.errors` as {kind: symlink, path:
// <repo path>} — the shape the parity script reads (`.errors[].path`) — in
// the run result and history; a FIFO at a destination never blocks the
// sweep; the symlink's target stays untouched and the rest of the tick lands.
func TestConvergeRefusesALiveSymlinkSwappedInAfterItsCheck(t *testing.T) {
	b := newBox(t, map[string]string{"scripts/a.sh": "a1\n", "scripts/b.sh": "b1\n"})
	e := b.engine()
	_, err := e.Run()
	mustT(t, err)
	commitFiles(t, b.seed, "v2", map[string]string{"scripts/a.sh": "a2\n", "scripts/b.sh": "b2\n", "scripts/c.sh": "c1\n"})
	victim := filepath.Join(b.root, "victim")
	mustT(t, os.WriteFile(victim, []byte("victim\n"), 0o600))
	dest := filepath.Join(b.root, "opt/scripts/a.sh")
	swapIn(t, dest, func() {
		mustT(t, os.Remove(dest))
		mustT(t, os.Symlink(victim, dest))
	})
	mustT(t, syscall.Mkfifo(filepath.Join(b.root, "opt/scripts/c.sh"), 0o644))

	var res Result
	within(t, func() { res, err = e.Run() })

	if got, _ := os.ReadFile(victim); string(got) != "victim\n" {
		t.Fatalf("root write redirected through the swapped symlink: %q", got)
	}
	if err == nil {
		t.Fatal("a tick with refused writes must exit non-zero")
	}
	raw, jerr := json.Marshal(res)
	mustT(t, jerr)
	var wire struct {
		Errors []struct{ Kind, Path string } `json:"errors"`
	}
	mustT(t, json.Unmarshal(raw, &wire))
	want := []struct{ Kind, Path string }{{"symlink", "scripts/a.sh"}, {"write", "scripts/c.sh"}}
	if len(wire.Errors) != len(want) || wire.Errors[0] != want[0] || wire.Errors[1] != want[1] {
		t.Fatalf(".errors = %+v, want %+v", wire.Errors, want)
	}
	if b.live("scripts/b.sh") != "b2\n" {
		t.Fatal("a refusal blocked the rest of the sweep")
	}
	hist, herr := State{Dir: b.m.StateDir}.History(1)
	mustT(t, herr)
	if len(hist) != 1 || len(hist[0].Errors) != 2 || hist[0].Errors[0].Path != "scripts/a.sh" {
		t.Fatalf("history lacks the refusal: %+v", hist)
	}
}
