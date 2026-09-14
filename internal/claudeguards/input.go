package claudeguards

import (
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

// HookInput is the payload Claude Code pipes to hook commands — PreToolUse
// carries the tool fields, the session-lifecycle hooks carry session_id.
// Unknown fields are ignored by encoding/json, so schema growth is safe.
type HookInput struct {
	CWD            string `json:"cwd"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	ToolName       string `json:"tool_name"`
	ToolInput      struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
		Offset   int    `json:"offset"`
		Limit    int    `json:"limit"`
	} `json:"tool_input"`

	guardsCfg  *Config // memoized by guards(); never set from JSON
	budgetVal  Budget  // memoized by budget()
	budgetErr  error
	budgetRead bool
}

// budget returns this payload's context budget, reading the transcript at most
// once. guardBudget consults it, and on every allowed call BudgetContext then
// consults it again — at up to 4 MiB of JSON per read, that doubled the hook's
// I/O for an answer that cannot have changed in between.
func (in *HookInput) budget() (Budget, error) {
	if !in.budgetRead {
		in.budgetVal, in.budgetErr = ReadBudget(in.TranscriptPath)
		in.budgetRead = true
	}
	return in.budgetVal, in.budgetErr
}

// guards returns the repo's guards config, loading it at most once per hook
// payload. One Bash call runs several rules and many segments, and each used to
// re-run vconfig.Load with its own `git rev-parse`. Memoizing on the PAYLOAD
// rather than in a package variable keeps the rules free of process-global
// state, which was eve's standing ruling on #53.
func (in *HookInput) guards() Config {
	if in.guardsCfg == nil {
		cfg := guardConfig(in.CWD)
		in.guardsCfg = &cfg
	}
	return *in.guardsCfg
}

// ReadInput parses the hook payload; any error means fail-open.
func ReadInput(r io.Reader) (*HookInput, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		return nil, err
	}
	var in HookInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	return &in, nil
}

// shellSegment is one inspectable piece of a command plus the separator that
// FOLLOWS it ("" for the last piece), so callers can tell `a | b` from `a; b`.
type shellSegment struct {
	text string
	sep  string
}

// splitShell splits a command on shell separators that are really separators —
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
func splitShell(cmd string) []shellSegment {
	var out []shellSegment
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
			out = append(out, shellSegment{text: cmd[start:i], sep: ")"})
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
		out = append(out, shellSegment{text: cmd[start:i], sep: sep})
		i += len(sep) - 1
		start = i + 1
		if sep == "$(" {
			stack = append(stack, quote)
			quote = 0
		}
	}
	return append(out, shellSegment{text: cmd[start:]})
}

// shellSegments applies splitShell to a command with quoted heredoc bodies
// already removed — the shared entry point for every rule family that needs to
// see the individual commands a string would run.
func shellSegments(cmd string) []shellSegment {
	return splitShell(stripQuotedHeredocs(cmd))
}

// A heredoc intro with a QUOTED delimiter: `<< 'EOF'` / << "EOF" (optionally
// <<-). A quoted delimiter makes the body literal text — no expansion, no
// command substitution — so it cannot execute anything and is stripped before
// segmenting. Unquoted heredocs (<< EOF) DO expand `$(...)` in the body, so
// their bodies stay in and are scanned like any other text.
var heredocQuotedRE = regexp.MustCompile(`<<-?[ \t]*(?:'([A-Za-z_][A-Za-z0-9_]*)'|"([A-Za-z_][A-Za-z0-9_]*)")`)

// stripQuotedHeredocs removes the bodies (and closing delimiter lines) of
// quoted-delimiter heredocs so prose/scripts fed via stdin don't false-trip
// command rules. Content-preserving for everything else.
func stripQuotedHeredocs(cmd string) string {
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
			}
		}
		pos = end
	}
	return b.String()
}

// segments splits a command string into independently inspectable pieces,
// left-trimmed, empties dropped. Quoted-heredoc bodies are excluded first.
// trimSubshell strips the wrapper a subshell leaves on a segment. Splitting
// breaks `(cd /tmp && git log -n 20)` into `(cd /tmp` and `git log -n 20)`, and
// the stray `)` made the last field "20)" — not a number, so the cap went
// unseen and context:unbounded-output fired on a correctly capped command.
// ~/.claude/CLAUDE.md REQUIRES `(cd <abs> && cmd)` for directory changes, so
// this is the common shape, not an edge case. Only unbalanced trailing parens
// are removed, leaving a legitimate `)` inside an argument alone.
func trimSubshell(s string) string {
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
var assignPrefix = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

// trimAssignments removes the leading `NAME=value` words of a segment. The
// shell treats them as environment for the command that follows, so leaving
// them in place let `FOO=1 env` walk past every rule that identifies a command
// by its first token — a hard ban defeated by typing five characters.
//
// Leading only: `env FOO=bar make build` runs env as a runner and keeps its
// assignment. Values may be quoted (`FOO='a b' cmd`), so the end of a value is
// found by scanning for unquoted whitespace rather than splitting on spaces.
func trimAssignments(s string) string {
	for {
		s = strings.TrimLeft(s, " \t")
		m := assignPrefix.FindStringIndex(s)
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

// commandWord returns the command a segment actually runs, with any leading
// `FOO=bar` assignments and any directory prefix removed. `FOO=1 env` runs env,
// and so does `/usr/bin/env`.
func commandWord(s string) string {
	for _, f := range strings.Fields(s) {
		if assignPrefix.MatchString(f) {
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
var commandRunners = map[string]bool{
	"ssh": true, "docker": true, "podman": true, "kubectl": true,
	"nerdctl": true, "lxc": true, "nsenter": true, "chroot": true,
	"sh": true, "bash": true, "zsh": true, "dash": true, "ash": true,
	"sudo": true, "su": true, "doas": true, "env": true,
	"nohup": true, "timeout": true, "xargs": true, "watch": true,
	"arch": true, "nice": true, "time": true, "command": true, "stdbuf": true,
}

// runnerPayloads returns the quoted arguments of a segment that will be run as
// commands elsewhere. Empty for every other command, which is exactly what
// keeps a grep pattern, a sed script or a commit message from being read as a
// command line.
func runnerPayloads(s string) []string {
	if !commandRunners[commandWord(s)] {
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

// commandChainHas reports whether a segment actually RUNS name — as its command
// word, or as the target of a launcher chain (`sudo osascript`,
// `arch -x86_64 osascript`, `xargs cliclick`).
//
// The point is to tell running a command from naming one. A quoted literal is a
// single field, so `rg -e 'osascript … System Events'` carries no `osascript`
// token and does not match, while `sudo osascript -e …` does. Scanning stops at
// the first token that is neither name nor a launcher, so only a real wrapper
// chain is followed.
func commandChainHas(s, name string) bool {
	toks := shellFields(trimAssignments(s))
	for i, t := range toks {
		if j := strings.LastIndexByte(t, '/'); j >= 0 {
			t = t[j+1:]
		}
		if t == name {
			return true
		}
		if i == 0 && !commandRunners[t] {
			return false
		}
	}
	return false
}

// maxRunnerDepth bounds recursion through nested runners
// (`ssh a 'ssh b "env"'`); beyond it a payload is scanned flat.
const maxRunnerDepth = 3

func segments(cmd string) []string {
	return appendSegments(nil, cmd, 0)
}

func appendSegments(out []string, cmd string, depth int) []string {
	for _, p := range shellSegments(cmd) {
		s := trimAssignments(trimSubshell(strings.Trim(p.text, " \t\r")))
		if s == "" {
			continue
		}
		out = append(out, s)
		if depth >= maxRunnerDepth {
			continue
		}
		for _, payload := range runnerPayloads(s) {
			out = appendSegments(out, payload, depth+1)
		}
	}
	return out
}

// textOnly reports whether a segment merely *mentions* commands without being
// able to execute them: comments, echoed/printed text, commit messages. Chained
// real commands live in their own segment, so skipping these costs no coverage.
func textOnly(s string) bool {
	if strings.HasPrefix(s, "#") {
		return true
	}
	for _, p := range []string{"echo ", "printf ", "cat ", "git commit "} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return s == "git commit"
}
