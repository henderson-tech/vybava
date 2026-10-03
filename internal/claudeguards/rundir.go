package claudeguards

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/henderson-tech/vybava/internal/shellseg"
)

// ---------------------------------------------------------------------------
// runDirs — WHERE each command of a string runs.
//
// shellseg says what a string runs; this says where. A rule with a directory
// carve-out (git-switch is fine in a worktree, compose down -v in a wt- stack)
// used to judge the hook's cwd alone, which misses the only two ways a session
// parked in its checkout reaches another tree — `cd <dir> && …` and
// `git -C <dir>` — because Claude Code resets the shell's cwd between calls.
// The git-switch denial recommended `cd .worktrees/<name>` and then blocked
// exactly that (Reservine session 611cadd3, 2026-10-03). The same gap ran the
// other way: from a worktree cwd, `cd ../.. && git checkout` switched the
// primary clone unseen.
//
// Fail-closed for a carve-out: a segment's directory is KNOWN only when every
// step to it is certain — a literal cd (or -C) target, success guaranteed by
// `&&` from the cd all the way to the segment (a `;`, `||`, `|`, `&` or
// newline after a cd at the same shell level breaks the chain), no cd inside a
// subshell or substitution that closed before the segment, no cd that is a
// pipeline element (`echo | cd x` runs in its own subshell), no pushd/popd, no
// remote runner. Anything else leaves known false and dir at the cwd — exactly
// what every rule saw before this walk existed. An absolute target
// re-establishes certainty by itself: `cd x; cd /abs && git checkout` is
// certain.
//
// devboxonly's devboxWalk is the other direction — every directory a command
// COULD run in, for a routing rule that must not miss one — and keeps its own
// certainty scan (cdsCertain); folding it onto this walk is the open step.
// ---------------------------------------------------------------------------

// runSeg is one command a string runs — the piece shellseg.Segments would
// list — with the directory it provably runs in.
type runSeg struct {
	text  string
	dir   string // where text runs when known; the session cwd otherwise
	known bool
	moved bool // a literal cd or git -C put it in dir; it did not just start there
}

// runDirs lists cmd's commands exactly as shellseg.Segments does (same pieces,
// same order, runner payloads unwrapped), each with its directory.
func runDirs(cmd, cwd string) []runSeg {
	home, _ := os.UserHomeDir()
	w := dirWalk{cwd: cwd, home: home}
	return w.scan(nil, cmd, level{dir: cwd, known: true}, 0)
}

// RunSeg is one command a string runs and where: Dir is the directory a
// literal cd or git -C moved it to when Known, the starting directory
// otherwise. Moved: a cd or -C put it there — `cd <start> && …` too — rather
// than it just starting there.
type RunSeg struct {
	Text, Dir    string
	Known, Moved bool
}

// RunDirs is runDirs for attribution — tokentime files an agent's minutes
// under the repository its writing commands ran in — so "where does this run"
// keeps one walk. Attribution wants the likely directory, not the proven one:
// every literal cd is taken to have applied, so a `;`, `||` or newline after
// it keeps its directory (`cd /w/lib; make`). A cd in its own subshell — a
// pipeline element, a background job — still never moves what follows. The
// guards keep the fail-closed walk.
func RunDirs(cmd, cwd string) []RunSeg {
	home, _ := os.UserHomeDir()
	w := dirWalk{cwd: cwd, home: home, assume: true}
	segs := w.scan(nil, cmd, level{dir: cwd, known: true}, 0)
	out := make([]RunSeg, len(segs))
	for i, s := range segs {
		out[i] = RunSeg{Text: s.text, Dir: s.dir, Known: s.known, Moved: s.moved}
	}
	return out
}

// dirWalk is one runDirs pass. remote marks the payload of a remote runner,
// where nothing on this machine is known; assume takes every literal cd as
// applied (RunDirs).
type dirWalk struct {
	cwd, home      string
	remote, assume bool
}

// level is one shell level — the top level, a `( … )` subshell or a `$( … )`
// substitution. A frame is pushed when one opens and popped when it closes,
// so a cd inside never leaks out.
type level struct {
	dir    string
	known  bool
	moved  bool // a cd ran at this level; a later non-&& separator breaks the chain
	placed bool // dir is a literal cd's, at this level or one enclosing it
}

// scan walks one shell level (a command string or a runner's payload) that
// starts at start.
func (w dirWalk) scan(out []runSeg, cmd string, start level, depth int) []runSeg {
	cur := start
	var stack []level
	backtick := false
	pipe := false // the segments since the last command hang off a `|`: each runs in its own subshell
	push := func() {
		stack = append(stack, cur)
		cur.moved = false
	}
	pop := func() {
		if n := len(stack); n > 0 {
			cur, stack = stack[n-1], stack[:n-1]
			return
		}
		cur.known = false // a close with no open: the structure is lost
	}
	for _, p := range shellseg.SplitScript(cmd) {
		raw := strings.Trim(p.Text, " \t\r")
		opens, closes := subshellParens(raw)
		for i := 0; i < opens; i++ {
			push()
		}
		if s := shellseg.TrimAssignments(shellseg.TrimSubshell(raw)); s != "" {
			out = w.segment(out, s, &cur, pipe && opens == 0, depth)
			pipe = false
		}
		for i := 0; i < closes; i++ {
			pop()
		}
		if p.Sep == "|" {
			pipe = true
		}
		switch p.Sep {
		case "&&", "":
		case "$(":
			push()
		case ")":
			pop()
		case "`":
			if backtick = !backtick; backtick {
				push()
			} else {
				pop()
			}
		default: // ; | & || newline: the next command runs whether or not a cd did
			// Taken as applied, a cd still moved nothing when it ran in its
			// own subshell: a pipeline element, a background job.
			if cur.moved && !(w.assume && p.Sep != "|" && p.Sep != "&") {
				cur.known = false
			}
		}
	}
	return out
}

// segment records s, which runs at cur, then applies what s does to the
// commands after it: a cd moves them, pushd/popd lose them, a runner's payload
// is its own level starting where the runner runs — a cd inside never moves
// the commands after the runner. piped marks a later element of a pipeline
// (`echo | cd x && …`): it runs in its own subshell, so its cd or pushd never
// moves the shell either, and the commands after it run where they did before.
func (w dirWalk) segment(out []runSeg, s string, cur *level, piped bool, depth int) []runSeg {
	fields := shellseg.Fields(s)
	word := shellseg.CommandWord(s)
	dir, known, moved := cur.dir, cur.known, cur.placed
	if word == "git" {
		var gitC bool
		dir, known, gitC = gitCDir(fields, dir, known, w.home)
		moved = moved || gitC
	}
	if w.remote || !known {
		dir, known, moved = w.cwd, false, false
	}
	out = append(out, runSeg{text: s, dir: dir, known: known, moved: moved})
	if target, ok := cdTarget(fields); ok {
		if piped {
			return out
		}
		cur.moved = true
		if target == "-" || !literalPath(target) {
			cur.known = false
		} else {
			cur.known = cur.known || rooted(target)
			cur.dir = resolveDir(target, cur.dir, w.home)
			cur.placed = true
		}
		return out
	}
	if !piped && (word == "pushd" || word == "popd") {
		cur.moved, cur.known = true, false
	}
	if depth < shellseg.MaxRunnerDepth {
		inner := w
		inner.remote = w.remote || shellseg.RemoteRunners[word]
		for _, payload := range shellseg.RunnerPayloads(s) {
			out = inner.scan(out, payload, level{dir: dir, known: known, placed: moved}, depth+1)
		}
	}
	return out
}

// gitCDir is where a git segment runs: dir, moved by each literal `-C <path>`
// among git's global options (a relative one against the directory so far, the
// way git applies them). A -C that is not spelled out — $W, a substitution —
// makes the directory unknown: a variable's value cannot be proven from the
// command. gitC reports a literal -C applied.
func gitCDir(fields []string, dir string, known bool, home string) (_ string, _ bool, gitC bool) {
	for i := 1; i < len(fields) && strings.HasPrefix(fields[i], "-"); i++ {
		name, value, joined := strings.Cut(fields[i], "=")
		if !joined && gitGlobalWithValue[name] && i+1 < len(fields) {
			i++
			value = fields[i]
		}
		if name != "-C" {
			continue
		}
		if !literalPath(value) {
			return dir, false, false
		}
		known = known || rooted(value)
		dir = resolveDir(value, dir, home)
		gitC = true
	}
	return dir, known, gitC
}

// cdTarget is the directory a `cd` names: its operand after the builtin's
// options (-L, -P, -e, -@) and an optional `--`, or ~ when it has none. ok is
// false when fields are not a cd.
func cdTarget(fields []string) (target string, ok bool) {
	if len(fields) == 0 || fields[0] != "cd" {
		return "", false
	}
	args := fields[1:]
	for len(args) > 0 && len(args[0]) > 1 && args[0][0] == '-' && strings.Trim(args[0][1:], "LPe@") == "" {
		args = args[1:]
	}
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return "~", true
	}
	return args[0], true
}

// cdMove is dir after a `cd` segment, for the plain running-directory walks
// (machinecap, plugincache, prodmerge) whose fail-closed direction is "it
// moved": every cd is taken to have applied, and a glob or variable target is
// resolved as spelled — `cd ~/.claude/plugins/cache/*/x && bun install` must
// still land under the guarded tree. ok is false when fields are not a cd;
// `cd -` leaves dir as it is.
func cdMove(fields []string, dir, home string) (string, bool) {
	target, ok := cdTarget(fields)
	if !ok {
		return dir, false
	}
	if target == "-" {
		return dir, true
	}
	return resolveDir(target, dir, home), true
}

// literalPath reports whether a path argument is spelled out: no expansion the
// shell would perform — a variable, a substitution, a glob — stands between
// the text and the directory the command sees.
func literalPath(p string) bool {
	return p != "" && !strings.ContainsAny(p, "$`*?[{")
}

// rooted reports whether p names a directory on its own, independent of the
// directory it is resolved against.
func rooted(p string) bool {
	return filepath.IsAbs(p) || p == "~" || strings.HasPrefix(p, "~/")
}

// subshellParens counts the `(` a segment opens and the trailing `)` it closes,
// the way shellseg.TrimSubshell strips them — Split keeps subshell parens in the
// text (`(cd x` … `a)`) — but outside quotes only, so an echoed ")" cannot pop
// a frame and hand a later command the directory of an earlier one.
func subshellParens(raw string) (opens, closes int) {
	i := 0
	for ; i < len(raw) && strings.IndexByte("( \t", raw[i]) >= 0; i++ {
		if raw[i] == '(' {
			opens++
		}
	}
	depth, trailing := 0, 0
	var quote byte
	for ; i < len(raw); i++ {
		c := raw[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' {
				i++
			}
			trailing = 0
		case c == '\\':
			i++
			trailing = 0
		case c == '\'' || c == '"':
			quote = c
			trailing = 0
		case c == '(':
			depth++
			trailing = 0
		case c == ')':
			depth--
			trailing++
		case c == ' ' || c == '\t':
		default:
			trailing = 0
		}
	}
	if depth < 0 {
		closes = min(-depth, trailing)
	}
	return opens, closes
}
