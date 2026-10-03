//go:build !windows

package uiloop

import (
	"io/fs"

	"golang.org/x/sys/unix"
)

// statStamp is file's fileStamp, symlinks followed like os.ReadFile.
func statStamp(file string) (fileStamp, bool, error) {
	var st unix.Stat_t
	for {
		err := unix.Stat(file, &st)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return fileStamp{}, false, &fs.PathError{Op: "stat", Path: file, Err: err}
		}
		break
	}
	return fileStamp{Size: st.Size, MtimeNs: st.Mtim.Nano(), CtimeNs: st.Ctim.Nano(), Ino: uint64(st.Ino), Dev: uint64(st.Dev)}, true, nil
}
