// Package claudeguards is the single-binary PreToolUse enforcement for the
// hard bans in ~/.claude/CLAUDE.md: destructive git/docker calls, secret and
// environment dumps, host-input automation, /e2e screenshot hygiene,
// commit-time secret scanning, the machine-health rules (`claude-guards list
// machine`) — and the context-budget rules that keep an agent from dumping
// whole files or rewriting them through the shell. Every rule id is a row of
// Rules in registry.go; `claude-guards list` renders it and a test keeps it
// in step with the deny() sites.
//
// CLAUDE.md is context, not enforcement: Claude reads it and *usually*
// complies. These bans are incident-born and must hold unconditionally,
// including under bypassPermissions and inside subagents (where skills don't
// even load). Each hook call is one compiled process; what it costs and the
// paths that cost more are measured in docs/claude-guards.md.
//
// Failure contract: fail OPEN on malformed input (a guard that blocks
// everything on a parse error bricks the session), fail CLOSED only on a
// positive rule match. A Denial is the hook's block decision; the CLI turns it
// into exit 2 with the reason on stderr, which is what Claude sees.
package claudeguards

import "fmt"

// Denial is one positive rule match. Rule is the closed identifier
// ("destructive:git-stash"), Message the plain-language reason, EscapeHatch
// the sanctioned way around it when one exists.
type Denial struct {
	Rule        string
	Message     string
	EscapeHatch string
}

// Text renders the block reason exactly as Claude reads it on stderr.
func (d *Denial) Text() string {
	if d.EscapeHatch == "" {
		return fmt.Sprintf("🚨 BLOCKED by claude-guards (%s)\n\n%s\n", d.Rule, d.Message)
	}
	return fmt.Sprintf("🚨 BLOCKED by claude-guards (%s)\n\n%s\n\n%s\n", d.Rule, d.Message, d.EscapeHatch)
}

func deny(rule, msg, escapeHatch string) *Denial {
	return &Denial{Rule: rule, Message: msg, EscapeHatch: escapeHatch}
}

// Bash evaluates every PreToolUse:Bash rule in this order; the first match
// wins, so an allowed call runs them all and the order only decides what a
// denied call pays. It is not by cost: the first five rules do no I/O,
// guardAppiumChurn is the first to load the repo config (memoized for the
// rest), guardMachineCap may fork `ps -axo`, and guardBudget and
// guardContextBash read the transcript and files. prod-merge and
// commit-secrets run last because they fork git and may call gh (prod-merge
// only for a merge or push command in a repo that declares PROD_BRANCHES).
func Bash(in *HookInput) *Denial {
	for _, g := range []func(*HookInput) *Denial{
		guardDestructive,
		guardPluginCache,
		guardEnvDump,
		guardHostInput,
		guardRootWalk,
		guardAppiumChurn,
		guardTestWorkerCap,
		guardDevboxOnly,
		guardDevboxWhenWorkspace,
		guardMachineCap,
		guardBudget,
		guardContextBash,
		guardE2EScreenshot,
		guardProdMerge,
		guardCommitSecrets,
	} {
		if d := g(in); d != nil {
			return d
		}
	}
	return nil
}

// Read evaluates every PreToolUse:Read rule.
func Read(in *HookInput) *Denial {
	for _, g := range []func(*HookInput) *Denial{guardE2ERead, guardBudget, guardContextRead} {
		if d := g(in); d != nil {
			return d
		}
	}
	return nil
}
