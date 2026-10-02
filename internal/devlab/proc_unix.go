//go:build !windows

package devlab

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// psStartLayout is ps's lstart once strings.Fields has collapsed the
// day-of-month padding ("Thu Oct  1 22:51:44 2026").
const psStartLayout = "Mon Jan 2 15:04:05 2006"

// procStart reads a process's start time. kill(pid, 0) settles "gone"
// cheaply (only ESRCH means gone; EPERM is a live process of another user);
// the start time then tells a recycled pid from the original holder.
func procStart(pid int) (time.Time, bool, error) {
	if pid <= 0 {
		return time.Time{}, false, errors.New("no pid")
	}
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return time.Time{}, false, nil
	}
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	cmd.Env = append(scrubbedEnv(), "LC_ALL=C")
	out, err := cmd.Output()
	line := strings.Join(strings.Fields(string(out)), " ")
	if line == "" {
		var exitErr *exec.ExitError
		if err == nil || errors.As(err, &exitErr) {
			return time.Time{}, false, nil
		}
		return time.Time{}, false, err
	}
	start, perr := time.ParseInLocation(psStartLayout, line, time.Local)
	if perr != nil {
		return time.Time{}, false, perr
	}
	return start.UTC(), true, nil
}

// stopGroup sends SIGTERM to a whole process group a lease recorded.
func stopGroup(pgid int) error {
	if pgid <= 1 {
		return errors.New("refusing to signal process group <= 1")
	}
	err := syscall.Kill(-pgid, syscall.SIGTERM)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// stopProcess sends SIGTERM to one process a lease recorded.
func stopProcess(pid int) error {
	if pid <= 1 {
		return errors.New("refusing to signal pid <= 1")
	}
	err := syscall.Kill(pid, syscall.SIGTERM)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

// ownGroup starts a child in its own process group, so a timeout (or a
// lease release) stops exactly the tree devlab started.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
