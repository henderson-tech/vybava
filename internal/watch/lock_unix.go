//go:build !windows

package watch

import (
	"errors"
	"os"
	"syscall"
)

var errDaemonLocked = errors.New("daemon lock held")

// lockDaemon takes the one-daemon lock for the process's lifetime: two
// daemons starting together would otherwise both find the socket stale and
// the second would unlink the first's live one.
func lockDaemon(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errDaemonLocked
		}
		return nil, err
	}
	return f, nil
}
