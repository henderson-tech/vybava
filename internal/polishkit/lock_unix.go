//go:build !windows

package polishkit

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// lockFile takes an exclusive flock on the pass's lock file, retrying
// non-blocking attempts until wait passes (errLockTimeout). The kernel
// releases the lock when the holder's descriptor closes or its process
// dies, so a crashed holder never blocks the next command.
func lockFile(path string, wait time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				_ = f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
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
