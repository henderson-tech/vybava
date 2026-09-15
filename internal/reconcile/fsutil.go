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

// fileSHA is `sha256sum`; "" when the file is unreadable/absent.
func fileSHA(path string) string {
	f, err := os.Open(path)
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
func writeLive(dest string, content []byte, mode fs.FileMode) error {
	if !isRegular(dest) {
		return atomicWrite(dest, content, mode)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
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

// classifyWriteError turns a failed write into an Issue: EACCES/EPERM become
// a `permission` issue naming the destination owner (the deploy-user gap),
// everything else a plain `write` issue.
func classifyWriteError(rp, dest, ownerHint string, err error) Issue {
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
