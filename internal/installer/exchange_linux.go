package installer

import "golang.org/x/sys/unix"

// exchange swaps two paths in one step (renameat2 RENAME_EXCHANGE).
func exchange(a, b string) error {
	return unix.Renameat2(unix.AT_FDCWD, a, unix.AT_FDCWD, b, unix.RENAME_EXCHANGE)
}
