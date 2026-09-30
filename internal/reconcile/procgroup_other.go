//go:build !unix

package reconcile

import "os/exec"

// killGroupOnCancel keeps exec.CommandContext's default (kill the process);
// the probe's WaitDelay still bounds a descendant holding its pipes.
func killGroupOnCancel(*exec.Cmd) {}

// killSurvivors has no process group to inspect here, so the orphan guarantees
// are unix-only (reconcile runs on the linux boxes): exec.ErrWaitDelay flags an
// exit-0 probe whose descendant outlived it, but one that exits nonzero past a
// live descendant reads as a plain failure — the vhost is still held, yet the
// sweep's breaker does not trip and the descendant is not killed.
func killSurvivors(*exec.Cmd) bool { return false }
