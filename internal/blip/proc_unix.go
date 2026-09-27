//go:build !windows

package blip

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// detach puts the daemon in its own session so it survives the parent's exit.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// ownsPID reports whether pid is a live blip daemon serving name — PIDs are
// recycled, so a stale state file's pid is never signalled on faith.
func ownsPID(pid int, name string) bool {
	out, err := exec.Command("ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "serve --name "+name+" ")
}

// terminate asks a verified daemon to stop.
func terminate(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}
