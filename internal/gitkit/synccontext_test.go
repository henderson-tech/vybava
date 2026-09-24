package gitkit

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseConfig(t *testing.T) {
	cfg := parseConfig(strings.Join([]string{
		"# comment", "", "DEFAULT_BRANCH=main", "DB_MIGRATE_CMD = pnpm db:migrate ",
		`RESTART_CMD="docker compose restart api"`, "MERGE_STRATEGY=rebase", "junk-line-without-eq",
	}, "\n"))
	want := gitConfig{"DEFAULT_BRANCH": "main", "DB_MIGRATE_CMD": "pnpm db:migrate", "RESTART_CMD": "docker compose restart api", "MERGE_STRATEGY": "rebase"}
	if !maps.Equal(cfg, want) {
		t.Fatalf("parseConfig = %v", cfg)
	}
}

func TestPackageManagerAndInstallCmd(t *testing.T) {
	for files, want := range map[string]string{
		"pnpm-lock.yaml,package.json": "pnpm", "bun.lock": "bun", "yarn.lock": "yarn", "package-lock.json": "npm", "README.md": "",
	} {
		if got := packageManager(strings.Split(files, ",")); got != want {
			t.Errorf("packageManager(%s) = %q, want %q", files, got, want)
		}
	}
	if *installCmdForLockfile([]string{"package-lock.json"}) != "npm install" || installCmdForLockfile(nil) != nil {
		t.Error("installCmdForLockfile")
	}
}

func TestIsLocalDBURL(t *testing.T) {
	for raw, want := range map[string]bool{
		"postgres://u:p@localhost:5432/app":            true,
		"postgres://u:p@127.0.0.1:5432/app":            true,
		"postgres://u:p@db:5432/app":                   true, // docker-compose service
		"postgres://u:p@host.docker.internal:5432/app": true,
		"postgres://u:p@[::1]:5432/app":                true,
		"postgres://u:p@db.prod.example.com:5432/app":  false,
		"postgres://u:p@203.0.113.10:5432/app":         false,
		// A credential-shaped @localhost: before the real host is not local.
		"postgres://user@localhost:5432@prod.example.com/app": false,
		"": false,
	} {
		if got := isLocalDBURL(raw); got != want {
			t.Errorf("isLocalDBURL(%q) = %v", raw, got)
		}
	}
	if h := dbHostOf("postgres://user:secret@db.prod.example.com:5432/app"); h == nil || *h != "db.prod.example.com" {
		t.Errorf("dbHostOf = %v", h)
	}
	if h := dbHostOf("not-a-url"); h != nil {
		t.Errorf("dbHostOf(not-a-url) = %q", *h)
	}
}

func TestDetectMode(t *testing.T) {
	// Branch identity decides: a feature branch in the primary clone is branch
	// mode, and a detached HEAD never masquerades as the default branch.
	for _, tc := range [][3]string{{"main", "main", "main"}, {"master", "master", "main"}, {"work/tz-fix", "main", "branch"}, {"HEAD", "main", "branch"}} {
		if got := detectMode(tc[0], tc[1]); got != tc[2] {
			t.Errorf("detectMode(%s, %s) = %s", tc[0], tc[1], got)
		}
	}
}

func TestGlobToRegExp(t *testing.T) {
	for _, tc := range []struct {
		glob, path string
		want       bool
	}{
		{"*.ts", "a.ts", true}, {"*.ts", "src/a.ts", false}, {"**/*.ts", "src/deep/a.ts", true},
		{"**/generated/**", "apps/web/generated/api.ts", true}, {"openapi*.{json,yaml}", "openapi.json", true},
		{"openapi*.{json,yaml}", "openapi-v2.yaml", true}, {"openapi*.{json,yaml}", "openapi.ts", false},
		{"a.ts", "axts", false}, // a literal dot is not the regex wildcard
	} {
		if got := globToRegExp(tc.glob).MatchString(tc.path); got != tc.want {
			t.Errorf("globToRegExp(%q).Match(%q) = %v", tc.glob, tc.path, got)
		}
	}
}

func TestIsGeneratedPath(t *testing.T) {
	cfg := []string{"packages/api-client/src/generated", "apps/web/src/api.gen.ts"}
	d := defaultGeneratedGlobs
	for _, tc := range []struct {
		path     string
		patterns []string
		want     bool
	}{
		{"packages/api-client/src/generated/hooks.ts", cfg, true},
		{"packages/api-client/src/generated", cfg, true},
		{"apps/web/src/api.gen.ts", cfg, true},
		// prefix match respects segment boundaries
		{"packages/api-client/src/generated-by-hand.ts", cfg, false},
		{"packages/api-client/src/client.ts", cfg, false},
		{"apps/web/generated/api.ts", d, true}, {"src/__generated__/gql.ts", d, true},
		{"src/deep/client.gen.ts", d, true}, {"src/types.generated.d.ts", d, true},
		{"openapi.json", d, true}, {"docs/openapi-v1.yaml", d, true}, {"prisma/client/index.d.ts", d, true},
		// hand-written code that merely mentions the words stays hand-merged
		{"src/generateReport.ts", d, false}, {"src/pricing.ts", d, false}, {"src/generator/rules.ts", d, false},
	} {
		if got := isGeneratedPath(tc.path, tc.patterns); got != tc.want {
			t.Errorf("isGeneratedPath(%q) = %v", tc.path, got)
		}
	}
}

func ptr(s string) *string { return &s }

func TestFreezeConfigAppendsOnlyUnsetKeysIdempotently(t *testing.T) {
	existing := "# hand written\nDEFAULT_BRANCH=develop\n"
	resolved := []kv{
		{"DEFAULT_BRANCH", ptr("main")}, // already set → user's value wins
		{"INSTALL_CMD", ptr("bun install")},
		{"REGEN_CMD", nil},      // unresolved → never written
		{"VERIFY_CMD", ptr("")}, // empty → never written
		{"GENERATED_PATHS", ptr("**/generated/**")},
	}
	text, added := freezeConfig(existing, resolved, "2026-07-29")
	want := existing + "\n# auto-detected by /sync 2026-07-29 — edit freely, /sync never overwrites\nINSTALL_CMD=bun install\nGENERATED_PATHS=**/generated/**\n"
	if !slices.Equal(added, []string{"INSTALL_CMD", "GENERATED_PATHS"}) || text != want {
		t.Fatalf("first freeze = %q %v", text, added)
	}
	again, added := freezeConfig(text, resolved, "2026-07-30")
	if len(added) != 0 || again != text {
		t.Fatalf("re-freeze changed the file: %q %v", again, added)
	}
	empty, added := freezeConfig("", []kv{{"INSTALL_CMD", ptr("pnpm install")}, {"RESTART_CMD", nil}}, "2026-07-29")
	if !slices.Equal(added, []string{"INSTALL_CMD"}) || parseConfig(empty)["INSTALL_CMD"] != "pnpm install" {
		t.Fatalf("empty freeze = %q", empty)
	}
}

func TestReadGitConfigOverlaysLocal(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, ".claude/.claude.git.config"), []byte("DEFAULT_BRANCH=main\nMERGE_METHOD=squash\n"), 0o644)
	os.WriteFile(filepath.Join(root, ".claude/.claude.git.config.local"), []byte("DEFAULT_BRANCH=devlp\n"), 0o644)
	if cfg, found := readGitConfig(root); !found || !maps.Equal(cfg, gitConfig{"DEFAULT_BRANCH": "devlp", "MERGE_METHOD": "squash"}) {
		t.Fatalf("readGitConfig = %v %v", cfg, found)
	}
	if cfg, found := readGitConfig(filepath.Join(root, "nope")); found || len(cfg) != 0 {
		t.Fatalf("missing root = %v %v", cfg, found)
	}
}
