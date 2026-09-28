//go:build !unix

package reconcile

import "os/exec"

// killGroupOnCancel keeps exec.CommandContext's default (kill the process);
// the probe's WaitDelay still bounds a descendant holding its pipes.
func killGroupOnCancel(*exec.Cmd) {}
