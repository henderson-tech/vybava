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
// the directories it may run in. When the text before it is a pure `&&`
// chain (`(cd .worktrees/x && tsc)`, `cd apps/api && lint && tsc`) each `cd`
// provably applies, so the one tracked directory is the answer. Any other shape
// (`;`, `||`, a closing subshell, a pipe) leaves it open where the command runs
// - `(cd x && echo); tsc` runs in the session's checkout, `cd x || tsc` runs
// where the cd failed - so the session cwd and every cd target are candidates.
// A bun `--cwd <dir>` on the segment moves each candidate.
func devboxMatch(cmd, cwd string, patterns []*regexp.Regexp) (devboxHit, bool) {
	if len(patterns) == 0 {
		return devboxHit{}, false
	}
	home, _ := os.UserHomeDir()
	dir, targets := cwd, []string{cwd}
	for _, raw := range localSegments(cmd) {
		if f := shellFields(strings.TrimSpace(trimSubshell(raw))); len(f) > 1 && f[0] == "cd" {
			dir = resolveDir(f[1], dir, home)
			targets = append(targets, dir)
			continue
		}
		if textOnly(raw) {
			continue
		}
		s := strings.TrimSpace(trimAssignments(trimSubshell(raw)))
		if s == "" {
			continue
		}
		for _, re := range patterns {
			if !re.MatchString(s) && !re.MatchString(unwrapRunners(s)) {
				continue
			}
			full := leadingAssignments(cmd, s) + s
			candidates := []string{dir}
			if at := strings.Index(cmd, full); at < 0 || !pureAndChain(cmd[:at]) {
				candidates = append(candidates, targets...)
			}
			hit := devboxHit{seg: s, full: full}
			for _, c := range candidates {
				if r := bunCwd(s, c, home); !slices.Contains(hit.runs, r) {
					hit.runs = append(hit.runs, r)
				}
			}
			return hit, true
		}
	}
	return devboxHit{}, false
}

// pureAndChain reports whether the text before a command is empty or only
// `&&`-joined commands, optionally opened by one `(`: then every `cd` in it
// runs, in order, in the command's own shell. Anything with `;`, `|`, `||`,
// `&`, a parenthesis, a backquote or a newline is not.
func pureAndChain(prefix string) bool {
	p := strings.TrimSpace(prefix)
	return p == "" || andChain.MatchString(p)
}

var andChain = regexp.MustCompile("^\\(?\\s*([^;&|()`\\n]+&&\\s*)+$")

// leadingAssignments is the `VAR=value ` run written right before seg in cmd
// (localSegments strips it), so a rerun keeps NODE_OPTIONS=… and the like.
func leadingAssignments(cmd, seg string) string {
	re := regexp.MustCompile(`((?:[A-Za-z_][A-Za-z0-9_]*=(?:'[^']*'|"[^"]*"|[^\s'"]*)[ \t]+)+)` + regexp.QuoteMeta(seg))
	if m := re.FindStringSubmatch(cmd); m != nil {
		return m[1]
	}
	return ""
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
var bunCwdFlag = regexp.MustCompile(`\s--cwd(=|\s+)\S+`)

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
