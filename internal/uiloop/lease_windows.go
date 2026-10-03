//go:build windows

package uiloop

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// pidAlive cannot probe a pid without a handle here, so a pid never stales
// a lease on Windows: its ttl does.
func pidAlive(int) bool { return true }

// lockLeases takes an exclusive LockFileEx lock on file, waiting for it;
// unlock closes the handle, which drops the lock.
func lockLeases(file string) (unlock func() error, err error) {
	f, err := os.OpenFile(file, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, new(windows.Overlapped)); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f.Close, nil
}
