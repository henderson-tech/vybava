package readeff

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/henderson-tech/vybava/internal/shellseg"
)

// Readers print a file into the result; searchers print where something is.
// Everything else a shell runs is ClassOther unless it writes a file.
var (
	readVerbs   = map[string]bool{"cat": true, "nl": true, "bat": true, "less": true, "more": true, "head": true, "tail": true, "sed": true}
	searchVerbs = map[string]bool{"grep": true, "egrep": true, "rg": true, "ag": true, "ack": true, "find": true, "fd": true, "ls": true, "tree": true}
	gitSearches = map[string]bool{"grep": true, "ls-files": true}
)

// shellCall classifies one command line by what its segments do: any write
// makes it an edit, else any read a read, else any search a search. cwd is
// where it ran; a `cd` segment moves it for the segments after it.
func shellCall(cmd, cwd string) Call {
	piped := map[string]bool{}  // top-level segments whose output feeds a pipe
	vars := map[string]string{} // `W=… && cd $W`: assignments later words expand
	for _, p := range shellseg.SplitScript(cmd) {
		text := shellseg.TrimSubshell(strings.Trim(p.Text, " \t\r"))
		if p.Sep == "|" {
			piped[shellseg.TrimAssignments(text)] = true
		}
		if shellseg.TrimAssignments(text) == "" {
			for _, f := range shellseg.Fields(text) {
				if k, v, ok := strings.Cut(f, "="); ok {
					vars[k] = expandVars(v, vars)
				}
			}
		}
	}
	var c Call
	var read, search, edit bool
	dir := cwd
	for _, seg := range shellseg.Segments(cmd) {
		f := shellseg.Fields(seg)
		if len(f) == 0 {
			continue
		}
		for i := range f {
			f[i] = expandVars(f[i], vars)
		}
		word, args := shellseg.CommandWord(seg), f[1:]
		if targets := redirectTargets(seg); len(targets) > 0 && word != "cd" {
			for _, t := range targets {
				if p := resolve(dir, expandVars(t, vars)); !scratch(p) {
					edit = true
					c.Edited = append(c.Edited, p)
				}
			}
			continue // its output went to the file, not into context
		}
		switch {
		case word == "cd":
			if len(args) > 0 {
				dir = resolve(dir, args[0])
			}
		case word == "apply_patch":
			edit = true
		case word == "tee":
			for _, p := range operands(word, args, dir) {
				if !scratch(p) {
					edit = true
					c.Edited = append(c.Edited, p)
				}
			}
		case (word == "sed" || word == "perl") && inPlace(args):
			edit = true
			c.Edited = append(c.Edited, operands(word, args, dir)...)
		case readVerbs[word]:
			spans := readSpans(word, args, dir)
			read = read || len(spans) > 0 // no file operand: it filters a pipe
			if piped[seg] {
				for i := range spans { // filtered on the way: the range is unknown
					spans[i] = Span{Path: spans[i].Path}
				}
			}
			c.Spans = append(c.Spans, spans...)
		case searchVerbs[word], word == "git" && len(args) > 0 && gitSearches[args[0]]:
			search = true
			if c.Query == "" {
				c.Query, c.dir = seg, dir
			}
		case word == "git" && len(args) > 0 && args[0] == "show":
			for _, a := range args[1:] {
				if i := strings.IndexByte(a, ':'); i > 0 && i < len(a)-1 {
					read = true
					c.Spans = append(c.Spans, Span{Path: resolve(dir, a[i+1:])})
				}
			}
		}
	}
	switch {
	case edit:
		c.Class = ClassEdit
	case read:
		c.Class = ClassRead
	case search:
		c.Class = ClassSearch
	default:
		c.Class = ClassOther
	}
	return c
}

// readSpans returns the file ranges a reader prints. Start 0 means the range
// is unknown (a tail, a non-range sed script).
func readSpans(word string, args []string, dir string) []Span {
	start, n, whole := 1, 0, true
	switch word {
	case "head", "tail":
		n, whole = 10, false
		if v, ok := countFlag(args); ok {
			n = v
		}
		if word == "tail" {
			start = 0
		}
	case "sed":
		start, n, whole = sedRange(args)
	}
	var out []Span
	for _, p := range operands(word, args, dir) {
		out = append(out, Span{Path: p, Start: start, N: n, Whole: whole})
	}
	return out
}

// countFlag reads head/tail's line count: -n N, -nN, -N or --lines=N.
func countFlag(args []string) (int, bool) {
	for i, a := range args {
		v := ""
		switch {
		case a == "-n" && i+1 < len(args):
			v = args[i+1]
		case strings.HasPrefix(a, "--lines="):
			v = strings.TrimPrefix(a, "--lines=")
		case strings.HasPrefix(a, "-n"):
			v = a[2:]
		case strings.HasPrefix(a, "-"):
			v = a[1:]
		}
		if n, err := strconv.Atoi(strings.TrimPrefix(v, "+")); err == nil && v != "" {
			return n, true
		}
	}
	return 0, false
}

var sedPrint = regexp.MustCompile(`^(\d+)(?:,(\d+|\$))?p$`)

// sedRange reads one print range from a sed script (`-n '40,90p'`). Without
// -n sed prints the whole file; any other script is an unknown range.
func sedRange(args []string) (start, n int, whole bool) {
	quiet, script := false, ""
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-n":
			quiet = true
		case a == "-e" && i+1 < len(args):
			i++
			if script == "" {
				script = args[i]
			}
		case strings.HasPrefix(a, "-"):
		default:
			if script == "" {
				script = a
			}
		}
	}
	if !quiet {
		return 1, 0, true
	}
	m := sedPrint.FindStringSubmatch(strings.TrimSpace(script))
	if m == nil {
		return 0, 0, false
	}
	start, _ = strconv.Atoi(m[1])
	switch end, err := strconv.Atoi(m[2]); {
	case m[2] == "":
		return start, 1, false
	case m[2] == "$":
		return start, 0, start <= 1
	case err == nil && end >= start:
		return start, end - start + 1, false
	}
	return 0, 0, false
}

// operands are a command's file arguments: every word that is not a flag, a
// flag's value or (for sed/perl) the script.
func operands(word string, args []string, dir string) []string {
	var out []string
	scriptSeen := word != "sed" && word != "perl"
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-n" && (word == "head" || word == "tail"), a == "-e", a == "-c":
			i++
			scriptSeen = scriptSeen || a == "-e"
		case a == "" || strings.HasPrefix(a, "-") || strings.ContainsAny(a, "<>|"):
			// BSD `sed -i ''` passes an empty backup suffix; never a file.
		case !scriptSeen:
			scriptSeen = true
		default:
			out = append(out, resolve(dir, a))
		}
	}
	return out
}

// inPlace reports sed -i / perl -i (also bundled: -pi, -i.bak, -Ei).
func inPlace(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "i") {
			return true
		}
		if a == "--in-place" || strings.HasPrefix(a, "--in-place=") {
			return true
		}
	}
	return false
}

// redirectTargets lists the files a segment's unquoted `>` / `>>` redirects
// write. Descriptor redirects (2>&1) and /dev/null are not writes.
func redirectTargets(seg string) []string {
	var out []string
	var quote byte
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
			continue
		case c == '\'' || c == '"':
			quote = c
			continue
		case c != '>':
			continue
		}
		if i > 0 && seg[i-1] >= '0' && seg[i-1] <= '9' || i > 0 && seg[i-1] == '&' {
			continue // 2>err, &>out: diagnostics, not the edit
		}
		j := i + 1
		for j < len(seg) && (seg[j] == '>' || seg[j] == '|' || seg[j] == ' ' || seg[j] == '\t') {
			j++
		}
		k := j
		for k < len(seg) && !strings.ContainsRune(" \t;&|<>)", rune(seg[k])) {
			k++
		}
		if t := strings.Trim(seg[j:k], `'"`); t != "" && !strings.HasPrefix(seg[j:], "&") && t != "/dev/null" {
			out = append(out, t)
		}
		i = k
	}
	return out
}

// scratch reports a throwaway location: writing there changes no code.
func scratch(p string) bool {
	for _, dir := range []string{"/tmp/", "/private/tmp/", "/var/folders/", "/private/var/folders/"} {
		if strings.HasPrefix(p, dir) {
			return true
		}
	}
	return false
}

var shellVar = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// expandVars substitutes the variables the command itself assigned, and
// $HOME; any other variable stays as written.
func expandVars(s string, vars map[string]string) string {
	if !strings.Contains(s, "$") {
		return s
	}
	return shellVar.ReplaceAllStringFunc(s, func(m string) string {
		name := strings.Trim(m, "${}")
		if v, ok := vars[name]; ok {
			return v
		}
		if name == "HOME" && home != "" {
			return home
		}
		return m
	})
}

// home is the transcripts' own machine's home: `~` in a command means it.
var home, _ = os.UserHomeDir()

// resolve makes a path absolute against dir, expanding `~`.
func resolve(dir, p string) string {
	if home != "" && (p == "~" || strings.HasPrefix(p, "~/")) {
		p = home + p[1:]
	}
	if filepath.IsAbs(p) || dir == "" {
		return filepath.Clean(p)
	}
	return filepath.Join(dir, p)
}
