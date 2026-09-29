//go:build !windows

package reconcile

import (
	"errors"
	"os"
	"syscall"
)

// openNoFollow opens path without following a symlink at its final component
// (O_NOFOLLOW: the kernel answers ELOOP, surfaced as a symlink refusal) and
// without blocking on a FIFO (O_NONBLOCK: a reader opens at once, a writer
// with no reader gets ENXIO — which open(2) returns only for a FIFO, socket
// or device, never a regular file); openRegular's fstat then decides the
// type. O_NONBLOCK has no effect on a regular file's reads and writes.
func openNoFollow(path string, flag int) (*os.File, error) {
	f, err := os.OpenFile(path, flag|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	switch {
	case errors.Is(err, syscall.ELOOP):
		return nil, &refusedDest{kind: "symlink", dest: path, why: "is a symlink"}
	case errors.Is(err, syscall.ENXIO):
		return nil, &refusedDest{kind: "write", dest: path, why: "is a FIFO, socket or device, not a regular file"}
	}
	return f, err
}
