package claudeguards

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardConfigPerCall(t *testing.T) {
	root, _, big, _ := fixture(t)
	write := func(raw string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "vybava.config.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"guards":{"noRead":["**/big.ts"],"maxDumpLines":50}}`)
	if d := contextBashMatch("sed -n '1,10p' "+big, root); d == nil || d.Rule != "context:no-read" {
		t.Fatalf("noRead: %v", d)
	}
	if d := contextReadMatch(big, 10, root); d == nil || d.Rule != "context:no-read" {
		t.Fatalf("Read noRead: %v", d)
	}
	if d := contextBashMatch("rg -n x "+big, root); d != nil {
		t.Fatal(d)
	}
	write(`{"guards":{"maxDumpLines":75}}`)
	if d := contextBashMatch("sed -n '1,70p' "+big, root); d != nil {
		t.Fatal(d)
	}
	if d := contextBashMatch("sed -n '1,80p' "+big, root); d == nil {
		t.Fatal("configured budget ignored")
	}
	if d := contextReadMatch(big, 80, root); d == nil {
		t.Fatal("Read limit bypassed configured budget")
	}
	// A key from a newer claude-guards is skipped; every known guard applies.
	write(`{"guards":{"noRead":["**/big.ts"],"devboxOnly":["^bun run test(:|$)"],"fromTheFuture":["x"]}}`)
	cfg, err := loadGuardConfig(root)
	if err != nil || strings.Join(cfg.unknownKeys, ",") != "fromTheFuture" {
		t.Fatalf("unknown key: %v, %v", cfg.unknownKeys, err)
	}
	if d := contextBashMatch("sed -n '1,10p' "+big, root); d == nil || d.Rule != "context:no-read" {
		t.Fatalf("noRead dropped beside an unknown key: %v", d)
	}
	in := &HookInput{CWD: root}
	in.ToolInput.Command = "bun run test"
	if d := guardDevboxOnly(in); d == nil {
		t.Fatal("devboxOnly dropped beside an unknown key")
	}
	// A wrong TYPE on a known key still voids the section, loudly.
	write(`{"guards":{"devboxOnly":"^bun run test"}}`)
	if _, err := loadGuardConfig(root); err == nil {
		t.Fatal("a mistyped known key must still error")
	}
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"**/translation-keys.d.ts", "translation-keys.d.ts", true},
		{"packages/generated/**", "packages/generated/a/b.ts", true},
		{"*.lock", "nested/bun.lock", false},
	} {
		if got := MatchNoRead(tc.pattern, tc.name); got != tc.want {
			t.Fatalf("%s %s: %v", tc.pattern, tc.name, got)
		}
	}
}

func TestUnboundedOutput(t *testing.T) {
	for _, tc := range []struct{ deny, allow string }{
		{"docker logs app", "docker logs --tail 200 app"},
		// docker spells its uncapped default as a value, not an absent flag.
		{"docker logs --tail all app", "docker logs --tail 200 app"},
		{"docker logs --tail=all app", "docker logs --tail=200 app"},
		{"docker logs -n all app", "docker logs -n 200 app"},
		{"gh run view 12 --log", "gh run view 12 --log | tail -100"},
		{"gh run view 12 --log-failed", "gh run view 12"},
		{"git log --oneline", "git log --oneline -20"},
		{"git log", "git log --max-count=10"},
		{"git diff", "git diff --stat"},
		{"git show HEAD", "git show HEAD -- file.go"},
		// git's global options sit before the subcommand; the pair-forming
		// used to read `git -C repo log` as `git -C` and allow it uncapped.
		{"git -C /srv/repo log --oneline", "git -C /srv/repo log --oneline -20"},
		{"git --no-pager log", "git --no-pager log -n 20"},
		{"git -c core.pager=cat log", "git -c core.pager=cat log -n 5"},
	} {
		t.Run(tc.deny, func(t *testing.T) {
			if d := contextBashMatch(tc.deny, t.TempDir()); d == nil || d.Rule != "context:unbounded-output" {
				t.Fatalf("deny: %v", d)
			}
			if d := contextBashMatch(tc.allow, t.TempDir()); d != nil {
				t.Fatalf("allow: %v", d)
			}
		})
	}
	// Test runners stay out of this rule: every repo here documents a bare
	// suite run as its verify step, and a guard that refuses the documented
	// command only teaches people to route around the guard.
	for _, cmd := range []string{"go test ./...", "bun test", "bunx jest", "go test ./... && go vet ./..."} {
		if d := contextBashMatch(cmd, t.TempDir()); d != nil {
			t.Fatalf("%s: %v", cmd, d)
		}
	}
	// A suggestion that drops the revisions the caller typed is a different
	// command from the one they wanted.
	if d := contextBashMatch("git show HEAD~3", t.TempDir()); d == nil || !strings.Contains(d.Message, "HEAD~3") {
		t.Fatalf("fix must keep the caller's arguments: %v", d)
	}
}
