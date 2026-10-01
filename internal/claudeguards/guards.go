// Package claudeguards is the single-binary PreToolUse enforcement for the
// hard bans in ~/.claude/CLAUDE.md: destructive git/docker calls, secret and
// environment dumps, host-input automation, /e2e screenshot hygiene,
// pattern kills and kills of agent sessions (`claude-guards list process`),
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
// denied call pays. It is not by cost: the first eight rules do no I/O (the
// memo ledger rule stats a directory only for a write to a file named
// LEDGER.md, MEMORY.md or usage.jsonl), guardHeavyWalk reads a bounded slice of the tree only for an uncapped
// find/bfs, guardAppiumChurn is the first to load the repo config (memoized for the
// rest), guardProcessKill forks `ps -A` only for a kill naming a literal PID,
// guardMachineCap may fork `ps -axo`, and guardBudget and
// guardContextBash read the transcript and files. prod-merge and
// commit-secrets run last because they fork git and may call gh (prod-merge
// only for a merge or push command in a repo that declares PROD_BRANCHES).
func Bash(in *HookInput) *Denial {
	for _, g := range []func(*HookInput) *Denial{
		guardDestructive,
		guardPluginCache,
		guardEnvDump,
		guardSecretPrint,
		guardHostInput,
		guardRootWalk,
		guardDevboxSSHExec,
		guardMemoLedger,
		guardHeavyWalk,
		guardAppiumChurn,
		guardTestWorkerCap,
		guardDesktopUITests,
		guardDevboxOnly,
		guardDevboxWhenWorkspace,
		guardProcessKill,
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

// Codex evaluates the Bash rules a Codex session breaks things with as easily
// as a Claude one: the hard bans, the secret dumps, the machine-load rules and
// the merge and commit gates. Codex speaks the same hook contract (cwd and
// tool_input.command on stdin, exit 2 blocks). The context-budget rules read a
// Claude transcript and the /e2e rules a Claude skill, so they stay in Bash.
func Codex(in *HookInput) *Denial {
	for _, g := range []func(*HookInput) *Denial{
		guardDestructive,
		guardPluginCache,
		guardEnvDump,
		guardSecretPrint,
		guardHostInput,
		guardRootWalk,
		guardDevboxSSHExec,
		guardHeavyWalk,
		guardAppiumChurn,
		guardTestWorkerCap,
		guardDesktopUITests,
		guardDevboxOnly,
		guardDevboxWhenWorkspace,
		guardProcessKill,
		guardMachineCap,
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
