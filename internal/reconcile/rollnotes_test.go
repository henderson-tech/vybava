package reconcile

// produlinka-infra scripts/tests/test-reconcile-roll-notes.sh part 1 (#233),
// driven through the produlinka fixture manifest's roll_notes arms: a
// postmaster-context change says RESTART REQUIRED with the pg-safe-restart
// wrapper, a reloadable one says RELOAD, a comment-only edit says nothing yet
// stays claimed, and an unclaimed file keeps the generic roll.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	notePGRestart     = "RESTART REQUIRED: fixit-prod postgres (%s) — use /opt/scripts/pg-safe-restart.sh fixit-prod, never a bare `docker compose restart postgres`"
	notePGReload      = "RELOAD postgres: docker kill -s HUP fixit-prod-postgres"
	noteBouncerReload = "RELOAD pgbouncer: docker kill -s HUP fixit-prod-pgbouncer"
	absent            = "\x00absent" // before: no live file yet
)

const pgConf = `# fixture postgresql.conf
shared_buffers = 4GB
max_connections = 150
statement_timeout = 60000
archive_command = 'pgbackrest --stanza=fixit-prod archive-push %p'
shared_preload_libraries = 'pg_stat_statements'
pg_stat_statements.max = 5000
pg_stat_statements.track = top
track_io_timing = on
`

const bouncerIni = `; fixture pgbouncer.ini
[databases]
fixit_prod = host=postgres port=5432 dbname=fixit_prod

[pgbouncer]
listen_addr = 0.0.0.0
listen_port = 6432
pool_mode = transaction
server_login_retry = 1
query_wait_timeout = 20
`

func dropLines(s, pattern string) string {
	re := regexp.MustCompile(pattern)
	var out []string
	for _, l := range strings.SplitAfter(s, "\n") {
		if !re.MatchString(l) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "")
}

func TestRollNoteSteps(t *testing.T) {
	const (
		pg      = "apps/fixit-prod/postgresql.conf"
		bouncer = "apps/fixit-prod/pgbouncer/pgbouncer.ini"
	)
	m := loadFixtureManifest(t, "produlinka")
	withStar := strings.Replace(bouncerIni, "[databases]\n", "[databases]\n* = host=postgres port=5432\n", 1)
	for _, tc := range []struct {
		name, path, before, after string
		claimed                   bool
		want                      string
	}{
		{"#230 replay", pg, dropLines(pgConf, `^(shared_preload_libraries|pg_stat_statements\.|track_io_timing)`), pgConf,
			true, strings.Replace(notePGRestart, "%s", "pg_stat_statements.max shared_preload_libraries", 1)},
		{"reloadable setting", pg, pgConf, strings.Replace(pgConf, "statement_timeout = 60000", "statement_timeout = 30000", 1),
			true, notePGReload},
		{"comment-only edit", pg, pgConf, strings.Replace(pgConf, "shared_buffers = 4GB", "shared_buffers = 4GB   # trailing comment", 1) + "# a comment\n",
			true, ""},
		{"keys are case-insensitive", pg, pgConf, strings.Replace(pgConf, "shared_buffers", "Shared_Buffers", 1),
			true, ""},
		{"# inside a quoted value", pg, pgConf, strings.Replace(pgConf, "%p'", "%p #2'", 1),
			true, notePGReload},
		{"a quote in the comment after a quoted value", pg,
			strings.Replace(pgConf, "'pg_stat_statements'", "'pg_stat_statements' # operator's note", 1),
			strings.Replace(pgConf, "'pg_stat_statements'", "'pg_stat_statements' # operator's other note", 1),
			true, ""},
		{"unparseable line counts as a change", pg, pgConf, pgConf + "= orphan\n",
			true, notePGReload},
		{"new file: every setting is added", pg, absent, "shared_buffers = 1GB\nwork_mem = 4MB\n",
			true, strings.Replace(notePGRestart, "%s", "shared_buffers", 1)},
		{"pgbouncer reloadable", bouncer, dropLines(bouncerIni, `^(server_login_retry|query_wait_timeout) `), bouncerIni,
			true, noteBouncerReload},
		{"pgbouncer no-reload", bouncer, bouncerIni, strings.Replace(bouncerIni, "listen_port = 6432", "listen_port = 6433", 1),
			true, "RESTART REQUIRED: fixit-prod pgbouncer (listen_port) — a RELOAD ignores these and a restart drops every client connection; plan it"},
		{"pgbouncer quoted key", bouncer, bouncerIni, strings.Replace(bouncerIni, "listen_port = 6432", "'listen_port' = 6433", 1),
			true, "RESTART REQUIRED: fixit-prod pgbouncer (listen_port) — a RELOAD ignores these and a restart drops every client connection; plan it"},
		{"pgbouncer [databases]", bouncer, bouncerIni, strings.Replace(bouncerIni, "port=5432 dbname", "port=5433 dbname", 1),
			true, noteBouncerReload},
		{"pgbouncer * fallback", bouncer, withStar, strings.Replace(withStar, "* = host=postgres port=5432", "* = host=postgres port=5433", 1),
			true, noteBouncerReload},
		{"pgbouncer # inside a value", bouncer,
			strings.Replace(bouncerIni, "dbname=fixit_prod\n", "dbname=fixit_prod application_name=api#1\n", 1),
			strings.Replace(bouncerIni, "dbname=fixit_prod\n", "dbname=fixit_prod application_name=api#2\n", 1),
			true, noteBouncerReload},
		{"pgbouncer unparseable line counts as a change", bouncer, bouncerIni, bouncerIni + "=\n",
			true, noteBouncerReload},
		{"pgbouncer comment-only edit", bouncer, bouncerIni, bouncerIni + "; a comment\n  # another\n",
			true, ""},
		{"pg_hba.conf: any converge reloads", "apps/fixit-prod/pg_hba.conf", "host all all 10.0.0.0/8 scram-sha-256\n", "host all all 10.0.0.0/8 scram-sha-256\n# c\n",
			true, notePGReload},
		{"docker-compose.yml stays unclaimed", "apps/fixit-prod/docker-compose.yml", "services: {}\n", "services: {x: {}}\n",
			false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			before, after := filepath.Join(dir, "before"), filepath.Join(dir, "after")
			if tc.before != absent {
				mustT(t, os.WriteFile(before, []byte(tc.before), 0o644))
			}
			mustT(t, os.WriteFile(after, []byte(tc.after), 0o644))
			got := m.rollStepFor(tc.path, before, after)
			if got.claimed != tc.claimed || got.note != tc.want {
				t.Fatalf("got claimed=%v note=%q\nwant claimed=%v note=%q", got.claimed, got.note, tc.claimed, tc.want)
			}
		})
	}
	// a roll note on a path no mapping converges would never fire
	for _, r := range m.RollNotes {
		for _, p := range r.Match {
			if tgt, ok := m.MapPath(p); !ok || tgt.Hook != HookCompose || tgt.App != "fixit-prod" {
				t.Errorf("roll note %s does not map to the fixit-prod compose app: %+v", p, tgt)
			}
		}
	}
}
