//go:build !windows

package hostexec

import (
	"errors"
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in a group of its own so a stop reaches
// its whole tree (xcodebuild's clang children) and only it.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// stopGroup sends SIGTERM to the child's own process group; exec's
// WaitDelay escalates to SIGKILL of the leader when it lingers.
func stopGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}

// PipeCapacity measures how many bytes a fresh kernel pipe accepts before a
// write would block: 1 KiB non-blocking writes until EAGAIN, capped at 1 MiB.
// A healthy Mac answers 16-64 KiB. When the kernel's pipe memory is
// exhausted (thousands of processes across dozens of sessions) a fresh pipe
// takes 512 B, every clang `-v -E -dM` probe xcodebuild runs blocks on its
// write, and the build sits at "Planning build" forever (the 4 h hang). The
// process count is NOT the signal: 2729 processes measured a healthy 65536 B.
func PipeCapacity() (int, error) {
	var fds [2]int
	if err := syscall.Pipe(fds[:]); err != nil {
		return 0, err
	}
	defer syscall.Close(fds[0])
	defer syscall.Close(fds[1])
	if err := syscall.SetNonblock(fds[1], true); err != nil {
		return 0, err
	}
	return fillPipe(func(p []byte) (int, error) { return syscall.Write(fds[1], p) })
}

// fillPipe writes 1 KiB chunks through write until it would block (or a
// short write), capped at PipeProbeCap. Split out so a test stubs the writer.
func fillPipe(write func([]byte) (int, error)) (int, error) {
	chunk := make([]byte, 1024)
	total := 0
	for total < PipeProbeCap {
		n, err := write(chunk)
		if n > 0 {
			total += n
		}
		if err != nil {
			if errors.Is(err, syscall.EAGAIN) {
				return total, nil
			}
			return total, err
		}
		if n < len(chunk) {
			return total, nil
		}
	}
	return total, nil
}

// FreeBytes is the space available to an unprivileged user on the volume
// holding path.
func FreeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}

// ProcessAlive reports whether pid names a running process. Display and
// pre-signal check only: a recycled pid reads alive, so callers that act on
// it also compare the recorded start time.
func ProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// Terminate sends SIGTERM to one literal pid (never a pattern).
func Terminate(pid int) error {
	if pid <= 1 {
		return errors.New("refusing to signal pid <= 1")
	}
	return syscall.Kill(pid, syscall.SIGTERM)
}
