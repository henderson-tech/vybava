package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/henderson-tech/vybava/internal/installer"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/spf13/cobra"
)

// upgradeCommand moves the cask to the latest release, then replaces this
// process with the NEW binary's `update` — the running binary still embeds
// the old payload, so refreshing skills from it would reinstall stale files.
func (rt *runtime) upgradeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade the Homebrew cask, then refresh every installed package from the new release",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			s := &runx.Session{Tool: "vybava", JSON: rt.json, Verb: "upgrade", Stdout: rt.stdout, Stderr: rt.stderr}
			fail := func(code, detail, fix string) error {
				diag := runx.DiagError{Diag: runx.Diagnostic{Code: code, Severity: "error", Detail: detail, Fix: fix}}
				return runx.ExitError{Code: s.Finish(diag)}
			}
			executable, err := os.Executable()
			if err != nil {
				return runx.ExitError{Code: s.Finish(err)}
			}
			resolved, err := filepath.EvalSymlinks(executable)
			if err != nil {
				return runx.ExitError{Code: s.Finish(err)}
			}
			if !installer.BrewCask(resolved) {
				return fail("UPGRADE_NOT_CASK", "this vybava runs from "+resolved+", not the Homebrew cask; source and CI installs pin their own version",
					"brew install --cask "+installer.CaskName)
			}
			brew, err := exec.LookPath("brew")
			if err != nil {
				return fail("UPGRADE_BREW_MISSING", "brew is not on PATH", "eval \"$(/opt/homebrew/bin/brew shellenv)\"")
			}
			// brew's progress goes to stderr: stdout stays the new binary's report.
			upgrade := exec.Command(brew, "upgrade", "--cask", installer.CaskName)
			upgrade.Stdout, upgrade.Stderr = rt.stderr, rt.stderr
			if err := upgrade.Run(); err != nil {
				return fail("UPGRADE_BREW_FAILED", "brew upgrade --cask failed: "+err.Error(), "brew upgrade --cask "+installer.CaskName)
			}
			prefix, err := exec.Command(brew, "--prefix").Output()
			if err != nil {
				return fail("UPGRADE_BREW_FAILED", "brew --prefix failed: "+err.Error(), "brew doctor")
			}
			next := filepath.Join(strings.TrimSpace(string(prefix)), "bin", "vybava")
			argv := []string{next, "update"}
			if rt.json {
				argv = append(argv, "--json")
			}
			return syscall.Exec(next, argv, os.Environ())
		},
	}
}
