// Package shellseg is the ONE definition of "what commands does this string
// run": quote-aware splitting, heredoc stripping, assignment and subshell
// trimming, and runner-payload unwrapping. claude-guards' rules and readeff's
// command classification both read commands through it; never re-derive
// segmentation locally (docs/decisions/0004-guard-field-audit.md).
package shellseg

import (
	"regexp"
	"strings"
)

// Segment is one inspectable piece of a command plus the separator that
// FOLLOWS it ("" for the last piece), so callers can tell `a | b` from `a; b`.
type Segment struct {
	Text string
	Sep  string
}

// Split splits a command on shell separators that are really separators —
// the ones outside quotes. Quoting matters because a literal is not a command:
// `grep -nE "vault|env"` is one grep, not a pipe into `env`, and
// `git commit -m 'fix; git stash was the cause'` runs no stash.
//
// Single quotes suppress everything. Double quotes suppress the control
// operators but NOT command substitution, because the shell still expands
// `$(...)` and backticks inside them — so those keep splitting there, which is
// what keeps `echo "$(git stash)"` from laundering a hard ban past the
// destructive rules.
//
// A substitution opens a fresh quoting context, so the state is stacked and
// popped at its closing paren. Nesting deeper than the shell's own rules (a
// backtick reopened inside a substitution, say) degrades toward splitting more
// rather than less: over-splitting costs a false positive, under-splitting
// misses a ban.
func Split(cmd string) []Segment {
	var out []Segment
	var stack []byte // quoting contexts of enclosing substitutions
	var quote byte   // 0, '\'' or '"'
	start := 0
	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		// A backslash escapes the next byte everywhere but inside single
		// quotes, where it is literal.
		if c == '\\' && quote != '\'' {
			i++
			continue
		}
		if quote == '\'' {
			if c == '\'' {
				quote = 0
			}
			continue
		}
		if quote == 0 && (c == '\'' || c == '"') {
			quote = c
			continue
		}
		if quote == '"' && c == '"' {
			quote = 0
			continue
		}
		// A substitution's closing paren ends the command inside it, so it is a
		// separator too; the quoting that surrounded the substitution resumes.
		if c == ')' && len(stack) > 0 {
			quote, stack = stack[len(stack)-1], stack[:len(stack)-1]
			out = append(out, Segment{Text: cmd[start:i], Sep: ")"})
			start = i + 1
			continue
		}
		sep := ""
		switch {
		case c == '$' && i+1 < len(cmd) && cmd[i+1] == '(':
			sep = "$("
		case c == '`':
			sep = "`"
		case quote == '"':
			// Inside double quotes nothing else separates.
		case strings.HasPrefix(cmd[i:], "||"):
			sep = "||"
		case strings.HasPrefix(cmd[i:], "&&"):
			sep = "&&"
		case c == ';' || c == '&' || c == '|' || c == '\n':
			sep = string(c)
		}
		if sep == "" {
			continue
		}
		out = append(out, Segment{Text: cmd[start:i], Sep: sep})
		i += len(sep) - 1
		start = i + 1
		if sep == "$(" {
			stack = append(stack, quote)
			quote = 0
		}
	}
	return append(out, Segment{Text: cmd[start:]})
}

// SplitScript applies Split to a command with quoted heredoc bodies
// already removed — the shared entry point for every rule family that needs to
// see the individual commands a string would run.
func SplitScript(cmd string) []Segment {
	return Split(StripQuotedHeredocs(cmd))
}

// A heredoc intro with a QUOTED delimiter: `<< 'EOF'` / << "EOF" (optionally
// <<-). A quoted delimiter makes the body literal text — no expansion, no
// command substitution — so it cannot execute anything and is stripped before
// segmenting. Unquoted heredocs (<< EOF) DO expand `$(...)` in the body, so
// their bodies stay in and are scanned like any other text.
var heredocQuotedRE = regexp.MustCompile(`<<-?[ \t]*(?:'([A-Za-z_][A-Za-z0-9_]*)'|"([A-Za-z_][A-Za-z0-9_]*)")`)

// StripQuotedHeredocs removes the bodies (and closing delimiter lines) of
// quoted-delimiter heredocs so prose/scripts fed via stdin don't false-trip
// command rules. Content-preserving for everything else.
func StripQuotedHeredocs(cmd string) string {
	var b strings.Builder
	pos := 0
	for pos < len(cmd) {
		m := heredocQuotedRE.FindStringSubmatchIndex(cmd[pos:])
		if m == nil {
			b.WriteString(cmd[pos:])
			break
		}
		delim := ""
		if m[2] >= 0 {
			delim = cmd[pos+m[2] : pos+m[3]]
		} else {
			delim = cmd[pos+m[4] : pos+m[5]]
		}
		introEnd := pos + m[1]
		nl := strings.IndexByte(cmd[introEnd:], '\n')
		if nl < 0 {
			// Intro with no body in this string; keep everything.
			b.WriteString(cmd[pos:])
			break
		}
		bodyStart := introEnd + nl + 1
		// Keep everything through the intro line's newline.
		b.WriteString(cmd[pos:bodyStart])
		// Skip the body up to and including the closing delimiter line. If the
		// delimiter never appears, shell semantics make the whole rest the
		// body — skip it all.
		rest := cmd[bodyStart:]
		end := len(cmd)
		switch {
		case strings.HasPrefix(rest, delim+"\n"):
			end = bodyStart + len(delim) + 1
		case rest == delim:
			end = len(cmd)
		default:
			if i := strings.Index(rest, "\n"+delim+"\n"); i >= 0 {
				end = bodyStart + i + 1 + len(delim) + 1
			} else if strings.HasSuffix(rest, "\n"+delim) {
				end = len(cmd)
			} else if i := heredocEndBeforeQuote(rest, delim); i >= 0 {
				// The heredoc sits inside a runner payload (`bash -lc
				// "apply_patch <<'EOF' ... EOF"`): the delimiter line ends
				// with the payload's closing quote, which must survive so
				// the payload still unwraps.
				end = bodyStart + i + 1 + len(delim)
			}
		}
		pos = end
	}
	return b.String()
}

// heredocEndBeforeQuote finds a closing delimiter line that is immediately
// followed by a quote character instead of a newline, and returns the index
// of the newline that opens it, or -1.
func heredocEndBeforeQuote(rest, delim string) int {
	for _, q := range []string{"\"", "'"} {
		if i := strings.Index(rest, "\n"+delim+q); i >= 0 {
			return i
		}
	}
	return -1
}

// TrimSubshell strips the wrapper a subshell leaves on a segment. Splitting
// breaks `(cd /tmp && git log -n 20)` into `(cd /tmp` and `git log -n 20)`, and
// the stray `)` made the last field "20)" — not a number, so the cap went
// unseen and context:unbounded-output fired on a correctly capped command.
// ~/.claude/CLAUDE.md REQUIRES `(cd <abs> && cmd)` for directory changes, so
// this is the common shape, not an edge case. Only unbalanced trailing parens
// are removed, leaving a legitimate `)` inside an argument alone.
func TrimSubshell(s string) string {
	s = strings.TrimLeft(s, "( \t")
	depth := 0
	for _, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		}
	}
	for depth < 0 {
		trimmed := strings.TrimRight(s, " \t")
		if !strings.HasSuffix(trimmed, ")") {
			break
		}
		s = trimmed[:len(trimmed)-1]
		depth++
	}
	return strings.TrimRight(s, " \t")
}

// A leading `NAME=value` word is an environment assignment, not the command.
var AssignPrefix = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// TrimAssignments removes the leading `NAME=value` words of a segment. The
// shell treats them as environment for the command that follows, so leaving
// them in place let `FOO=1 env` walk past every rule that identifies a command
// by its first token — a hard ban defeated by typing five characters.
//
// Leading only: `env FOO=bar make build` runs env as a runner and keeps its
// assignment. Values may be quoted (`FOO='a b' cmd`), so the end of a value is
// found by scanning for unquoted whitespace rather than splitting on spaces.
func TrimAssignments(s string) string {
	for {
		s = strings.TrimLeft(s, " \t")
		m := AssignPrefix.FindStringIndex(s)
		if m == nil {
			return s
		}
		end := endOfWord(s, m[1])
		if end == len(s) {
			return "" // assignments with no command after them run nothing
		}
		s = s[end:]
	}
}

// endOfWord returns the index of the unquoted whitespace that ends the word
// starting at from, or len(s) if the word runs to the end.
func endOfWord(s string, from int) int {
	var quote byte
	for i := from; i < len(s); i++ {
		switch c := s[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == ' ' || c == '\t':
			return i
		}
	}
	return len(s)
}

// CommandWord returns the command a segment actually runs, with any leading
// `FOO=bar` assignments and any directory prefix removed. `FOO=1 env` runs env,
// and so does `/usr/bin/env`.
func CommandWord(s string) string {
	for _, f := range strings.Fields(s) {
		if AssignPrefix.MatchString(f) {
			continue
		}
		if i := strings.LastIndexByte(f, '/'); i >= 0 {
			f = f[i+1:]
		}
		return f
	}
	return ""
}

// Commands whose quoted arguments are themselves a command line — the payload
// runs, just on another host, in another container, or in a nested shell.
// Quoting makes a literal inert everywhere else; it does not here, which is why
// `ssh host 'env'` and `docker exec c sh -c "env"` still have to be scanned.
var Runners = map[string]bool{
	"ssh": true, "docker": true, "podman": true, "kubectl": true,
	"nerdctl": true, "lxc": true, "nsenter": true, "chroot": true,
	"sh": true, "bash": true, "zsh": true, "dash": true, "ash": true,
	"sudo": true, "su": true, "doas": true, "env": true,
	"nohup": true, "timeout": true, "xargs": true, "watch": true,
	"arch": true, "nice": true, "time": true, "command": true, "stdbuf": true,
}

// RunnerPayloads returns the quoted arguments of a segment that will be run as
// commands elsewhere. Empty for every other command, which is exactly what
// keeps a grep pattern, a sed script or a commit message from being read as a
// command line.
func RunnerPayloads(s string) []string {
	if !Runners[CommandWord(s)] {
		return nil
	}
	var out []string
	for i := 0; i < len(s); i++ {
		q := s[i]
		if q != '\'' && q != '"' {
			continue
		}
		j := strings.IndexByte(s[i+1:], q)
		if j < 0 {
			break
		}
		if payload := s[i+1 : i+1+j]; strings.TrimSpace(payload) != "" {
			out = append(out, payload)
		}
		i += j + 1
	}
	return out
}

// ChainHas reports whether a segment actually RUNS name — as its command
// word, or as the target of a launcher chain (`sudo osascript`,
// `arch -x86_64 osascript`, `xargs cliclick`).
//
// The point is to tell running a command from naming one. A quoted literal is a
// single field, so `rg -e 'osascript … System Events'` carries no `osascript`
// token and does not match, while `sudo osascript -e …` does. Scanning stops at
// the first token that is neither name nor a launcher, so only a real wrapper
// chain is followed.
func ChainHas(s, name string) bool {
	toks := Fields(TrimAssignments(s))
	for i, t := range toks {
		if j := strings.LastIndexByte(t, '/'); j >= 0 {
			t = t[j+1:]
		}
		if t == name {
			return true
		}
		if i == 0 && !Runners[t] {
			return false
		}
	}
	return false
}

// MaxRunnerDepth bounds recursion through nested runners
// (`ssh a 'ssh b "env"'`); beyond it a payload is scanned flat.
const MaxRunnerDepth = 3

// Segments splits a command string into independently inspectable pieces,
// left-trimmed, empties dropped, runner payloads unwrapped. Quoted-heredoc
// bodies are excluded first.
func Segments(cmd string) []string {
	return AppendSegments(nil, cmd, 0, false)
}

// RemoteRunners execute their command line on another machine (`ssh box
// 'vitest'`, `devbox run -- 'jest'`), so a rule about THIS machine's load must
// not read what they carry.
var RemoteRunners = map[string]bool{"ssh": true, "devbox": true}

// LocalSegments is Segments() with every remote runner and its payload left
// out: the commands that run on this machine.
func LocalSegments(cmd string) []string {
	return AppendSegments(nil, cmd, 0, true)
}

// AppendSegments appends cmd's segments to out, recursing into runner payloads
// up to MaxRunnerDepth; localOnly leaves remote runners out.
func AppendSegments(out []string, cmd string, depth int, localOnly bool) []string {
	for _, p := range SplitScript(cmd) {
		s := TrimAssignments(TrimSubshell(strings.Trim(p.Text, " \t\r")))
		if s == "" {
			continue
		}
		if localOnly && RemoteRunners[CommandWord(s)] {
			continue
		}
		out = append(out, s)
		if depth >= MaxRunnerDepth {
			continue
		}
		for _, payload := range RunnerPayloads(s) {
			out = AppendSegments(out, payload, depth+1, localOnly)
		}
	}
	return out
}

// Fields splits a segment on whitespace, honouring single and double
// quotes (kept out of the field). Good enough for argv-shaped commands.
func Fields(s string) []string {
	var out []string
	var cur strings.Builder
	inField, q := false, byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == q {
				q = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '\'' || c == '"':
			q, inField = c, true
		case c == ' ' || c == '\t' || c == '\r':
			if inField {
				out = append(out, cur.String())
				cur.Reset()
				inField = false
			}
		default:
			cur.WriteByte(c)
			inField = true
		}
	}
	if inField {
		out = append(out, cur.String())
	}
	return out
}
