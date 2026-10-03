package devlab

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// hookLockWait bounds a session hook's wait for a lease file lock: a
// session end never waits on a wedged perflab.
const hookLockWait = 3 * time.Second

// ReleaseSession releases every lease whose owner names sessionID and stops
// the process groups those leases recorded. claude-guards' SessionEnd
// `device-lease-release` hook calls it in-process. It never fails the
// session end: every problem is a line on stderr. It returns the released
// device ids.
func (l *Lab) ReleaseSession(sessionID string, stderr io.Writer) []string {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || !l.statePresent() {
		return nil
	}
	hook := *l
	hook.LockWait = hookLockWait
	ids, err := hook.leaseIDs()
	if err != nil {
		fmt.Fprintf(stderr, "perflab: session lease release: %v\n", err)
		return nil
	}
	var released []string
	for _, id := range ids {
		ls, err := hook.readLease(id)
		if err != nil || !ls.Held() || ls.Owner == nil || ls.Owner.SessionID != sessionID {
			continue
		}
		token := ls.TokenSHA256
		_, err = hook.updateLease(id, "session end", func(cur *Lease) error {
			if cur.TokenSHA256 != token {
				return errNotStale // re-leased since the read
			}
			hook.end(cur, "its session ended")
			return nil
		})
		if err != nil {
			if !errors.Is(err, errNotStale) {
				fmt.Fprintf(stderr, "perflab: could not release %s at session end: %v\n", id, err)
			}
			continue
		}
		released = append(released, id)
	}
	if len(released) > 0 {
		fmt.Fprintf(stderr, "perflab: released the session's device leases: %s\n", strings.Join(released, ", "))
	}
	return released
}

// ReapStale releases every stale lease (expired AND holder not alive); the
// SessionStart `weather --reap` hook calls it. Quiet when nothing is stale.
func (l *Lab) ReapStale(stderr io.Writer) []string {
	if !l.statePresent() {
		return nil
	}
	hook := *l
	hook.LockWait = hookLockWait
	res, err := hook.Reap(false)
	if err != nil {
		fmt.Fprintf(stderr, "perflab: stale lease reap: %v\n", err)
		return nil
	}
	data, _ := res.Data.(ReapData)
	var ids []string
	for _, v := range data.Released {
		ids = append(ids, v.Device)
	}
	if len(ids) > 0 {
		fmt.Fprintf(stderr, "perflab: reaped stale device leases: %s\n", strings.Join(ids, ", "))
	}
	return ids
}
