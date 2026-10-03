//go:build darwin || linux

package installer

import (
	"errors"
	"syscall"
)

// exchangeUnsupported reports a filesystem or kernel without an atomic
// exchange. EXDEV is not one: it means another volume, and the caller
// retries from a stage root on the destination's own volume.
func exchangeUnsupported(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOSYS)
}
