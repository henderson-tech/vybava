//go:build windows

package polishkit

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive LockFileEx lock on the pass's lock file,
// retrying fail-immediately attempts until wait passes (errLockTimeout).
// Windows releases the lock when the holder's handle closes or its process
// dies, so a crashed holder never leaves a stale lock behind (an O_EXCL
// marker file would).
func lockFile(path string, wait time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	handle := windows.Handle(f.Fd())
	deadline := time.Now().Add(wait)
	for {
		ol := new(windows.Overlapped)
		err := windows.LockFileEx(handle, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
		if err == nil {
			return func() {
				_ = windows.UnlockFileEx(handle, 0, 1, 0, new(windows.Overlapped))
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) && !errors.Is(err, windows.ERROR_IO_PENDING) {
			_ = f.Close()
			return nil, err
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, errLockTimeout
		}
		time.Sleep(lockPoll)
	}
}
