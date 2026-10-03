//go:build windows

package fleet

import (
	"errors"
	"time"
)

var errLedgerBusy = errors.New("ledger busy")

func lockLedger(string, time.Duration) (func(), error) {
	return nil, errors.New("the fleet ledger currently supports macOS and Linux")
}
