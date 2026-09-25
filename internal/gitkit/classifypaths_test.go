package gitkit

import (
	"slices"
	"testing"
)

func TestClassifyPath(t *testing.T) {
	cases := map[string][]string{
		"secret": {
			".env", ".env.local", "apps/api/.env.production", "config/prod.env",
			"server.key", "cert.pem", "keystore.p12", "id_rsa", "deploy/id_ed25519",
			"credentials", "aws-credentials.json", "secrets.json", ".npmrc", "auth.token",
		},
		"artifact": {
			"allure-results/x.json", "allure-report/index.html", "playwright-report/index.html",
			"test-results/run/trace.zip", "coverage/lcov.info", "node_modules/x/index.js",
			".DS_Store", "logs/app.log", "apps/api/__pycache__/m.pyc",
		},
		"commit": {
			// env templates, a public key, and source that merely mentions token/secret
			".env.example", ".env.sample", "apps/web/.env.template", "id_rsa.pub",
			"src/auth/tokenizer.ts", "src/lib/secretManager.service.ts", "src/index.ts", "README.md",
		},
	}
	for want, paths := range cases {
		for _, p := range paths {
			if got := ClassifyPath(p); got != want {
				t.Errorf("ClassifyPath(%q) = %q, want %q", p, got, want)
			}
		}
	}
}

func TestClassifyStatusPreservesOrder(t *testing.T) {
	r := ClassifyStatus([]string{"src/a.ts", ".env", "allure-results/r.json", "README.md", "server.key"})
	if !slices.Equal(r.Commit, []string{"src/a.ts", "README.md"}) ||
		!slices.Equal(r.Secrets, []string{".env", "server.key"}) ||
		!slices.Equal(r.Artifacts, []string{"allure-results/r.json"}) {
		t.Fatalf("partition = %+v", r)
	}
}

func TestParsePorcelain(t *testing.T) {
	got := parsePorcelain(" M src/a.ts\nR  old.ts -> new.ts\n?? \"with space.txt\"\n\n")
	if want := []string{"src/a.ts", "new.ts", "with space.txt"}; !slices.Equal(got, want) {
		t.Fatalf("parsePorcelain = %q, want %q", got, want)
	}
}
