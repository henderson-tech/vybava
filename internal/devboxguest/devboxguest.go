// Package devboxguest answers one question: does this process run inside a
// Devbox guest (the devbox-vm on Devulinka or Produlinka) rather than on the
// owner's Mac? A Devbox portal session runs Claude Code there with the Mac's
// Claudik settings, so the hooks those settings wire meet a different
// machine: claude-guards' Mac rules (route this command to the Devbox, cap
// this Mac's dev servers, reap this Mac's orphans) must stand down — the
// command already runs on the Devbox, and the process table is every
// workspace's — and memo must not write the personal homes of the box's
// pull-only Claudik clone.
//
// A test binary that exercises either side points Marker at a path of its
// own from TestMain: vybava's suites run ON a Devbox guest (`devbox run
// verify`), where the real marker exists.
package devboxguest

import "os"

// Marker is the file every Devbox guest carries: the box's runtime config
// (/etc/devbox/runtime.env), written by the infra repo's deploy-guest.sh and
// read by the guest's devbox. Only its existence is consulted, never its
// contents.
var Marker = "/etc/devbox/runtime.env"

// Detected reports whether Marker exists.
func Detected() bool {
	_, err := os.Stat(Marker)
	return err == nil
}
