//go:build unix

package reconcile

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// killGroupOnCancel starts cmd as the leader of its own process group and
// makes its context's expiry SIGKILL the whole group: `docker compose` runs the
// compose plugin as docker's child, so killing docker alone would leave the
// plugin running with the probe's pipes open.
func killGroupOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); !errors.Is(err, syscall.ESRCH) {
			return err
		}
		return os.ErrProcessDone
	}
}

// killSurvivors SIGKILLs whatever is left of cmd's process group once cmd has
// exited, and reports whether anything was: a descendant that outlived the
// probe (and held its pipes through WaitDelay) must not keep running either.
func killSurvivors(cmd *exec.Cmd) bool {
	return cmd.Process != nil && syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) == nil
}
