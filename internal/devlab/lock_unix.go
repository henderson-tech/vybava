//go:build !windows

package devlab

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// lockFile takes an exclusive flock on path, retrying non-blocking attempts
// until wait passes (errLockTimeout); wait 0 tries once. The kernel releases
// the lock when the descriptor closes or the process dies.
func lockFile(path string, wait time.Duration) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
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
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
