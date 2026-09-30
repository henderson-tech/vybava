package reconcile

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// box is a minimal converging fixture: scripts/* → opt/scripts, nginx/*.conf
// → opt/conf.d (hook nginx, fake test/reload), apps/*/* → opt/apps (compose).
type box struct {
	root, seed string
	m          Manifest
	out        bytes.Buffer
}

func newBox(t *testing.T, files map[string]string) *box {
	t.Helper()
	root := tempRoot(t)
	bin := filepath.Join(root, "bin")
	mustT(t, os.MkdirAll(bin, 0o755))
	mustT(t, os.WriteFile(filepath.Join(bin, "nginx-test"), []byte("#!/bin/sh\n[ ! -f "+root+"/nginx-test-fails ]\n"), 0o755))
	mustT(t, os.WriteFile(filepath.Join(bin, "nginx-reload"), []byte("#!/bin/sh\necho reload >> "+root+"/nginx-reloads\n"), 0o755))
	mustT(t, os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\necho \"$PWD $*\" >> "+root+"/docker-calls\n"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	seed := seedRepo(t, root, files)
	tru := true
	m := Manifest{
		SchemaVersion: 1, Repo: "fixture", HostLabel: "fixture",
		Clone: root + "/repo", StateDir: root + "/state", ModeFile: root + "/mode",
		LockFile: root + "/lock", AppsRoot: root + "/opt/apps", MetricsFile: root + "/metrics.prom",
		Skip: []string{"*.md"},
		Mappings: []Mapping{
			{Match: []string{"scripts/*"}, Strip: "scripts/", Dest: root + "/opt/scripts/{rest}"},
			{Match: []string{"nginx/*.conf"}, Strip: "nginx/", Dest: root + "/opt/conf.d/{rest}", Hook: HookNginx},
			{Match: []string{"apps/*/*"}, Strip: "apps/", Dest: root + "/opt/apps/{rest}", Hook: HookCompose, RequireLiveDir: &tru},
		},
		Hooks: Hooks{Nginx: NginxHook{Test: []string{"nginx-test"}, Reload: []string{"nginx-reload"}}},
	}
	mustT(t, m.Finalize(""))
	mustT(t, os.WriteFile(m.ModeFile, []byte("converge\n"), 0o644))
	return &box{root: root, seed: seed, m: m}
}

func (b *box) engine() *Engine { return &Engine{M: b.m, Out: &b.out, Err: &b.out, Version: "v1.2.3"} }

func (b *box) live(rel string) string {
	raw, _ := os.ReadFile(filepath.Join(b.root, "opt", rel))
	return string(raw)
}

func TestRollbackPinsAndUnpins(t *testing.T) {
	b := newBox(t, map[string]string{"nginx/site.conf": "listen 80\n", "scripts/a.sh": "a\n"})
	e := b.engine()
	res, err := e.Run()
	mustT(t, err)
	good := res.Commit
	if res.LastGood != good {
		t.Fatalf("last-good = %q, want %q", res.LastGood, good)
	}
	bad := commitFiles(t, b.seed, "v2", map[string]string{"nginx/site.conf": "listen 81\n", "scripts/a.sh": "a2\n"})
	res, err = e.Run()
	mustT(t, err)
	if res.Commit != bad || b.live("conf.d/site.conf") != "listen 81\n" {
		t.Fatal("second tick did not converge v2")
	}
	// v2 converged fully, so it is now last-good; the operator names the
	// commit to go back to. A hand-edit made meanwhile stays HELD.
	if res.LastGood != bad {
		t.Fatalf("last-good after v2 = %s, want %s", short(res.LastGood), short(bad))
	}
	mustT(t, os.WriteFile(filepath.Join(b.root, "opt/scripts/a.sh"), []byte("hotfix\n"), 0o644))
	res, err = e.Rollback(good, false)
	mustT(t, err)
	if res.Commit != good || b.live("conf.d/site.conf") != "listen 80\n" {
		t.Fatalf("rollback did not converge to last-good: commit=%s live=%q", short(res.Commit), b.live("conf.d/site.conf"))
	}
	if b.live("scripts/a.sh") != "hotfix\n" || len(res.Held) != 1 {
		t.Fatal("rollback overwrote a HELD file")
	}
	if (State{Dir: b.m.StateDir}).Pin() != good {
		t.Fatal("rollback did not pin")
	}
	// while pinned, run stays on the pin even though origin/main moved on
	res, _ = e.Run()
	if res.Commit != good || res.Pin != good {
		t.Fatalf("pinned run followed origin/main: %s", short(res.Commit))
	}
	if !strings.Contains(b.out.String(), "pinned to") {
		t.Fatal("pinned run did not say so")
	}
	if _, err := e.Rollback("", true); err != nil {
		t.Fatal(err)
	}
	res, _ = e.Run()
	if res.Commit != bad {
		t.Fatal("unpin did not resume following origin/main")
	}
	if _, err := e.Rollback("0000000", false); err == nil {
		t.Fatal("rollback accepted an unknown commit")
	}
	hist, err := State{Dir: b.m.StateDir}.History(0)
	mustT(t, err)
	actions := map[string]int{}
	for _, h := range hist {
		actions[h.Action]++
	}
	if actions["run"] != 4 || actions["rollback"] != 1 {
		t.Fatalf("history actions = %v", actions)
	}
	metrics, _ := os.ReadFile(b.m.MetricsFile)
	if !strings.Contains(string(metrics), "infra_reconcile_last_good_commit_info{sha=\""+bad+"\"} 1") {
		t.Fatalf("metrics lack the last-good sha:\n%s", metrics)
	}
}

func TestRollbackWithoutLastGoodRefuses(t *testing.T) {
	b := newBox(t, map[string]string{"scripts/a.sh": "a\n"})
	if _, err := b.engine().Rollback("", false); err == nil || !strings.Contains(err.Error(), "no last-good") {
		t.Fatalf("err = %v", err)
	}
}

func TestComposeRollNoteAndAutoRoll(t *testing.T) {
	b := newBox(t, map[string]string{"apps/web/docker-compose.yml": "services: {}\n", "apps/auto/docker-compose.yml": "services: {}\n"})
	mustT(t, os.MkdirAll(b.root+"/opt/apps/web", 0o755))
	mustT(t, os.MkdirAll(b.root+"/opt/apps/auto", 0o755))
	b.m.AutoRollApps = []string{"auto"}
	e := b.engine()
	res, err := e.Run()
	mustT(t, err)
	if len(res.RollNotes) != 1 || res.RollNotes[0] != "web" {
		t.Fatalf("roll notes = %v", res.RollNotes)
	}
	calls, _ := os.ReadFile(b.root + "/docker-calls")
	if !strings.Contains(string(calls), b.root+"/opt/apps/auto compose up -d") || strings.Contains(string(calls), "/web ") {
		t.Fatalf("docker calls = %q", calls)
	}
	if !strings.Contains(b.out.String(), "compose converged, ROLL MANUALLY: web") {
		t.Fatalf("missing roll-manually line:\n%s", b.out.String())
	}
	if !strings.Contains(res.Digest, "cd "+b.root+"/opt/apps/web && docker compose up -d") {
		t.Fatalf("digest = %q", res.Digest)
	}
}

// produlinka-infra test-reconcile-roll-notes.sh part 2: a claimed file's step
// replaces the generic roll only for its own app; force prints it too.
func TestRollNotesReplaceGenericRoll(t *testing.T) {
	base := map[string]string{
		"apps/demo/postgresql.conf":    "max_connections = 150\nstatement_timeout = 60000\n",
		"apps/demo/docker-compose.yml": "services: {}\n",
	}
	b := newBox(t, base)
	b.m.RollNotes = []RollNote{{Match: []string{"apps/demo/postgresql.conf"}, Grammar: GrammarPostgres,
		Restart: "RESTART REQUIRED: demo postgres ({params}) — use /opt/scripts/pg-safe-restart.sh demo",
		Reload:  "RELOAD postgres: docker kill -s HUP demo-postgres"}}
	for p, c := range base { // live copies in sync: the first tick adopts them
		mustT(t, os.MkdirAll(filepath.Dir(filepath.Join(b.root, "opt", p)), 0o755))
		mustT(t, os.WriteFile(filepath.Join(b.root, "opt", p), []byte(c), 0o644))
	}
	e := b.engine()
	_, err := e.Run()
	mustT(t, err)

	restart := "RESTART REQUIRED: demo postgres (shared_preload_libraries) — use /opt/scripts/pg-safe-restart.sh demo"
	commitFiles(t, b.seed, "preload", map[string]string{"apps/demo/postgresql.conf": "max_connections = 150\nstatement_timeout = 60000\nshared_preload_libraries = 'pg_stat_statements'\n"})
	b.out.Reset()
	res, err := e.Run()
	mustT(t, err)
	if len(res.Applied) != 1 || len(res.RollSteps) != 1 || res.RollSteps[0] != restart {
		t.Fatalf("applied = %v, roll steps = %q", res.Applied, res.RollSteps)
	}
	if len(res.RollNotes) != 0 || strings.Contains(b.out.String(), "ROLL MANUALLY") {
		t.Fatalf("a claimed-only tick still says ROLL MANUALLY:\n%s", b.out.String())
	}
	if !strings.Contains(b.out.String(), "] "+restart+"\n") || !strings.Contains(res.Digest, "run the step:\n  "+restart+"\n") {
		t.Fatalf("no RESTART REQUIRED line:\n%s\ndigest:\n%s", b.out.String(), res.Digest)
	}
	if !strings.Contains(b.live("apps/demo/postgresql.conf"), "shared_preload_libraries") {
		t.Fatal("live postgresql.conf not converged")
	}

	commitFiles(t, b.seed, "reload-plus-compose", map[string]string{
		"apps/demo/postgresql.conf":    "max_connections = 150\nstatement_timeout = 30000\nshared_preload_libraries = 'pg_stat_statements'\n",
		"apps/demo/docker-compose.yml": "services: {x: {}}\n",
	})
	b.out.Reset()
	res, err = e.Run()
	mustT(t, err)
	if len(res.RollSteps) != 1 || res.RollSteps[0] != "RELOAD postgres: docker kill -s HUP demo-postgres" {
		t.Fatalf("roll steps = %q", res.RollSteps)
	}
	if len(res.RollNotes) != 1 || res.RollNotes[0] != "demo" {
		t.Fatalf("an unclaimed compose file lost its ROLL MANUALLY: %v\n%s", res.RollNotes, b.out.String())
	}

	// force: the live hand-edit is backed up, the step replaces the roll line
	mustT(t, os.WriteFile(filepath.Join(b.root, "opt/apps/demo/postgresql.conf"), []byte("max_connections = 300\n"), 0o644))
	b.out.Reset()
	mustT(t, e.Force("apps/demo/postgresql.conf"))
	if !strings.Contains(b.out.String(), "force: RESTART REQUIRED: demo postgres (max_connections shared_preload_libraries)") ||
		strings.Contains(b.out.String(), "roll manually") {
		t.Fatalf("force output:\n%s", b.out.String())
	}
}

func TestAlertsDedupPerChannel(t *testing.T) {
	b := newBox(t, map[string]string{"scripts/a.sh": "a\n"})
	var eveHits int
	var eveBody string
	eve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		raw := make([]byte, 8192)
		n, _ := r.Body.Read(raw)
		eveBody = string(raw[:n])
		eveHits++
	}))
	defer eve.Close()
	lib := filepath.Join(b.root, "telegram-notify.sh")
	// Same guard as the real /opt/scripts/lib/telegram-notify.sh: refuse when executed, allow when sourced.
	mustT(t, os.WriteFile(lib, []byte("if [[ \"${BASH_SOURCE[0]}\" == \"${0}\" ]]; then echo 'meant to be sourced, not executed' >&2; exit 2; fi\n"+
		"notify_telegram() { printf '%s|%s|%s\\n' \"$1\" \"$2\" \"$3\" >> "+b.root+"/tg; [ ! -f "+b.root+"/tg-fails ]; }\n"), 0o644))
	cfg := filepath.Join(b.root, "eve-webhook")
	mustT(t, os.WriteFile(cfg, []byte("EVE_MONITOR_URL="+eve.URL+"\nEVE_MONITOR_TOKEN=tok\n"), 0o600))
	b.m.Alerts = []Alert{{Type: "telegram", Lib: lib, Channel: "chan"}, {Type: "eve-monitor", Config: cfg}}
	e := b.engine()
	st := State{Dir: b.m.StateDir}

	// tick 1: HELD drift → both channels fire once
	mustT(t, e.state().Ensure())
	_, _ = e.Run() // installs a.sh
	mustT(t, os.WriteFile(b.root+"/opt/scripts/a.sh", []byte("hand\n"), 0o644))
	commitFiles(t, b.seed, "v2", map[string]string{"scripts/a.sh": "a2\n"})
	_, _ = e.Run()
	tg, _ := os.ReadFile(b.root + "/tg")
	if strings.Count(string(tg), "\n") != 1 || !strings.HasPrefix(string(tg), "chan|⚠️|infra-reconcile (fixture)") {
		t.Fatalf("telegram = %q", tg)
	}
	if eveHits != 1 || !strings.Contains(eveBody, `"title":"Infra drift (fixture)"`) || !strings.Contains(eveBody, `"host":"fixture"`) {
		t.Fatalf("eve hits=%d body=%s", eveHits, eveBody)
	}
	if st.AlertMarker("telegram") == "" || st.AlertMarker("eve") == "" {
		t.Fatal("markers not recorded after delivery")
	}
	// tick 2: same digest → deduped on both
	_, _ = e.Run()
	tg, _ = os.ReadFile(b.root + "/tg")
	if strings.Count(string(tg), "\n") != 1 || eveHits != 1 {
		t.Fatalf("same digest re-alerted: tg=%d eve=%d", strings.Count(string(tg), "\n"), eveHits)
	}
	// tick 3: a second HELD file changes the digest; telegram fails → eve
	// records its own delivery, telegram retries on the next tick
	commitFiles(t, b.seed, "v3", map[string]string{"scripts/b.sh": "b\n"})
	_, _ = e.Run() // installs b.sh; digest unchanged (a.sh still HELD) → deduped
	if eveHits != 1 {
		t.Fatalf("unchanged digest re-alerted eve (hits=%d)", eveHits)
	}
	mustT(t, os.WriteFile(b.root+"/opt/scripts/b.sh", []byte("hand2\n"), 0o644))
	commitFiles(t, b.seed, "v4", map[string]string{"scripts/b.sh": "b2\n"})
	mustT(t, os.WriteFile(b.root+"/tg-fails", nil, 0o644))
	_, _ = e.Run()
	if eveHits != 2 {
		t.Fatalf("eve did not get the new digest (hits=%d)", eveHits)
	}
	tgMarker := st.AlertMarker("telegram")
	mustT(t, os.Remove(b.root+"/tg-fails"))
	_, _ = e.Run()
	if eveHits != 2 {
		t.Fatal("eve re-alerted while only telegram was outstanding")
	}
	if st.AlertMarker("telegram") == tgMarker {
		t.Fatal("telegram did not retry after its own failure")
	}
	// clean tick clears both markers
	mustT(t, e.Force("scripts/a.sh"))
	mustT(t, e.Force("scripts/b.sh"))
	_, err := e.Run()
	mustT(t, err)
	if st.AlertMarker("telegram") != "" || st.AlertMarker("eve") != "" {
		t.Fatal("clean tick did not clear alert markers")
	}
}

func TestVersionPinMismatchIsReported(t *testing.T) {
	b := newBox(t, map[string]string{"scripts/a.sh": "a\n"})
	b.m.VybavaVersion = "v9.9.9"
	e := b.engine()
	rep, err := e.StatusReport(5)
	mustT(t, err)
	if !strings.Contains(rep.VersionMismatch, "v9.9.9") || !strings.Contains(rep.VersionMismatch, "v1.2.3") {
		t.Fatalf("mismatch = %q", rep.VersionMismatch)
	}
	b.m.VybavaVersion = "1.2.3"
	if rep, _ := b.engine().StatusReport(5); rep.VersionMismatch != "" {
		t.Fatalf("v-prefix mismatch reported: %q", rep.VersionMismatch)
	}
}

func TestStatusReportAndDiff(t *testing.T) {
	b := newBox(t, map[string]string{"nginx/site.conf": "listen 80\n", "README.md": "x\n"})
	e := b.engine()
	rep, err := e.StatusReport(5)
	mustT(t, err)
	if rep.Sync != "pending" || len(rep.Pending) != 1 || rep.LastTick != nil {
		t.Fatalf("fresh status = %+v", rep)
	}
	_, _ = e.Run()
	rep, _ = e.StatusReport(5)
	if rep.Sync != "in-sync" || rep.LastTick == nil || !rep.LastTick.OK || rep.LastGood != rep.Commit {
		t.Fatalf("converged status = %+v", rep)
	}
	mustT(t, os.WriteFile(b.root+"/opt/conf.d/site.conf", []byte("listen 99\n"), 0o644))
	rep, _ = e.StatusReport(5)
	if rep.Sync != "held" {
		t.Fatalf("sync = %s", rep.Sync)
	}
	diff, err := e.Diff("nginx/site.conf")
	mustT(t, err)
	if !strings.Contains(diff, "-listen 99") || !strings.Contains(diff, "+listen 80") {
		t.Fatalf("diff = %q", diff)
	}
	for _, bad := range []string{"README.md", "../etc/passwd", "/etc/passwd", "nginx/ghost.conf"} {
		if _, err := e.Diff(bad); err == nil {
			t.Fatalf("Diff(%q) accepted", bad)
		}
	}
}

func TestGlobMatchesLikeBashCase(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"*.md", "docs/a/b.md", true},
		{"nginx/*.conf", "nginx/sub/x.conf", true}, // `*` crosses `/` in bash case
		{"*/.env*", "apps/x/.env.example", true},
		{".env*", ".env", true},
		{".env*", "apps/.env", false},
		{"apps/*/*", "apps/x", false},
		{"apps/*/*", "apps/x/y", true},
		{"nginx/*-initial.conf", "nginx/foo-initial.conf", true},
		{"nginx/*-initial.conf", "nginx/foo.conf", false},
		{"host/nginx-proxy/conf.d/*.inc", "host/nginx-proxy/conf.d/a.inc", true},
		{"a?c", "abc", true},
		{"a[bc]d", "acd", true},
		{"a[!bc]d", "abd", false},
		{"a.b", "axb", false},
	}
	for _, c := range cases {
		if got := globMatch(c.pattern, c.s); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestManifestValidation(t *testing.T) {
	bad := []string{
		"schema_version: 2\nrepo: r\nhost_label: h\nmappings: [{match: [a], dest: /x}]\n",
		"schema_version: 1\nhost_label: h\nmappings: [{match: [a], dest: /x}]\n",
		"schema_version: 1\nrepo: r\nhost_label: h\nmappings: []\n",
		"schema_version: 1\nrepo: r\nhost_label: h\nmappings: [{match: [a], dest: relative}]\n",
		"schema_version: 1\nrepo: r\nhost_label: h\nmappings: [{match: [a], dest: /x, hook: systemd}]\n",
		"schema_version: 1\nrepo: r\nhost_label: h\nmappings: [{match: [a], skip: true, dest: /x}]\n",
		"schema_version: 1\nrepo: r\nhost_label: h\nmappings: [{match: [a], dest: /x, require_live_dir: true}]\n",
		"schema_version: 1\nrepo: r\nhost_label: h\nmappings: [{match: [a], dest: /x}]\nalerts: [{type: pager}]\n",
		"schema_version: 1\nrepo: r\nhost_label: h\nmappings: [{match: [a], dest: /x}]\nroll_notes: [{match: [a], grammar: mysql, reload: r, restart: s}]\n",
		"schema_version: 1\nrepo: r\nhost_label: h\nmappings: [{match: [a], dest: /x}]\nroll_notes: [{match: [a], grammar: postgresql, restart: s}]\n",
		"schema_version: 1\nrepo: r\nhost_label: h\nmappings: [{match: [a], dest: /x}]\nroll_notes: [{match: [a], grammar: pgbouncer, reload: r}]\n",
		"schema_version: 1\nrepo: r\nhost_label: h\nmappings: [{match: [a], dest: /x}]\nroll_notes: [{match: [a], grammar: raw, reload: r, restart: s}]\n",
		"schema_version: 1\nrepo: r\nhost_label: h\nmappings: [{match: [a], dest: /x}]\nroll_notes: [{match: [], grammar: raw, reload: r}]\n",
	}
	for i, raw := range bad {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("manifest %d accepted:\n%s", i, raw)
		}
	}
	m, err := Parse([]byte("schema_version: 1\nrepo: r\nhost_label: h\nmappings: [{match: ['apps/*/*'], strip: apps/, dest: '/opt/apps/{rest}', hook: compose}]\n"))
	mustT(t, err)
	tgt, ok := m.MapPath("apps/web/x/y.yml")
	if !ok || tgt.Dest != "/opt/apps/web/x/y.yml" || tgt.App != "web" || !tgt.RequireLiveDir {
		t.Fatalf("target = %+v", tgt)
	}
	if len(m.Hooks.Compose) == 0 || m.StateDir == "" || m.LockFile == "" {
		t.Fatalf("defaults missing: %+v", m)
	}
}
