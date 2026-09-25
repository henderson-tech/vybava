package reclaim

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// removeTree deletes path (file or directory) and returns the logical bytes
// it held. Deletion and accounting share one traversal: rm walks the tree
// anyway, so sizing costs nothing extra. Read-only entries (dotslash pins its
// binaries) are unlocked on the way. A dry run walks without deleting.
func removeTree(ctx context.Context, path string, dry bool) (int64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	if !info.IsDir() {
		size := info.Size()
		if dry {
			return size, nil
		}
		if err := os.Remove(path); err != nil {
			return 0, err // count only what actually left the disk
		}
		return size, nil
	}
	var total int64
	var errs []error
	entries, err := os.ReadDir(path)
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return total, ctx.Err()
		}
		child := filepath.Join(path, entry.Name())
		n, err := removeTree(ctx, child, dry)
		total += n
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			// Unlock and retry once: read-only files under a writable dir.
			// Only a real directory: Chmod follows a symlink and would
			// re-mode its target (a .bun link into the global store).
			if !dry {
				if fi, lerr := os.Lstat(child); lerr == nil && fi.IsDir() {
					_ = os.Chmod(child, 0o700)
				}
				if _, retry := os.Lstat(child); retry == nil {
					left, _ := treeSize(ctx, child) // what the first pass could not remove
					if err2 := os.RemoveAll(child); err2 == nil {
						total += left
						continue
					}
				}
			}
			errs = append(errs, err)
		}
	}
	if !dry {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = os.Chmod(filepath.Dir(path), 0o700)
			if err2 := os.Remove(path); err2 != nil {
				errs = append(errs, err2)
			}
		}
	}
	return total, errors.Join(errs...)
}

// removeAged deletes only regular files under root whose modification time is
// before cutoff, leaving the tree and every newer file in place. Empty
// directories are left alone — the owning app expects them.
func removeAged(ctx context.Context, root string, cutoff time.Time, dry bool) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		if !info.ModTime().Before(cutoff) {
			return nil
		}
		total += info.Size()
		if dry {
			return nil
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	})
	return total, err
}

// treeSize walks without deleting; used only by dry runs of command steps
// whose tool cannot size itself.
func treeSize(ctx context.Context, path string) (int64, error) {
	return removeTree(ctx, path, true)
}
