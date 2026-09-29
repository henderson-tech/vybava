//go:build !unix

package reconcile

import "os/exec"

// killGroupOnCancel keeps exec.CommandContext's default (kill the process);
// the probe's WaitDelay still bounds a descendant holding its pipes.
func killGroupOnCancel(*exec.Cmd) {}

// killSurvivors has no process group to inspect here; exec.ErrWaitDelay alone
// flags a probe whose descendant outlived it.
func killSurvivors(*exec.Cmd) bool { return false }
