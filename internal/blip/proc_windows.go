//go:build windows

package blip

import (
	"os/exec"
	"syscall"
)

// detach starts the daemon in a new process group without a console window.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | 0x08000000}
}

// ownsPID cannot verify a command line here, so a stale pid is never
// signalled on windows: `down` only cleans the files.
func ownsPID(int, string) bool { return false }

func terminate(int) error { return nil }
