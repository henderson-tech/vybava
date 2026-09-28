package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// fileSHA is `sha256sum` of a regular file; "" when it is absent, unreadable
// or not a regular file — a FIFO at a destination must never block the sweep
// (and the lock it holds) on an open, and a symlink is never read through.
func fileSHA(path string) string {
	f, err := openRegular(path, os.O_RDONLY)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func bytesSHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// canonical is `realpath -m`: symlinks in the existing prefix are resolved,
// the non-existent remainder is appended unchanged.
func canonical(p string) (string, error) {
	p = filepath.Clean(p)
	existing := p
	var rest []string
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		rest = append([]string{filepath.Base(existing)}, rest...)
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{resolved}, rest...)...), nil
}

// isSymlink reports Lstat mode symlink (false when absent).
func isSymlink(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode()&fs.ModeSymlink != 0
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func isRegular(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}

// applyFile is `install -D -m 755|644 src dest`: the repo file's exec bit
// decides the mode; parents are created; the write lands through writeLive.
func applyFile(src, dest string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	mode := fs.FileMode(0o644)
	if fi.Mode()&0o111 != 0 {
		mode = 0o755
	}
	content, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return writeLive(dest, content, mode)
}

// writeLive lands content on a LIVE destination. An existing regular file is
// rewritten in place (truncate + write on the same inode): a container that
// bind-mounts that single file (`./pgbouncer/pgbouncer.ini:/etc/pgbouncer/
// pgbouncer.ini`) has the mount pinned to the inode, so a temp + rename swap
// leaves the container reading the OLD content forever and a HUP or reload
// "sees" nothing (2026-09-14, fixit-prod pgbouncer, both boxes). A new file
// still lands via same-directory temp + rename so no reader ever opens a
// half-written file.
//
// The in-place write never goes THROUGH the final component (bash's
// `install -D` never does either): a symlink or a non-regular file there is
// refused (*refusedDest, nothing written), also when it is swapped in after
// the sweep's canonical check — the open itself refuses a symlink
// (O_NOFOLLOW) and the opened descriptor must be a regular file. Truncation
// happens only after that proof.
func writeLive(dest string, content []byte, mode fs.FileMode) error {
	fi, err := os.Lstat(dest)
	if errors.Is(err, fs.ErrNotExist) {
		return atomicWrite(dest, content, mode)
	}
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		return &refusedDest{kind: "symlink", dest: dest, why: "is a symlink"}
	}
	if !fi.Mode().IsRegular() {
		return notRegular(dest, fi.Mode())
	}
	if beforeInPlaceOpen != nil {
		beforeInPlaceOpen(dest)
	}
	f, err := openRegular(dest, os.O_WRONLY)
	if err != nil {
		return err
	}
	if err := f.Truncate(0); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(content); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// beforeInPlaceOpen runs between writeLive's type check and its open — the
// window a swapped-in symlink targets. A test seam; nil in production.
var beforeInPlaceOpen func(dest string)

// refusedDest is a live destination the reconciler will not write through:
// kind "symlink" (the kind of the static symlinked-component refusal) or
// "write" for anything but a regular file. Nothing was written.
type refusedDest struct {
	kind, dest, why string
}

func (r *refusedDest) Error() string { return r.dest + " " + r.why + " — refused" }

func notRegular(dest string, m fs.FileMode) error {
	what := "not a regular file"
	switch {
	case m.IsDir():
		what = "a directory, not a regular file"
	case m&fs.ModeNamedPipe != 0:
		what = "a named pipe, not a regular file"
	case m&fs.ModeSocket != 0:
		what = "a socket, not a regular file"
	case m&fs.ModeDevice != 0:
		what = "a device, not a regular file"
	}
	return &refusedDest{kind: "write", dest: dest, why: "is " + what}
}

// openRegular opens path's final component without following a symlink or
// blocking on a FIFO (openNoFollow), then proves on the opened descriptor
// that it is a regular file — a type swapped in after an earlier Lstat is
// caught here, not written through. Parent components still resolve: the
// sweep's canonical-destination check owns them, as it does for bash's
// `install -D`.
func openRegular(path string, flag int) (*os.File, error) {
	f, err := openNoFollow(path, flag)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, notRegular(path, fi.Mode())
	}
	return f, nil
}

// readRegular is os.ReadFile through openRegular: a symlink is never read
// through and a FIFO never blocks the caller.
func readRegular(path string) ([]byte, error) {
	f, err := openRegular(path, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// copyPreserve is `cp -p src dest` for the rollback snapshots.
func copyPreserve(src, dest string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	content, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return writeLive(dest, content, fi.Mode().Perm())
}

// classifyWriteError turns a failed write into an Issue: a refused
// destination keeps its own kind (`symlink`, like the static refusal), EACCES/
// EPERM become a `permission` issue naming the destination owner (the
// deploy-user gap), everything else a plain `write` issue.
func classifyWriteError(rp, dest, ownerHint string, err error) Issue {
	var refused *refusedDest
	if errors.As(err, &refused) {
		return Issue{Kind: refused.kind, Path: rp, Message: fmt.Sprintf("%s -> %s %s — refused", rp, dest, refused.why)}
	}
	if errors.Is(err, fs.ErrPermission) {
		msg := fmt.Sprintf("%s -> %s (permission denied", rp, dest)
		if owner := pathOwner(dest); owner != "" {
			msg += "; destination owned by " + owner
		}
		if me := currentUser(); me != "" {
			msg += ", running as " + me
		}
		if ownerHint != "" {
			msg += "; manifest owner hint: " + ownerHint
		}
		return Issue{Kind: "permission", Path: rp, Message: msg + ")"}
	}
	return Issue{Kind: "write", Path: rp, Message: fmt.Sprintf("%s -> %s (write failed — permissions?): %v", rp, dest, err)}
}

// pathOwner names the owner of the nearest existing ancestor of p.
func pathOwner(p string) string {
	for {
		if owner := ownerOf(p); owner != "" {
			return owner
		}
		parent := filepath.Dir(p)
		if parent == p {
			return ""
		}
		p = parent
	}
}

func containedIn(p, root string) bool {
	root = filepath.Clean(root)
	return strings.HasPrefix(p, root+string(filepath.Separator))
}
