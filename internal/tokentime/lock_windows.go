//go:build windows

package tokentime

import "errors"

func tryLock(string) (func(), error) {
	return nil, errors.New("tokentime currently supports macOS and Linux")
}
