package devlab

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Locks are kernel locks (flock on unix, LockFileEx on windows), lifted from
// polish-kit: the OS releases them when the holder's descriptor closes or its
// process dies, so a crash never leaves a stale lock. The holder writes who
// it is into the lock file, so a refused caller can name it.

// lockPoll is the retry interval of the non-blocking acquire loop.
const lockPoll = 50 * time.Millisecond

// errLockTimeout is what lockFile returns when the wait passed.
var errLockTimeout = errors.New("lock wait timed out")

// lockHolder is the JSON a holder writes into its lock file.
type lockHolder struct {
	PID   int       `json:"pid"`
	Verb  string    `json:"verb"`
	Since time.Time `json:"since"`
}

// heldLock is one acquired lock.
type heldLock struct {
	release func()
}

func (h heldLock) Release() {
	if h.release != nil {
		h.release()
	}
}

// lock takes locks/<name>.lock within wait, recording the holder.
func (l *Lab) lock(name, verb string, wait time.Duration) (heldLock, error) {
	dir := l.locksDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return heldLock{}, err
	}
	path := filepath.Join(dir, name+".lock")
	f, err := lockFile(path, wait)
	if err != nil {
		return heldLock{}, err
	}
	holder, _ := json.Marshal(lockHolder{PID: l.Pid, Verb: verb, Since: l.now()})
	_ = f.Truncate(0)
	_, _ = f.WriteAt(append(holder, '\n'), 0)
	return heldLock{release: func() {
		_ = f.Truncate(0)
		unlockFile(f)
	}}, nil
}

// readHolder names whoever holds a lock file, best effort.
func readHolder(path string) (lockHolder, bool) {
	b, err := os.ReadFile(path)
	if err != nil || len(b) == 0 {
		return lockHolder{}, false
	}
	var h lockHolder
	if json.Unmarshal(b, &h) != nil || h.PID == 0 {
		return lockHolder{}, false
	}
	return h, true
}

// lockLedger serialises writes of devices.json.
func (l *Lab) lockLedger(verb string) (heldLock, error) {
	return l.lockOrLedgerLocked("ledger", verb)
}

// lockLease serialises the read-modify-write of one device's lease file;
// it is held for milliseconds, never across a device command.
func (l *Lab) lockLease(id, verb string) (heldLock, error) {
	return l.lockOrLedgerLocked("lease-"+id, verb)
}

func (l *Lab) lockOrLedgerLocked(name, verb string) (heldLock, error) {
	wait := l.LockWait
	if wait <= 0 {
		wait = DefaultLockWait
	}
	h, err := l.lock(name, verb, wait)
	if errors.Is(err, errLockTimeout) {
		path := filepath.Join(l.locksDir(), name+".lock")
		detail := fmt.Sprintf("%s stayed locked for %s", path, wait)
		if holder, ok := readHolder(path); ok {
			detail += fmt.Sprintf(" (held by pid %d, %s, since %s)", holder.PID, holder.Verb, holder.Since.Format(time.RFC3339))
		}
		return heldLock{}, diag(DiagLedgerLocked, detail, "wait for that perflab process to finish, then re-run")
	}
	return h, err
}

// LockDevice takes the device lock every device-touching verb holds for its
// whole duration, so two invocations never interleave adb or devicectl
// calls, even with the same token. A held lock answers DEVICE_BUSY at once,
// naming the verb, pid and elapsed time.
func (l *Lab) LockDevice(id, verb string) (release func(), err error) {
	h, err := l.lock(deviceLockName(id), verb, 0)
	if errors.Is(err, errLockTimeout) {
		path := filepath.Join(l.locksDir(), deviceLockName(id)+".lock")
		detail := fmt.Sprintf("%s is busy", id)
		if holder, ok := readHolder(path); ok {
			detail = fmt.Sprintf("%s is busy: `perflab %s` (pid %d) has held it for %s", id, holder.Verb, holder.PID, l.now().Sub(holder.Since).Round(time.Second))
		}
		return nil, diag(DiagDeviceBusy, detail, fmt.Sprintf("wait for it to finish, or see the holder: perflab lease status %s --json", id))
	}
	if err != nil {
		return nil, err
	}
	return h.Release, nil
}

// Lock names carry a kind prefix, so no device id ("ledger", "lease-x",
// "host-build") can collide with another lock.
func deviceLockName(id string) string { return "device-" + id }
