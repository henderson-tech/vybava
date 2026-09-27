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
