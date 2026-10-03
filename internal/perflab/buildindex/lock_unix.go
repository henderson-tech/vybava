//go:build !windows

package buildindex

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// lockFile takes an exclusive (or shared) flock on path, retrying non-blocking attempts
// until wait passes (errLockTimeout; wait 0 tries once). The kernel releases
// the lock when the holder's descriptor closes or its process dies, so a
// crashed build never blocks the next one. Lifted from polishkit.
func lockFile(path string, wait time.Duration, shared bool) (*os.File, func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		how := syscall.LOCK_EX
		if shared {
			how = syscall.LOCK_SH
		}
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB)
		if err == nil {
			return f, func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
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
