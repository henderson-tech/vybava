package gitkit

import (
	"encoding/json"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
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
	if cfg, found, err := readGitConfig(root); err != nil || !found || !maps.Equal(cfg, gitConfig{"DEFAULT_BRANCH": "devlp", "MERGE_METHOD": "squash"}) {
		t.Fatalf("readGitConfig = %v %v", cfg, found)
	}
	if cfg, found, err := readGitConfig(filepath.Join(root, "nope")); err != nil || found || len(cfg) != 0 {
		t.Fatalf("missing root = %v %v", cfg, found)
	}
}

// The verb end to end: key order, nulls, script and DB detection, and a
// --freeze that writes only the unset keys.
func TestSyncContextVerb(t *testing.T) {
	root := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "develop"}, {"-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "c1"}} {
		if out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	write := func(name, body string) {
		os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0o755)
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".claude/.claude.git.config", "DEFAULT_BRANCH=main\nMERGE_STRATEGY=Rebase\n")
	write("package.json", `{"scripts":{"db:migrate":"x","typecheck":"tsc","generate":""}}`)
	write("bun.lock", "")
	write(".env", "DATABASE_URL=\"postgres://u:p@db:5432/app\"\r\n")
	t.Setenv("DATABASE_URL", "")
	t.Setenv("GIT_SKILL_REPO", "")

	var stdout, stderr strings.Builder
	if code := runSyncContext([]string{"--repo", root, "--freeze"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	top, _ := execFile(execOpts{dir: root}, "git", "rev-parse", "--show-toplevel")
	top = strings.TrimSpace(top)
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout.String()), &got); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"configFound": true, "mode": "branch", "currentBranch": "develop", "upstream": nil, "defaultBranch": "main",
		"verifyCmd": "bun run typecheck", "regenCmd": nil, "migrateCmd": "bun run db:migrate", "installCmd": "bun install",
		"mergeStrategy": "rebase", "packageManager": "bun", "dbHost": "db", "localDbOk": true, "configComplete": false,
	} {
		if !reflect.DeepEqual(got[key], want) {
			t.Errorf("%s = %v, want %v", key, got[key], want)
		}
	}
	keys := regexp.MustCompile(`(?m)^  "(\w+)":`).FindAllStringSubmatch(stdout.String(), -1)
	if len(keys) != 26 || keys[0][1] != "configFound" || keys[25][1] != "runAfterSync" {
		t.Errorf("wire keys = %v", keys)
	}
	frozen, _ := os.ReadFile(filepath.Join(root, ".claude/.claude.git.config"))
	if !strings.HasPrefix(string(frozen), "DEFAULT_BRANCH=main\nMERGE_STRATEGY=Rebase\n\n# auto-detected by /sync ") ||
		!strings.Contains(string(frozen), "INSTALL_CMD=bun install\nGENERATED_PATHS=") || strings.Contains(string(frozen), "REGEN_CMD") {
		t.Errorf("frozen config = %q", frozen)
	}
	if want := "freeze: wrote INSTALL_CMD, GENERATED_PATHS, VERIFY_CMD, DB_MIGRATE_CMD to " + filepath.Join(top, ".claude/.claude.git.config") + "\n"; stderr.String() != want {
		t.Errorf("stderr = %q, want %q", stderr.String(), want)
	}

	// A config that exists but cannot be read fails; it is never an empty config.
	os.Remove(filepath.Join(root, ".claude/.claude.git.config"))
	os.Mkdir(filepath.Join(root, ".claude/.claude.git.config.local"), 0o755)
	stderr.Reset()
	if code := runSyncContext([]string{"--repo", root}, io.Discard, &stderr); code != 1 || stderr.String() != "error: EISDIR: illegal operation on a directory, read\n" {
		t.Errorf("unreadable config: %d %q", code, stderr.String())
	}
}

// Unreadable is never absent: a package.json that cannot be read warns as a
// malformed one does; a config behind an untraversable directory, or an
// .env that cannot be read, fails.
func TestUnreadableInputsAreReported(t *testing.T) {
	root := t.TempDir()
	exec.Command("git", "init", "-q", root).Run()
	os.Mkdir(filepath.Join(root, "package.json"), 0o755)
	t.Setenv("GIT_SKILL_REPO", "")
	var stderr strings.Builder
	if code := runSyncContext([]string{"--repo", root}, io.Discard, &stderr); code != 0 ||
		stderr.String() != "warn: could not parse package.json (EISDIR: illegal operation on a directory, read)\n" {
		t.Errorf("package.json dir: %d %q", code, stderr.String())
	}

	claude := filepath.Join(root, ".claude")
	os.Mkdir(claude, 0o755)
	os.WriteFile(filepath.Join(claude, ".claude.git.config"), []byte("AFTER_MERGE_CMD=x\n"), 0o644)
	os.Chmod(claude, 0o000)
	defer os.Chmod(claude, 0o755)
	if os.Geteuid() == 0 {
		t.Skip("root traverses any directory")
	}
	if _, _, err := readGitConfig(root); err == nil || !strings.HasPrefix(err.Error(), "EACCES: ") {
		t.Errorf("untraversable .claude: %v", err)
	}
	os.Chmod(claude, 0o755)
	os.Remove(filepath.Join(root, "package.json"))
	os.Mkdir(filepath.Join(root, ".env.local"), 0o755)
	stderr.Reset()
	if code := runSyncContext([]string{"--repo", root}, io.Discard, &stderr); code != 1 || stderr.String() != "error: EISDIR: illegal operation on a directory, read\n" {
		t.Errorf("unreadable .env.local: %d %q", code, stderr.String())
	}
	// A path whose lookup itself fails is not absent either.
	os.Remove(filepath.Join(root, ".env.local"))
	os.Symlink(".env.local", filepath.Join(root, ".env.local"))
	stderr.Reset()
	if code := runSyncContext([]string{"--repo", root}, io.Discard, &stderr); code != 1 || !strings.HasPrefix(stderr.String(), "error: ELOOP: too many symbolic links encountered, open '") {
		t.Errorf("looping .env.local: %d %q", code, stderr.String())
	}
}
