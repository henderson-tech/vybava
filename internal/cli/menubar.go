package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	goruntime "runtime"
	"time"

	"github.com/henderson-tech/vybava/internal/menubar"
	"github.com/spf13/cobra"
)

func (rt *runtime) menubarApplet() *cobra.Command {
	command := rt.menubarCommand("menubar-doctor")
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetOut(rt.stdout)
	command.SetErr(rt.stderr)
	command.PersistentFlags().BoolVar(&rt.json, "json", false, "emit stable JSON output")
	return command
}

func (rt *runtime) menubarCommand(use string) *cobra.Command {
	return rt.menubarCommandWithEnv(use, rt.menubarEnv)
}

// menubarCommandWithEnv takes the machine as a seam so the command surface is
// testable off macOS, where the registry this reads does not exist.
func (rt *runtime) menubarCommandWithEnv(use string, newEnv func() (menubar.Env, error)) *cobra.Command {
	command := &cobra.Command{
		Use:   use,
		Short: "Find and fix macOS menu-bar items that run but never appear",
		Long: `Since macOS 26, Control Center files every third-party menu-bar item under the
responsible process of whatever launched the app — not under the app itself. An
app started from a terminal (open -a, a build script, xcodebuild, an agent
shell) is filed under the TERMINAL's bundle id, and when that terminal's "Allow
in the Menu Bar" switch is off, every item filed under it is silently never
hosted: the process runs, its status-item window is parked at a screen edge or
under the clock, and no icon is drawn.

The attribution is persisted, so relaunching, reinstalling, restarting Control
Center, changing displays and even logging out all leave the item invisible.

Bare, this scans and names every item filed under a foreign owner, exiting 1
when any of them is invisible. "fix" strips those mappings so each app is
attributed to itself again and restarts Control Center; "launch" starts an app
detached from this shell, which is how you avoid the trap in the first place.`,
		Example: `  menubar-doctor                                  # what is filed under whom
  menubar-doctor fix                              # repair the invisible ones
  menubar-doctor launch /Applications/Foo.app     # start it attributed to itself`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := newEnv()
			if err != nil {
				return err
			}
			report, err := menubar.Scan(env)
			if err != nil {
				return err
			}
			if rt.json {
				if err := writeJSON(rt.stdout, report); err != nil {
					return err
				}
			} else if err := rt.printMenubarReport(report); err != nil {
				return err
			}
			if len(report.Blocked()) > 0 {
				return ErrFindings
			}
			return nil
		},
	}
	command.AddCommand(rt.menubarFixCommand(newEnv), rt.menubarLaunchCommand(newEnv))
	return command
}

func (rt *runtime) menubarFixCommand(newEnv func() (menubar.Env, error)) *cobra.Command {
	var all bool
	command := &cobra.Command{
		Use:   "fix",
		Short: "Strip foreign attributions, back up the registry, restart Control Center",
		Long: `Backs the Control Center registry up to ~/Backups/menubar-doctor/, strips the
mappings that hide items under a switched-off owner, writes it back through
"defaults import" and restarts the preferences daemon and Control Center.

By default only the invisible items are repaired. An item filed under an owner
that IS allowed is visible right now, so stripping it would make it disappear
until its app is relaunched; --all does that too.

Each repaired app has to be relaunched outside this shell — "menubar-doctor
launch <app>" does it, and so does Finder or Spotlight.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env, err := newEnv()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			result, err := menubar.Fix(ctx, env, all)
			if err != nil {
				return err
			}
			if rt.json {
				return writeJSON(rt.stdout, result)
			}
			if len(result.Stripped) == 0 {
				_, err := fmt.Fprintln(rt.stdout, "nothing to repair — every menu-bar item is filed under its own app")
				return err
			}
			if _, err := fmt.Fprintf(rt.stdout, "backed up %s\n", result.Backup); err != nil {
				return err
			}
			for _, finding := range result.Stripped {
				if _, err := fmt.Fprintf(rt.stdout, "unfiled %s from %s\n", finding.Item, finding.Owner); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintf(rt.stdout, "\nControl Center restarted. Relaunch each app OUTSIDE this shell, e.g.\n  menubar-doctor launch /Applications/<App>.app\n")
			return err
		},
	}
	command.Flags().BoolVar(&all, "all", false, "also unfile items that are currently visible under a foreign owner")
	return command
}

func (rt *runtime) menubarLaunchCommand(newEnv func() (menubar.Env, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "launch <app-or-binary>",
		Short: "Start a menu-bar app detached from this shell so its item is attributed to itself",
		Long: `Runs the app under launchd instead of as a child of this terminal, so Control
Center files its menu-bar item under the app rather than under the terminal.
Takes an .app bundle or an executable. Use it whenever a script, a build or an
agent starts a menu-bar app — "open -a" is what creates the invisible-item trap.`,
		Example: `  menubar-doctor launch /Applications/SwitcherooBar.app`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env, err := newEnv()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()
			label, err := menubar.Launch(ctx, env, args[0])
			if err != nil {
				return err
			}
			if rt.json {
				return writeJSON(rt.stdout, map[string]string{"label": label, "target": args[0]})
			}
			_, err = fmt.Fprintf(rt.stdout, "launched %s as %s (launchctl remove %s to forget the job)\n", args[0], label, label)
			return err
		},
	}
}

func (rt *runtime) menubarEnv() (menubar.Env, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return menubar.Env{}, err
	}
	return menubar.Env{
		Home: home, GOOS: goruntime.GOOS, Now: time.Now(),
		ReadFile:  os.ReadFile,
		WriteFile: os.WriteFile,
		MkdirAll:  os.MkdirAll,
		Exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, name, args...).CombinedOutput()
		},
	}, nil
}

func (rt *runtime) printMenubarReport(report menubar.Report) error {
	if len(report.Findings) == 0 {
		_, err := fmt.Fprintf(rt.stdout, "clean — all %d tracked apps own their menu-bar items\n", len(report.Owners))
		return err
	}
	for _, finding := range report.Findings {
		state := "visible, but filed under"
		if finding.Blocked {
			state = "INVISIBLE — filed under the switched-off"
		}
		if _, err := fmt.Fprintf(rt.stdout, "%-44s %s %s\n", finding.Item, state, finding.Owner); err != nil {
			return err
		}
	}
	if len(report.Blocked()) == 0 {
		return nil
	}
	_, err := fmt.Fprintf(rt.stdout, "\n%d item(s) will never appear. Repair with:\n  menubar-doctor fix\n", len(report.Blocked()))
	return err
}
