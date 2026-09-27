package doctor

import (
	"context"
	"os/exec"
	"time"
)

// The agent harness runs every Bash tool call in a NON-interactive login
// zsh, which reads ~/.zshenv and ~/.zprofile and keeps zsh's `nomatch`
// option: an unmatched glob aborts the whole command before it runs. In the
// three days to 2026-09-27 that killed 6,674 agent commands, ~4,700 of them
// `grep --include=*.ts …` (zsh globbed the flag). `setopt nonomatch` for
// non-interactive shells restores bash's behaviour (the literal is passed
// through); interactive shells are left alone.
const shellGlobRemedy = "append to ~/.zshenv:  [[ -o interactive ]] || setopt nonomatch"

// agentShellNomatch probes the shell the harness uses: a non-interactive
// login zsh. on reports whether `nomatch` is set there; ok is false when zsh
// is missing or the probe could not run.
func agentShellNomatch() (on, ok bool) {
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		return false, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Exit 0 = the option is set; exit 1 = unset; anything else = no answer.
	err = exec.CommandContext(ctx, zsh, "-l", "-c", "[[ -o nomatch ]]").Run()
	if err == nil {
		return true, true
	}
	if exit, isExit := err.(*exec.ExitError); isExit && exit.ExitCode() == 1 {
		return false, true
	}
	return false, false
}

// agentShellGlobCheck is the doctor row for the probe above.
func agentShellGlobCheck(probe func() (on, ok bool)) Check {
	on, ok := probe()
	switch {
	case !ok:
		return Check{ID: "agent shell globs", Status: StatusPass, Message: "zsh not probed (not installed, or the probe failed); nothing to fix on this machine"}
	case on:
		return Check{ID: "agent shell globs", Status: StatusWarn,
			Message: "non-interactive zsh keeps `nomatch`: an unmatched glob (grep --include=*.ts …) aborts the whole agent command",
			Remedy:  shellGlobRemedy}
	default:
		return Check{ID: "agent shell globs", Status: StatusPass, Message: "non-interactive zsh passes unmatched globs through (nonomatch)"}
	}
}
