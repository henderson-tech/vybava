package installer

import "golang.org/x/sys/unix"

// exchange swaps two paths in one step (renamex_np RENAME_SWAP).
func exchange(a, b string) error {
	return unix.RenamexNp(a, b, unix.RENAME_SWAP)
}
