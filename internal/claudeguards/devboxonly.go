package claudeguards

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// ---------------------------------------------------------------------------
// machine:devbox-only - a command this repo routes to the Devbox, run on the
// Mac. FixIt's rule since 2026-09-17 sends apps/api, apps/web and
// apps/admin-web dev servers, Docker stacks and test suites to the branch's
// Devbox workspace; as prose it was ignored by enough sessions that on
// 2026-09-19/20 three Metro bundlers, four API servers and two next-servers
// (one at 7.2 GB) ran here anyway. The repo names the commands in
// guards.devboxOnly (RE2, matched against one local command segment) and
// this rule refuses them unless they are carried by `devbox run` or `ssh`.
// A hand test the user asked for on this Mac sets
// CLAUDE_GUARDS_ALLOW_LOCAL_STACK=1.
// ---------------------------------------------------------------------------

const devboxOnlyEscape = "A hand test the user asked for on this Mac: CLAUDE_GUARDS_ALLOW_LOCAL_STACK=1 <command>"

// devboxHit is the local segment a devbox rule matched, and where it may run.
type devboxHit struct {
	seg  string   // the command, assignments and subshell stripped (shown, matched)
	full string   // the same with its leading VAR=value assignments (rerun payload)
	runs []string // every directory it may run in, the likeliest first (see devboxMatch)
}

// devboxMatch returns the first local segment matching one of patterns and
// the directories it may run in. When every `cd` before it provably applies
// (cdsCertain: `(cd .worktrees/x && tsc)`, `echo; cd x && lint && tsc`) the one
// tracked directory is the answer. Otherwise it is open where the command runs
// - `(cd x && echo); tsc` runs in the session's checkout, `cd x || tsc` runs
// where the cd failed - so every directory the command could be in is a
// candidate: each `cd` may or may not have applied, and a relative one is
// resolved against every directory possible at that point (capped at
// maxDevboxCandidates). A bun `--cwd <dir>` on the segment moves each.
func devboxMatch(cmd, cwd string, patterns []*regexp.Regexp) (devboxHit, bool) {
	if len(patterns) == 0 {
		return devboxHit{}, false
	}
	home, _ := os.UserHomeDir()
	dir, possible := cwd, []string{cwd}
	cursor := 0 // segments come in order: each is located after the previous one
	for _, raw := range localSegments(cmd) {
		text := strings.TrimSpace(trimAssignments(trimSubshell(raw)))
		pos := -1
		if at := strings.Index(cmd[cursor:], text); text != "" && at >= 0 {
			pos = cursor + at
			cursor = pos + len(text)
		}
		if f := shellFields(text); len(f) > 1 && f[0] == "cd" {
			dir = resolveDir(f[1], dir, home)
			for _, d := range possible {
				if next := resolveDir(f[1], d, home); !slices.Contains(possible, next) && len(possible) < maxDevboxCandidates {
					possible = append(possible, next)
				}
			}
			continue
		}
		if textOnly(raw) || text == "" {
			continue
		}
		for _, re := range patterns {
			if !re.MatchString(text) && !re.MatchString(unwrapRunners(text)) {
				continue
			}
			prefix, assigns := "", ""
			if pos >= 0 {
				prefix = cmd[:pos]
				if m := assignmentTail.FindString(prefix); m != "" {
					assigns, prefix = m, prefix[:len(prefix)-len(m)]
				}
			}
			candidates := []string{dir}
			if pos < 0 || !cdsCertain(prefix) {
				candidates = append(candidates, possible...)
			}
			hit := devboxHit{seg: text, full: assigns + text}
			for _, c := range candidates {
				if r := bunCwd(text, c, home); !slices.Contains(hit.runs, r) {
					hit.runs = append(hit.runs, r)
				}
			}
			return hit, true
		}
	}
	return devboxHit{}, false
}

// maxDevboxCandidates bounds the directories one command is judged in.
const maxDevboxCandidates = 32

// assignmentTail is the `VAR=value ` run right before a command (localSegments
// strips it), so a rerun keeps NODE_OPTIONS=… and the like.
var assignmentTail = regexp.MustCompile(`(?:[A-Za-z_][A-Za-z0-9_]*=(?:'[^']*'|"[^"]*"|[^\s'"]*)[ \t]+)+$`)

// cdsCertain reports whether every `cd` in the text before a command provably
// moved it: each cd is followed by `&&` (so the command runs only if the cd
// succeeded) and sits in no subshell that closes before the command. `echo;
// cd x && tsc` is certain; `cd x; tsc`, `cd x || tsc`, `(cd x && a); tsc` are
// not. Quotes are respected; a command substitution or backquote is never
// certain.
func cdsCertain(prefix string) bool {
	depth := 0
	var cdDepths []int
	var seg strings.Builder
	var quote byte
	endSegment := func(sep string) bool {
		s := strings.TrimSpace(trimAssignments(seg.String()))
		seg.Reset()
		if f := strings.Fields(s); len(f) > 0 && f[0] == "cd" {
			if sep != "&&" {
				return false
			}
			cdDepths = append(cdDepths, depth)
		}
		return true
	}
	for i := 0; i < len(prefix); i++ {
		c := prefix[i]
		if quote != 0 {
			seg.WriteByte(c)
			if c == quote {
				quote = 0
			}
			continue
		}
		switch {
		case c == '\'' || c == '"':
			quote = c
			seg.WriteByte(c)
		case c == '`' || (c == '$' && i+1 < len(prefix) && prefix[i+1] == '('):
			return false
		case c == '(':
			if !endSegment("(") {
				return false
			}
			depth++
		case c == ')':
			if !endSegment(")") {
				return false
			}
			depth--
			for _, d := range cdDepths {
				if d > depth {
					return false // its subshell closed: the cd no longer applies
				}
			}
		case strings.HasPrefix(prefix[i:], "&&"):
			if !endSegment("&&") {
				return false
			}
			i++
		case strings.HasPrefix(prefix[i:], "||"):
			if !endSegment("||") {
				return false
			}
			i++
		case c == ';' || c == '|' || c == '&' || c == '\n':
			if !endSegment(string(c)) {
				return false
			}
		default:
			seg.WriteByte(c)
		}
	}
	return quote == 0 && strings.TrimSpace(seg.String()) == ""
}

// bunCwd is dir moved by a bun `--cwd <dir>` / `--cwd=<dir>` in the segment.
func bunCwd(seg, dir, home string) string {
	f := shellFields(seg)
	if len(f) == 0 || commandWord(f[0]) != "bun" {
		return dir
	}
	for i, a := range f {
		if v, ok := strings.CutPrefix(a, "--cwd="); ok {
			return resolveDir(v, dir, home)
		}
		if a == "--cwd" && i+1 < len(f) {
			return resolveDir(f[i+1], dir, home)
		}
	}
	return dir
}

// devboxRerun is the `devbox run` line for a command that runs in dir. devbox
// run starts at the root of the checkout it is invoked from, so the payload
// gets `cd <dir relative to that root>` - with a bun `--cwd` folded into it and
// dropped from the command - and keeps its leading assignments (NODE_OPTIONS=…).
// A checkout other than the session's is entered first.
func devboxRerun(h devboxHit, dir, sessionCwd, flags string) string {
	payload := h.full
	dest := checkoutRoot(dir)
	if dest != "" {
		payload = bunCwdFlag.ReplaceAllString(payload, "")
		if rel, err := filepath.Rel(dest, dir); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			payload = "cd " + shellArg(rel) + " && " + payload
		}
	}
	line := "devbox run" + flags + " -- " + shellSingleQuote(payload)
	if dest != "" && dest != checkoutRoot(sessionCwd) {
		line = "(cd " + shellArg(dest) + " && " + line + ")"
	}
	return line
}

// bunCwdFlag is a bun `--cwd <dir>` / `--cwd=<dir>`, folded into the rerun's cd.
var bunCwdFlag = regexp.MustCompile(`\s--cwd(?:=|\s+)(?:'[^']*'|"[^"]*"|[^\s'"]+)+`)

// shellArg is s as one shell argument: verbatim when the shell reads it so.
func shellArg(s string) string {
	if plainWord.MatchString(s) {
		return s
	}
	return shellSingleQuote(s)
}

// plainWord is a path the shell reads verbatim, needing no quotes.
var plainWord = regexp.MustCompile(`^[A-Za-z0-9._/+-]+$`)

// unwrapRunners drops a leading wrapper chain (`timeout 600`, `nice -n 5`,
// `env FOO=1`) so a pattern anchored at the command still sees it. The
// result is fields rejoined with single spaces: good enough to match, never
// shown to the user.
func unwrapRunners(s string) string {
	f := shellFields(s)
	for len(f) > 0 && commandRunners[commandWord(f[0])] {
		f = f[1:]
		for len(f) > 0 && (strings.HasPrefix(f[0], "-") || assignPrefix.MatchString(f[0]) || isDigits(f[0])) {
			f = f[1:]
		}
	}
	return strings.Join(f, " ")
}

func guardDevboxOnly(in *HookInput) *Denial {
	cmd := in.ToolInput.Command
	if cmd == "" || escapeHatch(cmd, "CLAUDE_GUARDS_ALLOW_LOCAL_STACK") {
		return nil
	}
	cfg := in.guards()
	if len(cfg.DevboxOnly) == 0 {
		return nil
	}
	hit, ok := devboxMatch(cmd, in.CWD, compileDevboxPatterns(cfg.DevboxOnly))
	if !ok {
		return nil
	}
	return deny("machine:devbox-only", fmt.Sprintf(`%s

runs on this Mac; this repo's guards.devboxOnly routes it to the Devbox:
    %s
From a worktree without a workspace, /devbox resolves-or-creates one. The
Mac keeps simulators, Appium specs and native builds; everything else that
serves or tests apps/api, apps/web and apps/admin-web runs on the box.`, hit.seg, devboxRerun(hit, hit.runs[0], in.CWD, "")), devboxOnlyEscape)
}
