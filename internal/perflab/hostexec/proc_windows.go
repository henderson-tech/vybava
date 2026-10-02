//go:build windows

package hostexec

import (
	"errors"
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
)

// setProcessGroup is a no-op on windows: the child is stopped directly.
func setProcessGroup(cmd *exec.Cmd) {}

// stopGroup kills the child process.
func stopGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// errUnsupported answers the probes that only exist on the Macs perflab
// measures from.
var errUnsupported = errors.New("not supported on windows")

// PipeCapacity is a macOS/Linux probe; windows has no equivalent hang.
func PipeCapacity() (int, error) { return 0, errUnsupported }

// FreeBytes is the space available to the caller on the volume holding path.
func FreeBytes(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree); err != nil {
		return 0, err
	}
	return free, nil
}

// ProcessAlive reports whether pid names a running process.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	windows.CloseHandle(h)
	return true
}

// Terminate kills one literal pid (never a pattern).
func Terminate(pid int) error {
	if pid <= 1 {
		return errors.New("refusing to signal pid <= 1")
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
