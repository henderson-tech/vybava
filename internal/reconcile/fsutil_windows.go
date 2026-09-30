//go:build windows

package reconcile

import "os"

// openNoFollow has no O_NOFOLLOW on Windows. The engine cannot take its lock
// there (lock_windows.go), so this only has to compile; writeLive's Lstat and
// openRegular's type check on the opened handle still apply.
func openNoFollow(path string, flag int) (*os.File, error) {
	return os.OpenFile(path, flag, 0)
}
