package reconcile

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// The TLS certificate hold, ported from the bash twins' `nginx_certs_present`
// (produlinka-infra / devulinka-infra scripts/infra-reconcile/reconcile.sh):
// a TLS vhost converged before its ACME issue fails `nginx -t` for the WHOLE
// proxy, and the nginx transaction would then roll back every conf file the
// tick touched — unrelated vhosts included — every tick until the certificate
// exists. Instead the one vhost is held (never copied, never probed again once
// in sync) and every other file lands; the next tick after the issue lands it.

// sslCertLine is the bash engine's extraction verbatim:
//
//	sed -nE 's/^[[:space:]]*ssl_certificate(_key)?[[:space:]]+([^;[:space:]]+)[[:space:]]*;.*/\2/p'
//
// one path per directive line, taken literally: no include is followed, no
// variable is expanded, a commented or multi-directive line is not read.
var sslCertLine = regexp.MustCompile(`^[[:space:]]*ssl_certificate(_key)?[[:space:]]+([^;[:space:]]+)[[:space:]]*;`)

// sslCertPaths lists the certificate paths a vhost names, in file order.
func sslCertPaths(conf []byte) []string {
	var paths []string
	for _, line := range strings.Split(string(conf), "\n") {
		if m := sslCertLine.FindStringSubmatch(line); m != nil {
			paths = append(paths, m[2])
		}
	}
	return paths
}

// certsPresent reports whether the repo vhost src may move onto the box. A
// vhost naming no certificate, or a manifest with no certs_present probe,
// always may; otherwise the probe runs with the paths appended. Any probe
// failure holds (fail closed, like bash); one that is more than "a path is
// missing" (the container is down, the binary is absent) is logged on stderr
// so a hold with a broken probe is never silent.
//
// The probe runs inside the tick, `status` and the status page (which holds
// its lock for the sweep), so it has a deadline, and the sweep a breaker: once
// one probe expires, every later TLS vhost of the sweep holds unprobed — N
// drifting vhosts behind a hung `docker compose exec` cost one deadline.
func (s *sweep) certsPresent(rp, src string) bool {
	e := s.e
	probe := e.M.Hooks.Nginx.CertsPresent
	if len(probe) == 0 {
		return true
	}
	conf, err := os.ReadFile(src)
	if err != nil {
		return true // bash: sed on an unreadable file finds no paths; the copy reports the failure
	}
	paths := sslCertPaths(conf)
	if len(paths) == 0 {
		return true
	}
	if s.certProbeExpired {
		if !s.certSkipLogged {
			s.certSkipLogged = true
			e.logErr("certificate probe skipped for %s and every later TLS vhost of this sweep (an earlier probe did not answer) — held", rp)
		}
		return false
	}
	argv := append(append([]string(nil), probe...), paths...)
	timeout := e.certProbeTimeout()
	err = runProbe(e.M.Hooks.Nginx.Workdir, argv, timeout)
	if err == nil {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		s.certProbeExpired = true
		e.logErr("certificate probe for %s did not answer within %s — held until it answers", rp, timeout)
		return false
	}
	if errors.Is(err, errProbeOrphaned) {
		s.certProbeExpired = true
		e.logErr("certificate probe for %s exited but left a child holding its output — held until it answers", rp)
		return false
	}
	// runQuiet returns the bare *exec.ExitError only when the probe wrote nothing
	// to stderr: a silent exit 1 is the probe's own "a path is missing".
	var exit *exec.ExitError
	missing := errors.As(err, &exit) && exit.ExitCode() == 1 && err == error(exit)
	if !missing {
		e.logErr("certificate probe for %s failed (%v) — held until it answers", rp, err)
	}
	return false
}

// certProbeWaitDelay bounds Wait once the probe's context is done or its
// process exited: a descendant that escaped the kill and still holds the
// stderr pipe cannot keep the sweep blocked.
const certProbeWaitDelay = time.Second

// errProbeOrphaned: the probe exited, but a descendant outlived it (and was
// killed) — Wait sat out certProbeWaitDelay on its pipes whatever the exit code,
// so the sweep's breaker trips as on expiry.
var errProbeOrphaned = errors.New("certificate probe left a child running")

// runProbe runs the certs_present probe under a deadline; an expired probe is
// context.DeadlineExceeded, one that outlived its own exit is errProbeOrphaned,
// anything else is runQuiet's error shape. The probe gets its own process group
// so expiry kills `docker compose exec` together with the compose plugin it
// runs as a child (procgroup_unix.go). Worst case: timeout + certProbeWaitDelay.
func runProbe(dir string, argv []string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.WaitDelay = certProbeWaitDelay
	killGroupOnCancel(cmd)
	err := runQuiet(cmd)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	// reap first, whatever the error says: an exit-0 probe past a live child is
	// ErrWaitDelay, and its child must not outlive the sweep either
	if orphaned := killSurvivors(cmd); orphaned || errors.Is(err, exec.ErrWaitDelay) {
		return errProbeOrphaned
	}
	return err
}
