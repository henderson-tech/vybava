package claudeguards

import (
	"regexp"
	"strings"

	"github.com/henderson-tech/vybava/internal/shellseg"
)

// ---------------------------------------------------------------------------
// guardEnvDump — full-environment dumps are banned (incident-born, 2026-08-28):
// a `docker exec … env | grep -iE "blob|s3" | sed s/SECRET.*/…/` probe leaked a
// fragment of a multi-line secret env value into the session transcript. The
// name-based sed redaction is per-line, and `env` prints a multi-line value's
// continuation lines with no NAME= prefix — a blocklist cannot redact what it
// cannot name. Secrets demand an allowlist: print names, or ask for specific
// non-secret vars.
//
// Blocked (incl. through sudo/ssh/docker exec/kubectl wrappers):
//   env · printenv                (bare: dumps every value)
//   env FOO=bar                   (assignments but no command: still dumps)
//   cat /proc/*/environ           (and tr/strings/redirect variants)
//   docker inspect … .Config.Env  (env baked into inspect output)
//   export                        (bare / -p: prints all exported values)
//
// Allowed:
//   env FOO=bar cmd · env -i cmd  (runner form — executes, prints nothing)
//   printenv NAME…                (specific, deliberately chosen vars)
//   … | cut -d= -f1  (or awk -F= / sed 's/=.*//')  — name-only projection
//   go env · conda env list · …   (subcommands of non-launcher tools)
// ---------------------------------------------------------------------------

// Commands that can sit in front of (or be) an env dump. Anything else in
// first position (`go env`, `npm run env`, `grep env …`) is a subcommand or
// argument, not a dump.
var envLaunchers = map[string]bool{
	"env": true, "printenv": true,
	"sudo": true, "ssh": true, "mosh": true,
	"docker": true, "podman": true, "kubectl": true,
	"sh": true, "bash": true, "zsh": true,
	"timeout": true, "nice": true, "time": true, "command": true, "xargs": true,
}

// Name-only projections that make a dump transcript-safe: values never print.
// Checked against the WHOLE command because pipes split segments.
var reNameOnlyFilter = regexp.MustCompile(
	`cut[[:space:]]+(-d[[:space:]]*['"]?=['"]?[[:space:]]+-f[[:space:]]*1|-f[[:space:]]*1[[:space:]]+-d[[:space:]]*['"]?=['"]?)` +
		`|awk[[:space:]]+['"]?-F=` +
		`|sed[[:space:]]+(-[En][[:space:]]+)?['"]?s/=\.\*//`)

var (
	// `.*` (not `\S*`): the pid slot often holds a substitution with spaces —
	// /proc/$(pgrep app)/environ. Same-line only (`.` excludes \n).
	reProcEnviron  = regexp.MustCompile(`/proc/.*/environ`)
	reInspectEnv   = regexp.MustCompile(`(^|[[:space:]])(docker|podman|nerdctl)[[:space:]]([^;|&]*[[:space:]])?inspect([[:space:]]|$)`)
	reConfigEnvTpl = regexp.MustCompile(`\.Config\.Env`)
)

const envDumpMsg = `Dumping a full environment is banned — env blocks print every value,
including secrets, straight into this session's permanent on-disk transcript
(a docker-exec env|grep|sed probe leaked a secret fragment on 2026-08-28;
per-line redaction cannot catch a multi-line value's continuation lines).

Secrets are an ALLOWLIST problem, not a redaction problem:
  • names only:      env | cut -d= -f1        (awk -F= '{print $1}' works too)
  • specific vars:   printenv BLOB_BACKEND S3_ENDPOINT S3_BUCKET
  • runner form:     env FOO=bar cmd           (unchanged — executes, prints nothing)
  • secret VALUES:   never in the transcript — use the Onyx vault MCP
                     (secret_list → run_command/http_call env_refs/auth_ref injection)`

// envTextOnly mirrors textOnly but keeps `cat` inspectable: `cat
// /proc/…/environ` IS the dump this guard exists for.
func envTextOnly(s string) bool {
	if strings.HasPrefix(s, "#") {
		return true
	}
	for _, p := range []string{"echo ", "printf ", "git commit "} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return s == "git commit"
}

// isRedirect reports shell redirection tokens (`2>/dev/null`, `>out`, `<f`,
// `2>&1`) that ride along without being the dump's command word.
func isRedirect(t string) bool {
	return strings.ContainsAny(t, "<>")
}

// envDumpSegment reports whether one segment invokes a full-environment dump.
// Segmentation is quote-unaware (by design, matching the rest of the guard),
// so tokens may carry glued quote characters — they are stripped per token.
func envDumpSegment(seg string) bool {
	raw := strings.Fields(seg)
	toks := make([]string, 0, len(raw))
	for _, t := range raw {
		if t = strings.Trim(t, `'"`); t != "" {
			toks = append(toks, t)
		}
	}
	if len(toks) == 0 {
		return false
	}
	if !envLaunchers[toks[0]] && !strings.HasSuffix(toks[0], "/env") && !strings.HasSuffix(toks[0], "/printenv") {
		// Bare `export` prints every exported value; `export FOO=bar` sets one.
		if toks[0] == "export" {
			rest := toks[1:]
			return len(rest) == 0 || (len(rest) == 1 && rest[0] == "-p")
		}
		return false
	}
	// Find the dump word anywhere in the wrapper chain (sudo/ssh/docker exec…).
	idx := -1
	dump := ""
	for i, t := range toks {
		if t == "env" || t == "printenv" || strings.HasSuffix(t, "/env") || strings.HasSuffix(t, "/printenv") {
			idx, dump = i, t
			break
		}
	}
	if idx < 0 {
		return false
	}
	if strings.HasSuffix(dump, "printenv") {
		// printenv with any non-flag arg reads specific, chosen names.
		for _, t := range toks[idx+1:] {
			if !strings.HasPrefix(t, "-") && !isRedirect(t) {
				return false
			}
		}
		return true
	}
	// env: flags and NAME=value assignments don't stop the dump — only a
	// command word does (runner form).
	rest := toks[idx+1:]
	for i := 0; i < len(rest); i++ {
		t := rest[i]
		switch {
		case t == "-u" || t == "--unset": // consumes a name operand
			i++
		case strings.HasPrefix(t, "-"): // -i, -0, --, -S…
		case strings.Contains(t, "="): // FOO=bar assignment
		case isRedirect(t):
		default:
			return false // a command word: env is running something, not dumping
		}
	}
	return true
}

// envDumpMatch returns the matched rule name, or "". Pure — unit-testable.
func envDumpMatch(cmd string) string {
	if cmd == "" || escapeHatch(cmd, "CLAUDE_ALLOW_DANGEROUS") {
		return ""
	}
	// Fast path: no dump-shaped keyword anywhere → zero regex/token work.
	// ".Config.Env" carries no lowercase "env", hence the "Env" check.
	if !strings.Contains(cmd, "env") && !strings.Contains(cmd, "Env") && !strings.Contains(cmd, "export") {
		return ""
	}
	// /proc/…/environ is checked against the whole command (minus literal
	// quoted-heredoc bodies): command substitution in the path — a real usage,
	// `/proc/$(pgrep app)/environ` — splits it across segments otherwise.
	live := shellseg.StripQuotedHeredocs(cmd)
	procHit := reProcEnviron.MatchString(live)
	// The sanctioned name-only projection anywhere in the pipeline clears the
	// whole command: values never reach the transcript.
	filtered := reNameOnlyFilter.MatchString(cmd)
	for _, seg := range shellseg.Segments(cmd) {
		if envTextOnly(seg) {
			continue
		}
		if procHit {
			return "proc-environ"
		}
		if reInspectEnv.MatchString(seg) && reConfigEnvTpl.MatchString(seg) {
			return "inspect-config-env"
		}
		if !filtered && envDumpSegment(seg) {
			return "env-dump"
		}
	}
	return ""
}

func guardEnvDump(in *HookInput) *Denial {
	if name := envDumpMatch(in.ToolInput.Command); name != "" {
		return deny("secrets:"+name, envDumpMsg, destructiveEscape)
	}
	return nil
}
