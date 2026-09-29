package reconcile

// The bash twins' TLS certificate hold (produlinka-infra / devulinka-infra
// reconcile.sh nginx_certs_present + map-paths.sh hook_nginx_certs_present):
// same extraction, same probe argv, same log line and digest block.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestSSLCertPaths(t *testing.T) {
	for _, tc := range []struct {
		name, conf string
		want       []string
	}{
		{"certificate and key", "server {\n    ssl_certificate /c/full.pem;\n    ssl_certificate_key /c/key.pem;\n}\n", []string{"/c/full.pem", "/c/key.pem"}},
		{"tabs, space before the semicolon, trailing comment", "\tssl_certificate\t/c/a.pem ;\nssl_certificate_key /c/k.pem; # note\n", []string{"/c/a.pem", "/c/k.pem"}},
		{"every server block's lines, duplicates kept", "ssl_certificate /c/a.pem;\nssl_certificate /c/a.pem;\n", []string{"/c/a.pem", "/c/a.pem"}},
		{"CRLF and no newline at EOF", "ssl_certificate /c/a.pem;\r\nssl_certificate_key /c/k.pem;", []string{"/c/a.pem", "/c/k.pem"}},
		{"variables and includes stay literal", "ssl_certificate $ssl_server_name.crt;\ninclude /etc/nginx/ssl.inc;\n", []string{"$ssl_server_name.crt"}},
		{"commented directive", "# ssl_certificate /c/a.pem;\n", nil},
		{"a second directive on the line is not read", "listen 443 ssl; ssl_certificate /c/a.pem;\n", nil},
		{"no semicolon", "ssl_certificate /c/a.pem\n", nil},
		{"other ssl_certificate_* directives", "ssl_certificate_compression on;\nssl_certificate_by_lua_block {\n}\n", nil},
		{"plain vhost", "server { listen 80; }\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sslCertPaths([]byte(tc.conf)); !slices.Equal(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// map-paths.sh hook_nginx_certs_present per box: the twins probe inside the
// proxy container; webulinka's bash variant has no certificate hold.
func TestFixtureManifestsCarryTheBashCertProbe(t *testing.T) {
	bash := []string{"docker", "compose", "exec", "-T", "nginx", "sh", "-c", `for p; do [ -e "$p" ] || exit 1; done`, "sh"}
	for name, want := range map[string][]string{"produlinka": bash, "devulinka": bash, "webulinka": nil} {
		if got := loadFixtureManifest(t, name).Hooks.Nginx.CertsPresent; !slices.Equal(got, want) {
			t.Errorf("%s certs_present = %q, want %q", name, got, want)
		}
	}
}

func TestTLSVhostCertHold(t *testing.T) {
	const (
		tlsVhost = "server {\n  listen 443 ssl;\n  ssl_certificate certs/full.pem;\n  ssl_certificate_key certs/key.pem;\n}\n"
		heldLine = "] HELD (TLS vhost, certificate missing on this box — issue it first) (1): nginx/tls.conf; \n"
		heldDig  = "HELD (TLS vhost without its certificate — issue it, next tick lands it):\n  nginx/tls.conf\n"
	)
	for _, tc := range []struct {
		name     string
		certs    []string // present under the probe's workdir
		noProbe  bool     // manifest without certs_present (webulinka)
		mode     string
		live     []string // conf.d files after the tick
		certHeld []string
		pending  []string
		errors   int
		probes   int
	}{
		{name: "certificate and key present", certs: []string{"full.pem", "key.pem"}, mode: "converge",
			live: []string{"site.conf", "tls.conf"}, probes: 1},
		{name: "certificate missing", certs: []string{"key.pem"}, mode: "converge",
			live: []string{"site.conf"}, certHeld: []string{"nginx/tls.conf"}, probes: 1},
		{name: "key only missing", certs: []string{"full.pem"}, mode: "converge",
			live: []string{"site.conf"}, certHeld: []string{"nginx/tls.conf"}, probes: 1},
		// without the hold the vhost lands, nginx -t fails and the transaction
		// rolls back the unrelated site.conf too — what the hold prevents
		{name: "no probe configured", noProbe: true, mode: "converge",
			errors: 1},
		{name: "report mode", mode: "report",
			certHeld: []string{"nginx/tls.conf"}, pending: []string{"nginx/site.conf (new file)"}, probes: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBox(t, map[string]string{"nginx/site.conf": "server { listen 80; }\n", "nginx/tls.conf": tlsVhost})
			// nginx itself: a live TLS vhost whose files are absent fails -t
			mustT(t, os.WriteFile(filepath.Join(b.root, "bin", "nginx-test"), []byte("#!/bin/sh\n"+
				"[ -f "+b.root+"/opt/conf.d/tls.conf ] || exit 0\n"+
				"[ -e "+b.root+"/certs/full.pem ] && [ -e "+b.root+"/certs/key.pem ] || { echo 'cannot load certificate' >&2; exit 1; }\n"), 0o755))
			mustT(t, os.MkdirAll(filepath.Join(b.root, "certs"), 0o755))
			for _, c := range tc.certs {
				mustT(t, os.WriteFile(filepath.Join(b.root, "certs", c), nil, 0o600))
			}
			b.m.Hooks.Nginx.Workdir = b.root // relative cert paths resolve where the probe runs
			if !tc.noProbe {
				b.m.Hooks.Nginx.CertsPresent = []string{"sh", "-c", `echo "$*" >> probes; for p; do [ -e "$p" ] || exit 1; done`, "sh"}
			}
			mustT(t, os.WriteFile(b.m.ModeFile, []byte(tc.mode+"\n"), 0o644))
			e := b.engine()

			res, _ := e.Run()
			var live []string
			for _, f := range []string{"site.conf", "tls.conf"} {
				if b.live("conf.d/"+f) != "" {
					live = append(live, f)
				}
			}
			probes, _ := os.ReadFile(filepath.Join(b.root, "probes"))
			if !slices.Equal(live, tc.live) || !slices.Equal(res.CertHeld, tc.certHeld) || !slices.Equal(res.Pending, tc.pending) ||
				len(res.Errors) != tc.errors || strings.Count(string(probes), "\n") != tc.probes {
				t.Fatalf("live=%q cert_held=%q pending=%q errors=%v probes=%q\n%s", live, res.CertHeld, res.Pending, res.Errors, probes, b.out.String())
			}
			if tc.probes > 0 && string(probes) != "certs/full.pem certs/key.pem\n" {
				t.Fatalf("probe argv = %q, want the vhost's paths appended", probes)
			}
			held := len(tc.certHeld) > 0
			if held != strings.Contains(b.out.String(), heldLine) || held != strings.Contains(res.Digest, heldDig) || slices.Contains(res.Held, "nginx/tls.conf") {
				t.Fatalf("held=%v but log/digest disagree:\n%s\ndigest:\n%s", held, b.out.String(), res.Digest)
			}
			if held && tc.mode == "converge" && len(res.Errors) != 0 {
				t.Fatalf("a held vhost failed the tick: %v", res.Errors)
			}
			metrics, _ := os.ReadFile(b.m.MetricsFile)
			if want := "infra_reconcile_cert_held " + strconv.Itoa(len(tc.certHeld)) + "\n"; !strings.Contains(string(metrics), want) {
				t.Fatalf("metrics lack %q:\n%s", want, metrics)
			}

			// status --json (the parity script's input) classifies the same way
			rep, err := e.StatusReport(5)
			mustT(t, err)
			raw, err := json.Marshal(rep)
			mustT(t, err)
			if !slices.Equal(rep.CertHeld, tc.certHeld) || (held && rep.Sync != "held") || !strings.Contains(string(raw), `"cert_held":[`) {
				t.Fatalf("status = %s", raw)
			}

			// issuing the certificate lands the vhost on the next tick; an
			// in-sync vhost is never probed again
			if held && tc.mode == "converge" {
				for _, c := range []string{"full.pem", "key.pem"} {
					mustT(t, os.WriteFile(filepath.Join(b.root, "certs", c), nil, 0o600))
				}
				res, err := e.Run()
				mustT(t, err)
				if len(res.CertHeld) != 0 || b.live("conf.d/tls.conf") != tlsVhost {
					t.Fatalf("issued certificate did not land the vhost: %+v", res)
				}
				before, _ := os.ReadFile(filepath.Join(b.root, "probes"))
				_, _ = e.Run()
				after, _ := os.ReadFile(filepath.Join(b.root, "probes"))
				if string(after) != string(before) {
					t.Fatalf("in-sync vhost probed again: %q", after)
				}
			}
		})
	}
}

// A hung probe (a stuck `docker compose exec`) holds its vhost without
// stalling the tick or the status sweep the page serves under its lock: one
// deadline per probe, and once one expires the rest of the sweep holds TLS
// vhosts unprobed. A probe that answers classifies every file as before.
func TestCertProbeDeadline(t *testing.T) {
	const deadline = 400 * time.Millisecond
	// the status sweep (what the page runs under its lock) costs ~60ms beside its
	// probes, so one expiry fits and two do not; the tick adds a git fetch and
	// hooks (~1s on a loaded laptop), so its bound only proves it never stalls —
	// the probe count proves its breaker
	const statusBound, tickBound = 2 * deadline, deadline + 5*time.Second
	const hang = "sleep 30; :" // sleep is sh's child: it holds the probe's pipes after sh dies
	expired := "certificate probe for nginx/a.conf did not answer within 400ms — held until it answers"
	skipped := "certificate probe skipped for nginx/b.conf and every later TLS vhost of this sweep (an earlier probe did not answer) — held"
	orphaned := "certificate probe for nginx/a.conf exited but left a child holding its output — held until it answers"
	for _, tc := range []struct {
		name     string
		probe    string   // runs after the call is recorded
		vhosts   []string // drifting TLS vhosts beside the plain site.conf
		certs    []string // present under the probe's workdir
		live     []string // conf.d files after the tick
		certHeld []string
		probes   [2]int   // run, then status (an in-sync vhost is not probed)
		logs     []string // the sweep's certificate-probe stderr lines, in order
		grace    bool     // the case sits out certProbeWaitDelay on a held pipe, under a deadline that outlasts it (as 3s does in production)
	}{
		{name: "probe past the deadline", probe: hang, vhosts: []string{"a"},
			live: []string{"site.conf"}, certHeld: []string{"nginx/a.conf"}, probes: [2]int{1, 1}, logs: []string{expired}},
		{name: "two vhosts cost one deadline", probe: hang, vhosts: []string{"a", "b"},
			live: []string{"site.conf"}, certHeld: []string{"nginx/a.conf", "nginx/b.conf"}, probes: [2]int{1, 1}, logs: []string{expired, skipped}},
		// a background child keeps the pipes after sh exits: exit 0 surfaces as
		// exec.ErrWaitDelay, exit 1 as a plain ExitError only the group check catches
		{name: "a probe exiting 0 past a live child costs one grace", probe: "sleep 30 & echo $! >> kids; exit 0", vhosts: []string{"a", "b"}, grace: true,
			live: []string{"site.conf"}, certHeld: []string{"nginx/a.conf", "nginx/b.conf"}, probes: [2]int{1, 1}, logs: []string{orphaned, skipped}},
		{name: "a probe exiting 1 past a live child costs one grace", probe: "sleep 30 & echo $! >> kids; exit 1", vhosts: []string{"a", "b"}, grace: true,
			live: []string{"site.conf"}, certHeld: []string{"nginx/a.conf", "nginx/b.conf"}, probes: [2]int{1, 1}, logs: []string{orphaned, skipped}},
		{name: "an answering probe classifies every vhost", probe: `for p; do [ -e "$p" ] || exit 1; done`, vhosts: []string{"a", "b"}, certs: []string{"a.pem"},
			live: []string{"a.conf", "site.conf"}, certHeld: []string{"nginx/b.conf"}, probes: [2]int{2, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"nginx/site.conf": "server { listen 80; }\n"}
			for _, v := range tc.vhosts {
				files["nginx/"+v+".conf"] = "server {\n  listen 443 ssl;\n  ssl_certificate certs/" + v + ".pem;\n}\n"
			}
			b := newBox(t, files)
			mustT(t, os.MkdirAll(filepath.Join(b.root, "certs"), 0o755))
			for _, c := range tc.certs {
				mustT(t, os.WriteFile(filepath.Join(b.root, "certs", c), nil, 0o600))
			}
			b.m.Hooks.Nginx.Workdir = b.root
			b.m.Hooks.Nginx.CertsPresent = []string{"sh", "-c", `echo "$*" >> probes; ` + tc.probe, "sh"}
			e := b.engine()
			e.CertProbeTimeout = deadline
			if tc.grace {
				e.CertProbeTimeout = 2 * certProbeWaitDelay
			}

			sweep := func(name string, bound time.Duration, probed int, run func() []string) {
				t.Helper()
				if tc.grace {
					bound += certProbeWaitDelay
				}
				b.out.Reset()
				_ = os.Remove(filepath.Join(b.root, "probes"))
				start := time.Now()
				certHeld := run()
				took := time.Since(start)
				probes, _ := os.ReadFile(filepath.Join(b.root, "probes"))
				var logs []string
				for _, line := range strings.Split(b.out.String(), "\n") {
					if _, msg, ok := strings.Cut(line, "] "); ok && strings.HasPrefix(msg, "certificate probe") {
						logs = append(logs, msg)
					}
				}
				if took > bound || !slices.Equal(certHeld, tc.certHeld) || strings.Count(string(probes), "\n") != probed || !slices.Equal(logs, tc.logs) {
					t.Fatalf("%s: took %s (bound %s) cert_held=%q probes=%q logs=%q\n%s", name, took, bound, certHeld, probes, logs, b.out.String())
				}
			}
			sweep("run", tickBound, tc.probes[0], func() []string {
				res, err := e.Run()
				mustT(t, err) // a held vhost is never an error
				return res.CertHeld
			})
			var live []string
			for _, f := range []string{"a.conf", "b.conf", "site.conf"} {
				if b.live("conf.d/"+f) != "" {
					live = append(live, f)
				}
			}
			if !slices.Equal(live, tc.live) {
				t.Fatalf("live = %q, want %q", live, tc.live)
			}
			sweep("status", statusBound, tc.probes[1], func() []string {
				rep, err := e.StatusReport(5)
				mustT(t, err)
				return rep.CertHeld
			})
			// every sweep's orphan is killed, not left running: a signal-0 probe
			// succeeds on a live process (and briefly on a zombie init has yet to reap)
			kids, _ := os.ReadFile(filepath.Join(b.root, "kids"))
			for _, f := range strings.Fields(string(kids)) {
				pid, _ := strconv.Atoi(f)
				p, err := os.FindProcess(pid)
				for end := time.Now().Add(time.Second); err == nil && p.Signal(syscall.Signal(0)) == nil; {
					if time.Now().After(end) {
						t.Fatalf("the probe's child %d outlived its sweep", pid)
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
		})
	}
}
