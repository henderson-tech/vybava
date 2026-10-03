//go:build !windows

package fleet

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

var errLedgerBusy = errors.New("ledger busy")

// lockLedger takes the session's ledger lock, waiting up to timeout: two
// tool calls of one session can finish together, and their read-modify-write
// of the ledger must not interleave.
func lockLedger(path string, timeout time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { f.Close() }, nil
		}
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("lock fleet ledger: %w", err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, errLedgerBusy
		}
		time.Sleep(20 * time.Millisecond)
	}
}
