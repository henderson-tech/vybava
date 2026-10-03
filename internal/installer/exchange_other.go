//go:build !darwin && !linux

package installer

import "errors"

// exchange is unavailable here; swapMod falls back to two renames.
func exchange(a, b string) error {
	return errExchangeUnsupported
}

var errExchangeUnsupported = errors.New("atomic exchange unsupported")

func exchangeUnsupported(err error) bool {
	return errors.Is(err, errExchangeUnsupported)
}
