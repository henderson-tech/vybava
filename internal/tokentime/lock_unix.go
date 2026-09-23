//go:build !windows

package tokentime

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// tryLock takes the index lock without waiting: a pass that finds another
// running reports ErrBusy instead of queueing behind it.
func tryLock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("lock tokentime state: %w", err)
	}
}
