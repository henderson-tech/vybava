//go:build !windows

package reclaim

import (
	"os"
	"syscall"
)

// hardlinkID returns a file's inode identity when more than one name links
// to it, so a size walk can count it once.
func hardlinkID(info os.FileInfo) (fileID, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st.Nlink < 2 {
		return fileID{}, false
	}
	return fileID{dev: uint64(st.Dev), ino: st.Ino}, true
}
