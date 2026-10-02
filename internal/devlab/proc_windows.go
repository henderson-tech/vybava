//go:build windows

package devlab

import (
	"errors"
	"os/exec"
	"time"
)

// procStart cannot read a start time on Windows; liveness is unknown, so a
// lease there is bounded by its TTL alone (unknown counts as alive).
func procStart(int) (time.Time, bool, error) {
	return time.Time{}, false, errors.New("process start time is not read on windows")
}

// stopGroup is a no-op on Windows: the lab's device tooling (devicectl,
// xctrace) is macOS-only and no child group is recorded there.
func stopGroup(int) error { return nil }

func ownGroup(*exec.Cmd) {}
