package gitkit

import (
	"slices"
	"strings"
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

// classify-paths reads its paths from git status, so a path in argv is
// refused rather than silently left unclassified — before any git call.
func TestClassifyPathsArgs(t *testing.T) {
	for _, argv := range [][]string{
		{"--repo", "/abs/repo"}, // push-all SKILL.md, commands/dirty.md
		{"--repo=/abs/repo", "--json"},
		nil,
	} {
		if _, _, err := classifyPathsArgs.parse("classify-paths", argv); err != nil {
			t.Errorf("%q: %v", argv, err)
		}
	}
	for want, argv := range map[string][]string{
		"unknown argument --staged":  {"--staged", "--repo", "/abs/repo"},
		`unexpected argument ".env"`: {"--repo", "/abs/repo", ".env"},
		"--repo needs a value":       {"--repo="}, // never the cwd's repository
	} {
		var stderr strings.Builder
		if code := runClassifyPaths(argv, &strings.Builder{}, &stderr); code != 1 || !strings.Contains(stderr.String(), want) || !strings.Contains(stderr.String(), classifyPathsArgs.usage) {
			t.Errorf("%q: %d %q, want %q + usage", argv, code, stderr.String(), want)
		}
	}
}

func TestParsePorcelain(t *testing.T) {
	got := parsePorcelain(" M src/a.ts\nR  old.ts -> new.ts\n?? \"with space.txt\"\n\n")
	if want := []string{"src/a.ts", "new.ts", "with space.txt"}; !slices.Equal(got, want) {
		t.Fatalf("parsePorcelain = %q, want %q", got, want)
	}
}
