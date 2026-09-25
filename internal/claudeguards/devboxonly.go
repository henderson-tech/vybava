package claudeguards

import (
	"fmt"
	"regexp"
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

// devboxOnlyMatch returns the first local segment matching one of patterns.
func devboxOnlyMatch(cmd string, patterns []*regexp.Regexp) string {
	if len(patterns) == 0 {
		return ""
	}
	for _, seg := range localSegments(cmd) {
		if textOnly(seg) {
			continue
		}
		s := strings.TrimSpace(trimAssignments(trimSubshell(seg)))
		if s == "" {
			continue
		}
		for _, re := range patterns {
			if re.MatchString(s) || re.MatchString(unwrapRunners(s)) {
				return s
			}
		}
	}
	return ""
}

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
	seg := devboxOnlyMatch(cmd, compileDevboxPatterns(cfg.DevboxOnly))
	if seg == "" {
		return nil
	}
	return deny("machine:devbox-only", fmt.Sprintf(`%s

runs on this Mac; this repo's guards.devboxOnly routes it to the Devbox:
    devbox run -- '%s'
From a worktree without a workspace, /devbox resolves-or-creates one. The
Mac keeps simulators, Appium specs and native builds; everything else that
serves or tests apps/api, apps/web and apps/admin-web runs on the box.`, seg, seg), devboxOnlyEscape)
}
