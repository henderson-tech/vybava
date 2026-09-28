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
	"testing"
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
