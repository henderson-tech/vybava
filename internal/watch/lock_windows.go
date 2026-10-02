package watch

import (
	"errors"
	"os"
)

var errDaemonLocked = errors.New("daemon lock held")

// lockDaemon on Windows opens the lock file only; the daemon is a macOS and
// Linux LaunchAgent/service, and the live-socket check still refuses a second.
func lockDaemon(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
}
