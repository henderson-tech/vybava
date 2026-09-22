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
// even load). One compiled process per Bash call costs 15-30 ms, nearly all of
// it process start for this binary: warm repeats sit at the low end, a cold
// exec on a memory-pressured Mac at the high end. Most rules add nothing
// measurable on top. Three paths do: commit-secrets roughly doubles the call
// when `git commit` has staged changes (it diffs them, and may refresh repo
// visibility), and machine:sim-cap / machine:dev-server-cap add ~450-630 ms for
// one `ps -axo` — but only when a command actually matches a boot or a
// dev-server start. The weather and reap verbs read the same process table
// unconditionally, once at SessionStart and again at SessionEnd, never per
// command.
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

// Bash evaluates every PreToolUse:Bash rule, cheapest first; commit-secrets
// last because it may do git/network IO. The first match wins.
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
		guardMachineCap,
		guardBudget,
		guardContextBash,
		guardE2EScreenshot,
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
