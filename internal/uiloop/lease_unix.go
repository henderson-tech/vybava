//go:build !windows

package uiloop

import (
	"errors"
	"os"
	"syscall"
)

// pidAlive reports whether pid is a live process on this host: signal 0
// checks without delivering anything, and EPERM is another user's process.
func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// lockLeases takes an exclusive flock on file, waiting for it; unlock
// closes the descriptor, which drops the lock.
func lockLeases(file string) (unlock func() error, err error) {
	f, err := os.OpenFile(file, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f.Close, nil
}
