//go:build !windows

package blip

import (
	"os/exec"
	"syscall"
)

// detach puts the daemon in its own session so it survives the parent's exit.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
