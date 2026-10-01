package polishkit

import (
	"errors"
	"fmt"
	"time"
)

// LockWait bounds every wait for a pass's ledger lock; past it the verb
// answers ledger-locked instead of hanging the operator. Tests shorten it.
var LockWait = 30 * time.Second

// lockPoll is the retry interval of the non-blocking acquire loop.
const lockPoll = 50 * time.Millisecond

// errLockTimeout is what lockFile returns when LockWait passed.
var errLockTimeout = errors.New("lock wait timed out")

// takeLock acquires the pass lock (OS-managed: flock on unix, LockFileEx on
// windows, both released by the OS when the holder's handle or process
// dies, so a crash never leaves a stale lock) within LockWait, mapping a
// timeout to the ledger-locked diagnostic.
func (t *Tool) takeLock(path string) (func(), error) {
	unlock, err := lockFile(path, LockWait)
	if errors.Is(err, errLockTimeout) {
		return nil, diag(DiagLedgerLocked,
			fmt.Sprintf("%s is held by another polish-kit command for over %s", path, LockWait),
			fmt.Sprintf("wait for the other polish-kit command, or delete %s if no polish-kit process is running", path))
	}
	return unlock, err
}
