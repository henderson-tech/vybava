package claudeguards

import (
	"regexp"
	"strings"

	"github.com/henderson-tech/vybava/internal/shellseg"
)

// ---------------------------------------------------------------------------
// shell:path-variable - `path` used as a shell variable. zsh ties the array
// `path` to `PATH`: `for path in …`, `while read path`, `path=$(…)` and
// `local path` replace the search path for the rest of the command, and every
// later word fails with `command not found` (curl 1,286 times, sed 82, tr
// 54, head 38, awk 20, mkdir 18 in the three days to 2026-09-27, from ~130
// such loops). FixIt memo #194 said so in prose and the loops kept coming, so
// this is a gate. The harness shell is zsh on this platform; bash payloads
// (`bash -c '…'`, a script the command writes) are not zsh and are not judged.
// ---------------------------------------------------------------------------

// pathAssignRE matches an assignment to `path` at the head of a segment,
// bare or behind local/export/declare/typeset (with their flags).
var pathAssignRE = regexp.MustCompile(`^(?:(?:local|export|declare|typeset|readonly)[ \t]+(?:-[A-Za-z]+[ \t]+)*)?path(?:\+?=|[ \t]*$)`)

// leadingAssignRE is one leading assignment word, `NAME=…` or `NAME+=…`
// (shellseg.AssignPrefix knows only the former).
var leadingAssignRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\+?=`)

// skipValue returns s past one assignment value: a parenthesised array
// (`(one two)`, spaces and all), a quoted string, or a bare word ending at
// the first unquoted whitespace.
func skipValue(s string) string {
	if strings.HasPrefix(s, "(") {
		if end := strings.IndexByte(s, ')'); end >= 0 {
			return s[end+1:]
		}
		return ""
	}
	var quote byte
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '\'' || c == '"':
			quote = c
		case c == '\\' && i+1 < len(s):
			i++
		case c == ' ' || c == '\t':
			return s[i:]
		}
	}
	return ""
}

// shellKeywords open a compound command and are not the command word.
var shellKeywords = map[string]bool{"while": true, "until": true, "if": true, "then": true, "else": true, "elif": true, "do": true, "{": true, "!": true, "time": true}

// pathVariableUse reports whether cmd binds the zsh variable `path` and
// returns the offending segment. It reads the top-level segments RAW: the
// shared Segments() strips leading assignments (the shape being judged) and
// expands `bash -c` payloads (bash, where path is an ordinary name).
func pathVariableUse(cmd string) (string, bool) {
	for _, top := range shellseg.SplitScript(cmd) {
		seg := top.Text
		s := strings.TrimLeft(shellseg.TrimSubshell(seg), " \t")
		// `while read path`, `if ! path=…`: the keyword is not the command.
		for {
			w, rest, _ := strings.Cut(s, " ")
			if !shellKeywords[w] {
				break
			}
			s = strings.TrimLeft(rest, " \t")
		}
		if pathAssignRE.MatchString(s) {
			return seg, true
		}
		// `FOO=1 path=/tmp/x cmd`, `FOO+=(one two) path=/tmp cmd`: every
		// leading assignment binds, not only the first word; `+=` is one too,
		// and a value may be a spaced array or a quoted string, so the walk
		// consumes values itself instead of splitting on whitespace.
		for rest := s; ; {
			m := leadingAssignRE.FindString(rest)
			if m == "" {
				break
			}
			if strings.HasPrefix(m, "path=") || strings.HasPrefix(m, "path+=") {
				return seg, true
			}
			rest = strings.TrimLeft(skipValue(rest[len(m):]), " \t")
		}
		f := shellseg.Fields(shellseg.TrimAssignments(s))
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "for", "select":
			if f[1] == "path" {
				return seg, true
			}
		case "read":
			for _, a := range f[1:] {
				if a == "path" {
					return seg, true
				}
			}
		}
	}
	return "", false
}

func guardPathVariable(in *HookInput) *Denial {
	seg, hit := pathVariableUse(in.ToolInput.Command)
	if !hit {
		return nil
	}
	return deny("shell:path-variable", strings.TrimSpace(seg)+`

binds the shell variable "path". This shell is zsh, which ties path to PATH: the
assignment or loop replaces the search path, and every later command in this
call fails with "command not found" (1,765 such failures in three days). Name
the variable p, f, file or dir instead.`, "")
}
