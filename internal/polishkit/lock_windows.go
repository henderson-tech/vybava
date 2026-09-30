//go:build windows

package polishkit

import (
	"os"
	"time"
)

// lockFile on Windows uses an exclusive-create lock file (no flock): the
// pass's lock file is created with O_EXCL and removed on release; a holder
// is waited for by polling.
func lockFile(path string) (func(), error) {
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
		if err == nil {
			return func() {
				_ = f.Close()
				_ = os.Remove(path)
			}, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}
