package reconcile

import (
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
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
func (e *Engine) certsPresent(rp, src string) bool {
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
	argv := append(append([]string(nil), probe...), paths...)
	err = e.runCmd(e.M.Hooks.Nginx.Workdir, argv)
	if err == nil {
		return true
	}
	// runCmd returns the bare *exec.ExitError only when the probe wrote nothing
	// to stderr: a silent exit 1 is the probe's own "a path is missing".
	var exit *exec.ExitError
	missing := errors.As(err, &exit) && exit.ExitCode() == 1 && err == error(exit)
	if !missing {
		e.logErr("certificate probe for %s failed (%v) — held until it answers", rp, err)
	}
	return false
}
