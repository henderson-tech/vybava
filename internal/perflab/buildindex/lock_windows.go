//go:build windows

package buildindex

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive (or shared) LockFileEx lock on path, retrying
// fail-immediately attempts until wait passes (errLockTimeout). Windows
// releases the lock when the holder's handle closes or its process dies.
// Lifted from polishkit.
func lockFile(path string, wait time.Duration, shared bool) (*os.File, func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, nil, err
	}
	handle := windows.Handle(f.Fd())
	deadline := time.Now().Add(wait)
	for {
		ol := new(windows.Overlapped)
		flags := uint32(windows.LOCKFILE_EXCLUSIVE_LOCK | windows.LOCKFILE_FAIL_IMMEDIATELY)
		if shared {
			flags = windows.LOCKFILE_FAIL_IMMEDIATELY
		}
		err := windows.LockFileEx(handle, flags, 0, 1, 0, ol)
		if err == nil {
			return f, func() {
				_ = windows.UnlockFileEx(handle, 0, 1, 0, new(windows.Overlapped))
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) && !errors.Is(err, windows.ERROR_IO_PENDING) {
			_ = f.Close()
			return nil, nil, err
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, nil, errLockTimeout
		}
		time.Sleep(lockPoll)
	}
}
