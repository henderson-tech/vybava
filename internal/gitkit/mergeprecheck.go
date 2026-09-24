package gitkit

import (
	"strconv"
	"strings"
)

// merge-precheck — read-only pre-merge gate gathering for /prm's terminus.

// hookContext fills the {slug}/{branch}/{worktree}/{pr} tokens of a
// configured AFTER_MERGE_CMD or BEFORE_REVIEW_CMD.
type hookContext struct {
	slug, branch, worktree string
	pr                     int
}

// substituteHookTokens replaces every occurrence of each token, in order; a
// command with no tokens is returned unchanged.
func substituteHookTokens(cmd string, ctx hookContext) string {
	cmd = strings.ReplaceAll(cmd, "{slug}", ctx.slug)
	cmd = strings.ReplaceAll(cmd, "{branch}", ctx.branch)
	cmd = strings.ReplaceAll(cmd, "{worktree}", ctx.worktree)
	return strings.ReplaceAll(cmd, "{pr}", strconv.Itoa(ctx.pr))
}
