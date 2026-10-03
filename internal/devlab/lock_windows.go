//go:build windows

package devlab

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// lockOffsetHigh places the locked byte at 4 GiB, past the holder JSON, so a
// refused caller can still read who holds the lock (LockFileEx forbids
// reads of a locked range).
const lockOffsetHigh = 1

// lockFile takes an exclusive LockFileEx lock, retrying fail-immediately
// attempts until wait passes (errLockTimeout); wait 0 tries once. Windows
// releases it when the handle closes or the process dies.
func lockFile(path string, wait time.Duration) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	handle := windows.Handle(f.Fd())
	deadline := time.Now().Add(wait)
	for {
		ol := &windows.Overlapped{OffsetHigh: lockOffsetHigh}
		err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) && !errors.Is(err, windows.ERROR_IO_PENDING) {
			_ = f.Close()
			return nil, err
		}
		if !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, errLockTimeout
		}
		time.Sleep(lockPoll)
	}
}

func unlockFile(f *os.File) {
	_ = windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{OffsetHigh: lockOffsetHigh})
	_ = f.Close()
}
