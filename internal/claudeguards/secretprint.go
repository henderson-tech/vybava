package claudeguards

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/henderson-tech/vybava/internal/secretscan"
)

// ---------------------------------------------------------------------------
// guardSecretPrint — printing a secret's value, or ANY part of it, is banned
// (incident-born, 2026-09-26): a read-only readiness subagent checked which
// production env vars were set with
//
//	x=$(printenv $v); echo "$v=<set len ${#x} prefix $(echo $x | cut -c1-8 …)>"
//
// over a list naming MAIL_PASSWORD, TWILIO_TOKEN, … — 22 "harmless" length +
// prefix fragments landed in the transcript, and the agent reported two.
// envdump allows `printenv NAME` for deliberately chosen names; this family
// refuses the names and the shapes that carry a secret. `vybava redact`
// cleans up what got through.
//
// Blocked (read through ssh / docker exec / sh -c payloads too):
//   printenv MAIL_PASSWORD · printenv $v with a secret name in the command
//   ${#x} ${x:0:8} · echo $x | cut -c / head -c / wc -c · .slice( .length [:8]
//     taken of a secret value: a secret-named variable, or one assigned in
//     the command from printenv / ${!v} / a secret-named expansion
//   echo "$API_TOKEN"            (not piped into a consumer, not redirected)
//
// Allowed:
//   [ -n "$MAIL_PASSWORD" ] && echo set        the sanctioned "is it set?"
//   x=$(printenv TOKEN) · printenv TOKEN >/dev/null
//   echo "$TOKEN" | gh auth login --with-token  piped into its consumer
//   printenv APP_URL · $TOKEN beside an unrelated head -c
//
// Printing a .env is deliberately NOT a rule here (Lukáš, 2026-09-26): it
// would have blocked ~20 commands a day; `vybava redact` cleans up after it.
// ---------------------------------------------------------------------------

var (
	// A shell expansion of a variable, tolerating the \$ a nested payload
	// carries: $NAME ${NAME} ${#NAME} ${NAME:0:8} ${!NAME}.
	reExpansion = regexp.MustCompile(`\\?\$\{?[#!]?([A-Za-z_][A-Za-z0-9_]*)`)
	// SCREAMING_SNAKE words, to ask secretscan.SecretName about.
	reUpperWord = regexp.MustCompile(`\b[A-Z][A-Z0-9_]{2,}\b`)
	// Ways to read an env value by a name held elsewhere in the command.
	reEnvAccessor = regexp.MustCompile(`\bprintenv\b|\\?\$\{!|process\.env|os\.environ|\bgetenv\b|os\.Getenv|System\.getenv|Deno\.env|\bENV\[`)
	rePrintenvArg = regexp.MustCompile(`\bprintenv[ \t]+((?:-[0-9A-Za-z]+[ \t]+)*)([^ \t;|&)<>]+)`)
	// NAME=… — the value is read up to the command's end to see its source.
	reAssign = regexp.MustCompile(`(?:^|[\s;&|(])([A-Za-z_][A-Za-z0-9_]*)=`)
	// A fragment taken by shell expansion: ${#x}, ${x:0:8}, ${x: -4}.
	reExpansionFragment = regexp.MustCompile(`\\?\$\{(?:#([A-Za-z_][A-Za-z0-9_]*)\}|([A-Za-z_][A-Za-z0-9_]*):[ -]?[0-9])`)
	// echo/printf of a value piped into a command that keeps part of it.
	reEchoIntoFragment = regexp.MustCompile(`(?:echo|printf)\b([^|;&\n]*)\|[ \t]*(?:cut[ \t]+(?:-\S+[ \t]+)*-[cb]|(?:head|tail)[ \t]+(?:-\S+[ \t]+)*-c|wc[ \t]+(?:-\S+[ \t]+)*-[cm]|fold[ \t]+-w|awk\b[^|]*\blength)`)
	// A language accessor reading a named variable and cutting it:
	// process.env.X.slice( · os.environ["X"][:6] · len(os.environ["X"]).
	reAccessorFragment = regexp.MustCompile(`(?:process\.env\.|process\.env\[["']|os\.environ\[["']|os\.environ\.get\(["']|os\.getenv\(["']|getenv\(["']|os\.Getenv\(")([A-Z][A-Z0-9_]*)["']?\]?\)?[ \t]*(?:\.slice\(|\.substring\(|\.substr\(|\.length\b|\[[ \t]*-?[0-9]*[ \t]*:)` +
		`|\blen\([ \t]*(?:os\.environ\[["']|os\.environ\.get\(["']|os\.getenv\(["'])([A-Z][A-Z0-9_]*)`)
	// echo/printf of an expansion, up to the next control operator.
	reEchoExpansion  = regexp.MustCompile(`(?:^|[\s;&|(])(?:echo|printf)\b([^|;&\n]*)(\|\|?|;|&|\n|$)`)
	reStdoutRedirect = regexp.MustCompile(`(?:^|[^0-9&])>{1,2}[ \t]*[^&\s]`)
)

const secretPrintMsg = `Printing a secret — its value, a prefix, a suffix or its length — is banned:
tool output is the session's permanent on-disk transcript, re-read by later
sessions and uploaded by archives (2026-09-26: a readiness check printed
"<set len 44 prefix abcdefg…>" for 11 production secrets — 22 fragments).

Check a secret by NAME only:
  • is it set?        [ -n "$MAIL_PASSWORD" ] && echo set || echo MISSING
  • which are set?    for v in A B; do [ -n "$(printenv "$v")" ] && echo "$v set"; done
  • names in a .env:  cut -d= -f1 .env
  • does it match?    onyx secret_compare (never a hash or prefix of your own)
  • use it:           pipe it into its consumer, or onyx run_command env_refs
A leak that already happened: vybava redact --session <id> --apply`

// secretPrintMatch returns the matched variant, or "". Pure — unit-testable.
func secretPrintMatch(cmd string) string {
	if cmd == "" || escapeHatch(cmd, "CLAUDE_ALLOW_DANGEROUS") {
		return ""
	}
	// Fast path: nothing that reads or prints an env value.
	if !strings.Contains(cmd, "$") && !strings.Contains(cmd, "printenv") && !strings.Contains(cmd, ".env") &&
		!strings.Contains(cmd, "environ") && !strings.Contains(cmd, "getenv") {
		return ""
	}
	// code: every shell segment as written but comments and commit messages
	// (quoted heredoc bodies are already gone) — the shell runs the
	// $expansions in an echo's words and in a bare `x="${!v}"` assignment,
	// which segments() sets aside as text or trims away.
	var code []string
	for _, p := range shellSegments(cmd) {
		s := strings.TrimSpace(p.text)
		if strings.HasPrefix(s, "#") || strings.HasPrefix(s, "git commit") {
			continue
		}
		if strings.HasPrefix(s, "echo ") || strings.HasPrefix(s, "printf ") {
			s = dropSingleQuoted(s)
		}
		code = append(code, s)
	}
	codeText := strings.Join(code, "\n")
	secretNamed := false
	for _, w := range reUpperWord.FindAllString(codeText, -1) {
		if secretscan.SecretName(w) {
			secretNamed = true
			break
		}
	}
	for _, p := range printingSegments(cmd) {
		if !p.prints {
			continue
		}
		for _, m := range rePrintenvArg.FindAllStringSubmatchIndex(p.text, -1) {
			arg := strings.Trim(p.text[m[4]:m[5]], `"'\`)
			if (secretscan.SecretName(arg) || strings.HasPrefix(arg, "$") && secretNamed) && !consumedInPayload(p.text, m[0], m[1]) {
				return "printenv-secret"
			}
		}
	}
	if fragmentOfSecret(codeText, secretNamed) {
		return "secret-fragment"
	}
	// echo/printf are prose to segments(), so they are read from the whole
	// command: one expanding a secret prints it — unless its output is piped
	// on (into the consumer), redirected away, or it sits in a { …; } group or
	// function body, whose output goes wherever the group is piped.
	stripped := stripQuotedHeredocs(cmd)
	for _, m := range reEchoExpansion.FindAllStringSubmatchIndex(stripped, -1) {
		args := stripped[m[2]:m[3]]
		if reStdoutRedirect.MatchString(args) || consumed(stripped[m[4]:]) || groupConsumed(stripped, m[0]) {
			continue
		}
		for _, e := range reExpansion.FindAllStringSubmatch(reNameCheck.ReplaceAllString(dropSingleQuoted(args), ""), -1) {
			if secretscan.SecretName(e[1]) {
				return "secret-echo"
			}
		}
	}
	return ""
}

// fragmentOfSecret reports a length, prefix or suffix taken of a secret's
// value — not merely a fragment operation somewhere near one (`$TOKEN` and a
// `head -c` over a response file is a curl probe, not a leak). A value is a
// secret when its variable is secret-named, or was assigned in this command
// from printenv / ${!v} (with a secret name in the command), a secret-named
// expansion, or a language accessor naming one.
func fragmentOfSecret(code string, secretNamed bool) bool {
	secret := func(name string) bool { return secretscan.SecretName(name) }
	tainted := map[string]bool{}
	for _, m := range reAssign.FindAllStringSubmatchIndex(code, -1) {
		value := code[m[1]:min(len(code), m[1]+256)]
		if end := strings.IndexAny(value, ";\n"); end >= 0 {
			value = value[:end]
		}
		source := secretNamed && (strings.Contains(value, "printenv") || strings.Contains(value, "${!") || strings.Contains(value, `\${!`))
		for _, e := range reExpansion.FindAllStringSubmatch(value, -1) {
			source = source || secret(e[1])
		}
		for _, w := range reUpperWord.FindAllString(value, -1) {
			source = source || secret(w) && reEnvAccessor.MatchString(value)
		}
		if source {
			tainted[code[m[2]:m[3]]] = true
		}
	}
	for _, m := range reExpansionFragment.FindAllStringSubmatch(code, -1) {
		if name := m[1] + m[2]; secret(name) || tainted[name] {
			return true
		}
	}
	for _, m := range reEchoIntoFragment.FindAllStringSubmatch(code, -1) {
		for _, e := range reExpansion.FindAllStringSubmatch(m[1], -1) {
			if secret(e[1]) || tainted[e[1]] {
				return true
			}
		}
	}
	for _, m := range reAccessorFragment.FindAllStringSubmatch(code, -1) {
		if secret(m[1] + m[2]) {
			return true
		}
	}
	return false
}

// printerCommands print what reaches them on stdin: a pipeline ending in one
// still ends in the transcript.
var printerCommands = map[string]bool{"cat": true, "bat": true, "less": true, "more": true, "head": true,
	"tail": true, "sed": true, "awk": true, "grep": true, "egrep": true, "rg": true, "strings": true,
	"xxd": true, "od": true, "cut": true, "wc": true, "tee": true, "base64": true, "tr": true, "rev": true, "fold": true}

// consumed reports whether the output leaving a command — rest starts at
// the operator after it — is taken away from the transcript: redirected to a
// file, or piped into a pipeline whose LAST stage is not itself a printer
// (`| tr -d '\n' | docker login --password-stdin` consumes; `| cat`,
// `| tee f` print).
func consumed(rest string) bool {
	rest = strings.TrimLeft(rest, " \t")
	switch {
	case strings.HasPrefix(rest, ">&"):
		return false // onto stderr: the transcript too
	case strings.HasPrefix(rest, ">"):
		return true
	case !strings.HasPrefix(rest, "|") || strings.HasPrefix(rest, "||"):
		return false
	}
	pipeline := rest[1:]
	for _, stop := range []string{"||", "&&", ";", "\n", ")"} {
		if i := strings.Index(pipeline, stop); i >= 0 {
			pipeline = pipeline[:i]
		}
	}
	stages := strings.Split(pipeline, "|")
	last := strings.TrimSpace(stages[len(stages)-1])
	fields := strings.Fields(last)
	return len(fields) > 0 && (!printerCommands[filepath.Base(fields[0])] || reStdoutRedirect.MatchString(last))
}

// groupConsumed reports that the echo at pos sits in a { …; } group or a
// function body whose output is consumed: the group is piped or redirected
// away after its `}`, or — a function — every call of it is. A group with no
// such tail prints (`{ echo "$T"; }`), and so does a bare call (`H() {…}; H`).
func groupConsumed(s string, pos int) bool {
	var stack []int
	open, close := -1, -1
	inSingle := false
	for i := 0; i < len(s) && close < 0; i++ {
		c := s[i]
		switch {
		case c == '\'':
			inSingle = !inSingle
		case inSingle:
		// `{ ` opens a group (`${` is an expansion, `{a,b}` a brace list).
		case c == '{' && (i == 0 || s[i-1] != '$') && (i+1 == len(s) || strings.IndexByte(" \t\n", s[i+1]) >= 0):
			stack = append(stack, i)
		case c == '}' && len(stack) > 0 && i > 0 && strings.IndexByte(" ;\n", s[i-1]) >= 0:
			if top := stack[len(stack)-1]; top == open {
				close = i
			}
			stack = stack[:len(stack)-1]
		}
		if i == pos-1 || pos == 0 && i == 0 {
			if len(stack) == 0 {
				return false
			}
			open = stack[len(stack)-1]
		}
	}
	if open < 0 || close < 0 {
		return false
	}
	head := strings.TrimRight(s[:open], " \t")
	if !strings.HasSuffix(head, "()") {
		return consumed(s[close+1:])
	}
	// A function: find its calls after the definition.
	name := head[strings.LastIndexAny(head[:len(head)-2], " \t;&|(\n")+1 : len(head)-2]
	calls := regexp.MustCompile(`(?:^|[\s;&|(])`+regexp.QuoteMeta(name)+`\b([^|;&\n)]*)`).FindAllStringSubmatchIndex(s[close+1:], -1)
	for _, c := range calls {
		if !consumed(s[close+1+c[3]:]) {
			return false
		}
	}
	return true
}

// reNameCheck is an expansion that tests or substitutes a secret without
// printing it: [ -n "$X" ], test -z "$X", ${X:+set}.
var reNameCheck = regexp.MustCompile(`\[\[?[ \t]+-[nz][ \t]+[^]]*\]|\btest[ \t]+-[nz][ \t]+\S+|\\?\$\{[A-Za-z_][A-Za-z0-9_]*:?\+[^}]*\}`)

type printingSegment struct {
	text   string // as segments() renders it, so the two can be matched
	prints bool
}

// printingSegments returns the command's top-level segments, each marked by
// whether its stdout reaches the transcript. A segment inside $(…) or
// backticks is captured — printed only
// when the command that opened the substitution is echo/printf
// (`echo "$(printenv X)"` prints, `[ -n "$(printenv X)" ]` and
// `x=$(printenv X)` do not). A segment redirecting stdout (>/dev/null,
// > file) prints nothing; nor does one piped into a consumer that is not
// itself a printer (`printenv TOKEN | docker login --password-stdin`).
// Comments, commit messages and echo/printf are prose here: what an echo
// expands is secret-echo's to judge.
func printingSegments(cmd string) []printingSegment {
	segs := shellSegments(cmd)
	var out []printingSegment
	prints := []bool{true} // does the current substitution level reach the transcript?
	for i, p := range segs {
		s := trimAssignments(trimSubshell(strings.TrimSpace(p.text)))
		level := prints[len(prints)-1]
		pipedAway := false
		if p.sep == "|" {
			var rest strings.Builder
			rest.WriteString(p.sep)
			for _, q := range segs[i+1:] {
				rest.WriteString(q.text + q.sep)
			}
			pipedAway = consumed(rest.String())
		}
		if s != "" && !envTextOnly(s) {
			out = append(out, printingSegment{text: s, prints: level && !reStdoutRedirect.MatchString(s) && !pipedAway})
		}
		switch {
		case p.sep == "$(" || p.sep == "`" && !inBacktick(segs[:i+1]):
			opener := strings.TrimSpace(p.text)
			echoes := strings.HasPrefix(opener, "echo ") || strings.HasPrefix(opener, "printf ") ||
				strings.Contains(opener, " echo ") || strings.Contains(opener, " printf ")
			prints = append(prints, level && echoes)
		case (p.sep == ")" || p.sep == "`") && len(prints) > 1:
			prints = prints[:len(prints)-1]
		}
	}
	return out
}

// inBacktick reports whether the last backtick among segs closes one: an
// even count of earlier backtick separators means this one opens.
func inBacktick(segs []shellSegment) bool {
	n := 0
	for _, p := range segs[:len(segs)-1] {
		if p.sep == "`" {
			n++
		}
	}
	return n%2 == 1
}

// consumedInPayload reports a printenv inside a runner payload's escaped
// substitution — `sh -c "x=\$(printenv \$v)"` — which splitShell leaves
// whole: captured unless the command opening it is an echo/printf, or its
// stdout is redirected right after.
func consumedInPayload(text string, start, end int) bool {
	if rest := text[end:]; reStdoutRedirect.MatchString(rest[:strings.IndexAny(rest+";", ";&|)\n")]) {
		return true
	}
	// Walk back through this command (`docker exec c printenv X` included)
	// to the substitution that opens it, if any.
	open := -1
	for i := start - 1; i >= 0 && open < 0; i-- {
		switch c := text[i]; {
		case c == ';' || c == '&' || c == '|' || c == '\n' || c == ')':
			return false
		case c == '`', c == '(' && i > 0 && text[i-1] == '$':
			open = i
		}
	}
	if open < 0 {
		return false
	}
	cmdStart := strings.LastIndexAny(text[:open], ";&|\n")
	opener := text[cmdStart+1 : open]
	return !strings.Contains(opener, "echo ") && !strings.Contains(opener, "printf ")
}

// dropSingleQuoted removes single-quoted literals — the shell expands
// nothing inside them — honouring double quotes, where ' is literal.
func dropSingleQuoted(s string) string {
	var b strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
			continue
		case c == '\\' && quote != '\'' && i+1 < len(s):
			b.WriteByte(c)
			i++
			c = s[i]
		case c == '"':
			quote = map[byte]byte{0: '"', '"': 0}[quote]
		case c == '\'' && quote == 0:
			quote = '\''
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func guardSecretPrint(in *HookInput) *Denial {
	if name := secretPrintMatch(in.ToolInput.Command); name != "" {
		return deny("secrets:"+name, secretPrintMsg, destructiveEscape)
	}
	return nil
}
