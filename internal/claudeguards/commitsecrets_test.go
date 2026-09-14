package claudeguards

import (
	"strings"
	"testing"
)

func TestBadFilePatterns(t *testing.T) {
	block := []string{
		"id_rsa", "keys/id_ed25519.bak", "cert.pem", "server.key", "app.p12",
		"deploy.pfx", "site.crt", "ca.cer", "x.der", "release.jks", "app.keystore",
		"putty.ppk", "cluster.kubeconfig", ".env", ".env.local", "api/.netrc",
		"ssh/known_hosts", "ssh/authorized_keys",
	}
	pass := []string{".env.example", "src/main.go", "docs/keys.md", "monkey.ts", "envelope.env.example"}
	for _, f := range block {
		if !reBadFile.MatchString(f) || reEnvExample.MatchString(f) {
			t.Errorf("should block staged file %q", f)
		}
	}
	for _, f := range pass {
		if reBadFile.MatchString(f) && !reEnvExample.MatchString(f) {
			t.Errorf("should pass staged file %q", f)
		}
	}
}

func TestSecretPatterns(t *testing.T) {
	block := []string{
		"+-----BEGIN RSA PRIVATE KEY-----",
		"+aws_key = AKIAIOSFODNN7EXAMPLE",
		"+token: ghp_abcdefghijklmnopqrstuv123456",
		"+github_pat_11ABCDEFG0123456789abcdef",
		"+slack: xoxb-1234567890-abcdef",
		"+key = sk-ant-api03-abcdefghijklmnopqrst",
		"+g = AIzaSyA-abcdefghijklmnopqrstuvwxyz0123456",
		"+jwt eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIx",
	}
	pass := []string{"+const skill = 'sk-illful'", "+// mention AKIA keys in docs", "+x = 1"}
	for _, l := range block {
		if !reSecret.MatchString(l) {
			t.Errorf("should flag %q", l)
		}
	}
	for _, l := range pass {
		if reSecret.MatchString(l) {
			t.Errorf("should pass %q", l)
		}
	}
}

func TestPasswordAssignments(t *testing.T) {
	if !credentialAssignment(`+password = "hunter2hunter2"`) {
		t.Error("literal password assignment should flag")
	}
	for _, l := range []string{
		`+password = "${DB_PASSWORD}"`,
		`+api_key = "process.env.KEY"`,
		`+secret: "changeme-please"`,
		`+token = "<your-token-here>"`,
	} {
		if credentialAssignment(l) {
			t.Errorf("placeholder/env form should pass: %q", l)
		}
	}
}

// An env-var NAME constant is the KEY handed to os.Getenv, not the secret —
// flagging it blocked real commits (vitrinka t/1312). Suppression demands BOTH
// signals, so a credential that merely happens to be SCREAMING_SNAKE, or an
// env-named identifier holding a real token, keeps flagging.
func TestEnvVarNameConstantsAreNotCredentials(t *testing.T) {
	flag := []string{
		`+var password = "hunter2hunter2"`,
		`+var apiKey = "sk_live_9f8a7b6c5d4e3f2a1b"`,
		`+var secret = "correct horse battery staple"`,
		`+var accessToken = "aG9yc2ViYXR0ZXJ5c3RhcGxlMTIz"`,
		// SCREAMING_SNAKE value, but the identifier names no env var.
		`+var password = "ADMIN_PASSWORD"`,
		`+var secret = "SUPER_SECRET_VALUE"`,
		`+var apiKey = "MY_API_KEY_VALUE_9"`,
		`+var password = "A1B2_C3D4_E5F6"`,
		// Env-named identifier, but the value is a real token, not a var name.
		`+var envPassword = "sk_live_9f8a7b6c5d4e3f2a1b"`,
		// Caps with no underscore keeps its entropy: a base32 TOTP seed.
		`+var secret = "JBSWY3DPEHPK3PXP"`,
	}
	pass := []string{
		`+const EnvPassword = "POSTA_APP_PASSWORD"`,
		`+const EnvAPIKey = "FIXIT_API_KEY"`,
		`+var passwordEnv = "DB_PASSWORD"`,
	}
	for _, l := range flag {
		if !credentialAssignment(l) {
			t.Errorf("should flag %q", l)
		}
	}
	for _, l := range pass {
		if credentialAssignment(l) {
			t.Errorf("should pass %q", l)
		}
	}
}

// rePasswordSkip knew Python's os.environ and JS's process.env but not Go's,
// Java's or Deno's spelling, so the same "this is the env var's name" line
// flagged in Go and passed in Python.
func TestEnvAccessorSpellingsAreSkipped(t *testing.T) {
	const base = `+	"password": "SMTP_PASSWORD",`
	if !credentialAssignment(base) {
		t.Fatalf("control must flag without an env accessor: %q", base)
	}
	for _, accessor := range []string{"os.Getenv", "System.getenv", "Deno.env.get", "process.env", "os.environ"} {
		if l := base + " // read via " + accessor; credentialAssignment(l) {
			t.Errorf("%s form should pass: %q", accessor, l)
		}
	}
}

func TestCdPrefix(t *testing.T) {
	for cmd, want := range map[string]string{
		`cd /x/y && git commit -m m`:     "/x/y",
		`(cd "/a b" && git commit -m m)`: "/a b",
		`git commit -m m`:                "",
	} {
		got := ""
		if m := reCdPrefix.FindStringSubmatch(cmd); m != nil {
			got = strings.TrimSpace(m[1])
		}
		if got != want {
			t.Errorf("cd prefix of %q: got %q, want %q", cmd, got, want)
		}
	}
}

func TestPrivateIP(t *testing.T) {
	private := []string{"10.0.0.1", "127.0.0.1", "172.16.0.1", "172.31.9.9", "192.168.1.1", "169.254.0.1", "0.0.0.0"}
	public := []string{"95.216.27.220", "8.8.8.8", "172.32.0.1"}
	for _, ip := range private {
		if !rePrivateIP.MatchString(ip) {
			t.Errorf("%s should be private", ip)
		}
	}
	for _, ip := range public {
		if rePrivateIP.MatchString(ip) {
			t.Errorf("%s should be public", ip)
		}
	}
}
