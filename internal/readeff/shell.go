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

// shellCall classifies one command line pipeline by pipeline. Only a
// pipeline's last stage prints into the result: a read piped into a search
// is a search, a reader with no file operand only filters, and a pipeline
// redirected into a file put nothing into context (it wrote the file). The
// call records every kind it did; Visible counts the pipelines whose output
// reached the result, so output lines are attributed only when one did. cwd
// is where it ran; `cd` moves it for what follows.
func shellCall(cmd, cwd string) Call {
	c := Call{Tool: "Bash"}
	c.shell(cmd, cwd, 0)
	if starts := patchStart.FindAllStringIndex(cmd, -1); c.patched && len(starts) > 0 { // apply_patch fed a heredoc
		for i, s := range starts {
			end := len(cmd)
			if i+1 < len(starts) {
				end = starts[i+1][0]
			}
			dir := cwd // each body pairs with the apply_patch that ran it
			if i < len(c.patchDirs) {
				dir = c.patchDirs[i]
			}
			files, changed := patchChanged(cmd[s[0]:end], dir)
			c.Edit, c.Edited, c.Changed = true, append(c.Edited, files...), c.Changed+changed
		}
	} else if c.wrote {
		c.Changed += heredocLines(cmd) // `cat > f <<'EOF'`: the body is the file
	}
	c.classify()
	return c
}

// patchStart is a patch's opening line; the marker inside an added line
// (`+x := "*** Begin Patch"`) is patch content, not a new patch.
var patchStart = regexp.MustCompile(`(?m)^\*\*\* Begin Patch`)

// shell folds cmd's pipelines into c; depth bounds nested `sh -c` payloads.
func (c *Call) shell(cmd, cwd string, depth int) {
	vars := map[string]string{} // `W=… && cd $W`: assignments later words expand
	dir := cwd
	var stages []string
	for _, p := range shellseg.SplitScript(cmd) {
		text := shellseg.TrimSubshell(strings.Trim(p.Text, " \t\r"))
		if shellseg.TrimAssignments(text) == "" {
			for _, f := range shellseg.Fields(text) {
				if k, v, ok := strings.Cut(f, "="); ok {
					vars[k] = expandVars(v, vars)
				}
			}
		} else {
			stages = append(stages, shellseg.TrimAssignments(text))
		}
		if p.Sep != "|" && len(stages) > 0 {
			c.pipeline(stages, &dir, vars, depth)
			stages = stages[:0]
		}
	}
}

// pipeline folds one pipeline's stages into c.
func (c *Call) pipeline(stages []string, dir *string, vars map[string]string, depth int) {
	var spans []Span
	var search, visible = false, true
	var query string
	for i, st := range stages {
		f := shellseg.Fields(st)
		if len(f) == 0 {
			continue
		}
		for j := range f {
			f[j] = expandVars(f[j], vars)
		}
		word, args := shellseg.CommandWord(st), f[1:]
		targets, away := redirectTargets(st)
		for _, t := range targets {
			if p := resolve(*dir, expandVars(t, vars)); !scratch(p) {
				c.Edit, c.wrote = true, true
				c.Edited = append(c.Edited, p)
			}
		}
		if away && i == len(stages)-1 {
			visible = false
		}
		if payloads := shellseg.RunnerPayloads(st); len(payloads) > 0 && depth < shellseg.MaxRunnerDepth {
			if i == len(stages)-1 {
				for _, p := range payloads { // `bash -lc '…'` runs its payload here
					c.shell(p, *dir, depth+1)
				}
				visible = false // the payload's own pipelines carry its output
				continue
			}
			// `bash -lc 'cat a.go' | head`: the payload's writes stand, but
			// its output is this pipeline's input — it is a stage here.
			var inner Call
			for _, p := range payloads {
				inner.shell(p, *dir, depth+1)
			}
			c.Edit, c.wrote, c.patched = c.Edit || inner.Edit, c.wrote || inner.wrote, c.patched || inner.patched
			c.patchDirs = append(c.patchDirs, inner.patchDirs...)
			c.Edited = append(c.Edited, inner.Edited...)
			for _, sp := range inner.Spans {
				spans = append(spans, Span{Path: sp.Path}) // filtered downstream: range unknown
			}
			if inner.Search && query == "" {
				search, query = true, inner.Query
			}
			continue
		}
		switch {
		case word == "cd" && len(stages) == 1:
			if len(args) > 0 {
				*dir = resolve(*dir, args[0])
			}
		case word == "apply_patch":
			c.Edit, c.patched = true, true
			c.patchDirs = append(c.patchDirs, *dir) // its paths are relative to where it ran
		case word == "tee":
			for _, p := range operands(word, args, *dir) {
				if !scratch(p) {
					c.Edit, c.wrote = true, true
					c.Edited = append(c.Edited, p)
				}
			}
		case (word == "sed" || word == "perl") && inPlace(args):
			c.Edit = true
			c.Edited = append(c.Edited, operands(word, args, *dir)...)
		case readVerbs[word]:
			for _, sp := range readSpans(word, args, *dir) { // no operand: a filter
				if i < len(stages)-1 {
					sp = Span{Path: sp.Path} // filtered downstream: range unknown
				}
				spans = append(spans, sp)
			}
		case searchVerbs[word], word == "git" && len(args) > 0 && gitSearches[args[0]]:
			search = true
			if query == "" {
				query = st
			}
		case word == "git" && len(args) > 0 && args[0] == "show":
			for _, a := range args[1:] {
				if j := strings.IndexByte(a, ':'); j > 0 && j < len(a)-1 {
					spans = append(spans, Span{Path: resolve(*dir, a[j+1:])})
				}
			}
		}
	}
	if !visible {
		return // written to a file or discarded: nothing reached context
	}
	switch {
	case search: // a read feeding a search only fed it
		c.Visible++
		c.outputs |= 1 << 1
		c.Search = true
		if c.Query == "" {
			c.Query, c.dir = query, *dir
		}
	case len(spans) > 0:
		c.Visible++
		c.outputs |= 1 << 0
		c.Read = true
		c.Spans = append(c.Spans, spans...)
	case !(len(stages) == 1 && silentVerbs[shellseg.CommandWord(stages[0])]):
		c.Visible++ // other output (a build, a test run) shares the result
		c.outputs |= 1 << 2
	}
}

// silentVerbs print nothing on success, or only what the command itself
// wrote (an `echo ---` separator): no navigation to attribute.
var silentVerbs = map[string]bool{"cd": true, "export": true, "set": true, "source": true, ".": true, "true": true, "mkdir": true, "unset": true, "echo": true, "printf": true}

// heredocLines counts the body lines of a command's quoted heredocs.
func heredocLines(cmd string) int {
	return max(0, countLines(cmd)-countLines(shellseg.StripQuotedHeredocs(cmd))-1)
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
// away reports stdout leaving the result at all, /dev/null included.
func redirectTargets(seg string) (out []string, away bool) {
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
		for q := byte(0); k < len(seg) && (q != 0 || !strings.ContainsRune(" \t;&|<>)", rune(seg[k]))); k++ {
			switch { // a quoted target is one word, spaces included
			case q != 0 && seg[k] == q:
				q = 0
			case q == 0 && (seg[k] == '\'' || seg[k] == '"'):
				q = seg[k]
			}
		}
		if t := strings.Join(shellseg.Fields(seg[j:k]), ""); t != "" && !strings.HasPrefix(seg[j:], "&") { // one word, quotes decoded
			away = true
			if t != "/dev/null" {
				out = append(out, t)
			}
		}
		i = k
	}
	return out, away
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
