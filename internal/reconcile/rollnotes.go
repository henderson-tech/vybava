package reconcile

// Roll notes name the step that applies a converged config file `docker
// compose up -d` does not: postgresql.conf, pg_hba.conf and pgbouncer.ini are
// single-file bind mounts re-read only on a HUP or a restart. A file a
// manifest `roll_notes` entry claims replaces the generic "ROLL MANUALLY" for
// its app with its exact step, classified from the live→repo diff BEFORE the
// rewrite. Port of produlinka-infra scripts/infra-reconcile/roll-notes.sh and
// map-paths.sh roll_note (#233): on 2026-09-27 a shared_preload_libraries
// change got the generic line, a bare `docker compose restart postgres`
// followed, and PgBouncer's cached login failure stalled FixIt for 15 s.

import (
	"errors"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Grammar is how a roll-note file is read to decide whether it changed.
type Grammar string

const (
	// GrammarPostgres is postgresql.conf (guc-file.l): `#` starts a comment
	// anywhere outside a quoted value; keys are case-insensitive.
	GrammarPostgres Grammar = "postgresql"
	// GrammarPgbouncer is pgbouncer.ini (libusual cfparser.c): only a line
	// STARTING with `#` or `;` is a comment — a value runs to the end of the
	// line, `#` included; keys may hold `-` / `*` or be quoted.
	GrammarPgbouncer Grammar = "pgbouncer"
	// GrammarRaw has no settings: every converge prints the reload step
	// (pg_hba.conf, whose rules only a HUP applies).
	GrammarRaw Grammar = "raw"
)

// RollNote is one ordered `roll_notes` arm: first match wins. Reload is the
// step for a setting change a reload applies; Restart (`{params}` = the
// changed restart-only settings, sorted) for one only a restart applies. A
// comment-only edit prints nothing but stays claimed — never the generic roll.
type RollNote struct {
	Match   []string `yaml:"match" json:"match"`
	Grammar Grammar  `yaml:"grammar" json:"grammar"`
	Reload  string   `yaml:"reload" json:"reload"`
	Restart string   `yaml:"restart,omitempty" json:"restart,omitempty"`
	Comment string   `yaml:"comment,omitempty" json:"comment,omitempty"`
}

// PostgreSQL 16 settings of context 'postmaster': only a restart applies them.
// From REL_16_4 src/backend/utils/misc/guc_tables.c (PGC_POSTMASTER) plus
// pg_stat_statements.max, the preloaded library's one. Regenerate on a major
// upgrade: SELECT name FROM pg_settings WHERE context = 'postmaster' ORDER BY 1;
var pgPostmasterParams = setOf(`archive_mode autovacuum_freeze_max_age autovacuum_max_workers
 autovacuum_multixact_freeze_max_age bonjour bonjour_name cluster_name config_file
 data_directory data_sync_retry debug_io_direct dynamic_shared_memory_type event_source
 external_pid_file hba_file hot_standby huge_page_size huge_pages ident_file
 ignore_invalid_pages jit_provider listen_addresses logging_collector max_connections
 max_files_per_process max_locks_per_transaction max_logical_replication_workers
 max_pred_locks_per_transaction max_prepared_transactions max_replication_slots
 max_wal_senders max_worker_processes min_dynamic_shared_memory old_snapshot_threshold
 port recovery_target recovery_target_action recovery_target_inclusive recovery_target_lsn
 recovery_target_name recovery_target_time recovery_target_timeline recovery_target_xid
 reserved_connections shared_buffers shared_memory_type shared_preload_libraries
 superuser_reserved_connections track_activity_query_size track_commit_timestamp
 unix_socket_directories unix_socket_group unix_socket_permissions wal_buffers
 wal_decode_buffer_size wal_level wal_log_hints pg_stat_statements.max`)

// PgBouncer 1.25 [pgbouncer] settings a RELOAD ignores (CF_NO_RELOAD in
// src/main.c; SHOW CONFIG reports them `changeable` = no). Everything else,
// [databases] included, applies on a HUP.
var pgbouncerNoReloadParams = setOf(`disable_pqexec job_name listen_addr listen_backlog listen_port
 pidfile pkt_buf resolv_conf service_name so_reuseport track_extra_parameters
 unix_socket_dir unix_socket_group unix_socket_mode user`)

func setOf(names string) map[string]bool {
	set := map[string]bool{}
	for _, n := range strings.Fields(names) {
		set[n] = true
	}
	return set
}

func (r RollNote) validate() error {
	if len(r.Match) == 0 {
		return errors.New("match is required")
	}
	for _, g := range r.Match {
		if g == "" {
			return errors.New("empty match glob")
		}
	}
	if strings.TrimSpace(r.Reload) == "" {
		return errors.New("reload is required: a claimed file replaces the generic roll, so it must name its step")
	}
	switch r.Grammar {
	case GrammarPostgres, GrammarPgbouncer:
		if strings.TrimSpace(r.Restart) == "" {
			return errors.New("restart is required for grammar " + string(r.Grammar))
		}
	case GrammarRaw:
		if r.Restart != "" {
			return errors.New("grammar raw has no settings to decide a restart — drop restart")
		}
	default:
		return errors.New("unknown grammar " + `"` + string(r.Grammar) + `"` + " (postgresql | pgbouncer | raw)")
	}
	return nil
}

// RollNoteFor returns the first roll_notes arm claiming rp.
func (m Manifest) RollNoteFor(rp string) (RollNote, bool) {
	for _, r := range m.RollNotes {
		for _, g := range r.Match {
			if globMatch(g, rp) {
				return r, true
			}
		}
	}
	return RollNote{}, false
}

// rollStep is one converging file's roll classification.
type rollStep struct {
	claimed bool   // a roll_notes arm owns the file: no generic compose roll
	note    string // the exact step; "" = none needed (comment-only edit)
}

// rollStepFor must run BEFORE the live file is rewritten: before is the live
// copy (absent = a new file, every setting added), after the repo copy.
func (m Manifest) rollStepFor(rp, before, after string) rollStep {
	r, ok := m.RollNoteFor(rp)
	if !ok {
		return rollStep{}
	}
	return rollStep{claimed: true, note: r.note(readConf(before), readConf(after))}
}

// readConf reads a config file; an absent one is empty. An unreadable one is
// empty too, so every repo setting counts as changed — the conservative side:
// a spurious RESTART REQUIRED beats a missing one.
func readConf(p string) string {
	raw, err := os.ReadFile(p)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	return string(raw)
}

func (r RollNote) note(before, after string) string {
	if r.Grammar == GrammarRaw {
		return r.Reload
	}
	changed := changedSettings(confSettings(before, r.Grammar), confSettings(after, r.Grammar))
	if len(changed) == 0 {
		return ""
	}
	var restart []string
	for _, k := range changed {
		switch r.Grammar {
		case GrammarPostgres:
			if pgPostmasterParams[k] {
				restart = append(restart, k)
			}
		case GrammarPgbouncer:
			if name, ok := strings.CutPrefix(k, "pgbouncer:"); ok && pgbouncerNoReloadParams[name] {
				restart = append(restart, name)
			}
		}
	}
	if len(restart) > 0 {
		return strings.ReplaceAll(r.Restart, "{params}", strings.Join(restart, " "))
	}
	return r.Reload
}

var (
	iniSection   = regexp.MustCompile(`^[ \t\r\n\f\v]*\[[^\]]*\]`)
	iniQuotedKey = regexp.MustCompile(`^'[^']+'`)
	iniKey       = regexp.MustCompile(`^[A-Za-z0-9_.*-]+`)
	pgKey        = regexp.MustCompile(`^[A-Za-z0-9_.]+`)
	keySep       = regexp.MustCompile(`^[ \t\r\n\f\v]*=?[ \t\r\n\f\v]*`)
	pgComment    = regexp.MustCompile(`[ \t\r\n\f\v]*#.*$`)
)

const confSpace = " \t\r\n\f\v"

// confSettings maps each active setting to its value, the last one winning
// like both servers. pgbouncer keys are prefixed "<section>:" (and lowercased
// in [pgbouncer], where the server ignores case). A line the grammar does not
// parse still counts, as itself ("<section>?<line>"), so a real change can
// never pass as comment-only; a comment-only edit changes nothing.
func confSettings(content string, g Grammar) map[string]string {
	ini := g == GrammarPgbouncer
	v := map[string]string{}
	section := ""
	for _, raw := range strings.Split(content, "\n") {
		if ini && iniSection.MatchString(raw) {
			s := strings.TrimLeft(raw, confSpace)[1:]
			s, _, _ = strings.Cut(s, "]")
			section = strings.ToLower(s) + ":"
			continue
		}
		line := strings.Trim(raw, confSpace)
		if line == "" || line[0] == '#' || (ini && line[0] == ';') {
			continue
		}
		var key string
		if ini {
			if key = iniQuotedKey.FindString(line); key == "" {
				key = iniKey.FindString(line)
			}
		} else {
			key = pgKey.FindString(line)
		}
		if key == "" {
			v[section+"?"+line] = "1"
			continue
		}
		rest := keySep.ReplaceAllString(line[len(key):], "")
		if ini {
			if section == "pgbouncer:" {
				key = strings.ToLower(key)
			}
			v[section+key] = rest
			continue
		}
		// a quoted value runs to the LAST quote (escaped quotes inside it
		// included); anything else ends at a # comment
		if strings.HasPrefix(rest, "'") && strings.Contains(rest[1:], "'") {
			rest = rest[:strings.LastIndex(rest, "'")+1]
		} else {
			rest = pgComment.ReplaceAllString(rest, "")
		}
		v[strings.ToLower(key)] = rest
	}
	return v
}

// changedSettings lists the keys added, removed or re-valued, byte-sorted.
func changedSettings(before, after map[string]string) []string {
	var out []string
	for k, a := range before {
		if b, ok := after[k]; !ok || a != b {
			out = append(out, k)
		}
	}
	for k := range after {
		if _, ok := before[k]; !ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
