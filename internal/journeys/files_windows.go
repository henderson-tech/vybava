//go:build windows

package journeys

import "os"

// Match the existing operator/reconcile boundary: never replace OS locking
// with an unsafe no-op merely to make the portable multicall binary compile.
func acquireJournalLock(string) (func(), error) {
	return nil, problem("TARGET_UNSAFE", "private journey journals currently require macOS or Linux")
}

func openJournalAppend(string) (*os.File, error) {
	return nil, problem("TARGET_UNSAFE", "private journey journals currently require macOS or Linux")
}
