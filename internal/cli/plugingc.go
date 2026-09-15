package cli

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/plugingc"
	"github.com/henderson-tech/vybava/internal/reclaim"
	"github.com/spf13/cobra"
)

func (rt *runtime) pluginGCApplet() *cobra.Command {
	command := rt.pluginGCCommand("plugin-gc")
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetOut(rt.stdout)
	command.SetErr(rt.stderr)
	command.PersistentFlags().BoolVar(&rt.json, "json", false, "emit stable JSON output")
	return command
}

func (rt *runtime) pluginGCCommand(use string) *cobra.Command {
	var (
		apply       bool
		only        []string
		skip        []string
		plugins     []string
		home        string
		orphanGrace time.Duration
	)
	command := &cobra.Command{
		Use:   use,
		Short: "Garbage-collect the Claude Code plugin cache — reports by default, deletes only on --apply",
		Long: `Claude Code keeps every plugin version it has ever installed under
~/.claude/plugins/cache/<marketplace>/<plugin>/<version>/ and refcounts each
one with PID marker files in <version>/.in_use/. A version is reclaimable only
once no marker remains, so an abandoned session pins its version forever —
and the payload is almost entirely node_modules, which nothing reads when a
skill loads.

Three moves, all off until --apply, ordered by how little each can break:

  sweep   drop .in_use markers whose PID is dead — gone, held by something
          that plainly is not a Claude Code session, or held by a process that
          started AFTER the marker was written (a recycled PID)
  strip   delete node_modules under an INACTIVE version — almost every byte,
          without touching a marker or a running session
  remove  delete an inactive version directory once no live marker holds it,
          and an ORPHANED one — "claude plugin uninstall" only writes an
          .orphaned_at stamp and leaves the whole tree behind, so an
          uninstalled plugin's cache outlives both the plugin and its
          marketplace (kept for --orphan-grace, default 7 days)

The active version comes from installed_plugins.json, never from sorting
version strings, and is never touched; neither is the marketplaces/ tree, nor
any version a live session still holds. Anything undecidable is reported and
kept.`,
		Example: `  plugin-gc                         # what would be reclaimed, nothing touched
  plugin-gc --plugin vitrinka       # narrow to one plugin
  plugin-gc --apply                 # sweep dead markers, strip, remove
  plugin-gc --apply --only strip    # the safe, high-yield move alone
  plugin-gc --apply --skip remove   # never delete a version directory
  plugin-gc --orphan-grace 720h     # keep uninstalled plugins' caches 30 days
  plugin-gc --json                  # stable report for agents`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if home == "" {
				resolved, err := plugingc.DefaultHome()
				if err != nil {
					return err
				}
				home = resolved
			}
			onlyMoves, err := plugingc.ParseMoves(only)
			if err != nil {
				return err
			}
			skipMoves, err := plugingc.ParseMoves(skip)
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			report, err := plugingc.Run(ctx, plugingc.Env{Home: home}, plugingc.Options{
				Apply: apply, Only: onlyMoves, Skip: skipMoves, Plugins: plugins,
				OrphanGrace: orphanGrace,
			})
			if err != nil {
				return err
			}
			if rt.json {
				return writeJSON(rt.stdout, report)
			}
			rt.pluginGCReport(report)
			return nil
		},
	}
	command.Flags().BoolVar(&apply, "apply", false, "perform the moves; without it nothing is deleted")
	command.Flags().StringSliceVar(&only, "only", nil, "run only these moves (sweep, strip, remove)")
	command.Flags().StringSliceVar(&skip, "skip", nil, "skip these moves (sweep, strip, remove)")
	command.Flags().StringSliceVar(&plugins, "plugin", nil, "narrow to these plugins, by name or plugin@marketplace")
	command.Flags().StringVar(&home, "home", "", "plugin home to operate on (default ~/.claude/plugins)")
	command.Flags().DurationVar(&orphanGrace, "orphan-grace", plugingc.DefaultOrphanGrace, "keep an uninstalled plugin's versions this long before removing them")
	return command
}

func (rt *runtime) pluginGCReport(report plugingc.Report) {
	for _, warning := range report.Warnings {
		fmt.Fprintf(rt.stdout, "warning: %s\n", warning)
	}
	fmt.Fprintf(rt.stdout, "%s · %d live claude session(s)%s\n", report.Home, report.Sessions, dryLabel(report.DryRun))
	for _, plugin := range report.Plugins {
		active := strings.Join(plugin.Active, ", ")
		if active == "" {
			active = "none — plugin is not installed"
		}
		fmt.Fprintf(rt.stdout, "\n%s  (%d versions, active %s)\n", plugin.Key, len(plugin.Versions), active)
		for _, version := range plugin.Versions {
			fmt.Fprintf(rt.stdout, "  %-14s %-8s %8s  markers %2d live / %2d dead   %s\n",
				trunc(version.Name, 14), version.Plan, reclaim.Human(version.Bytes),
				version.Live, version.Dead, version.Reason)
			// A stale version leaves whole, node_modules with it — only a
			// held one is worth naming a strip for.
			if version.Plan == plugingc.PlanHeld && version.ModuleBytes > 0 {
				fmt.Fprintf(rt.stdout, "  %-14s %-8s %8s  in %d node_modules tree(s)\n", "", "└ strip", reclaim.Human(version.ModuleBytes), len(version.Modules))
			}
			if version.OrphanedAt != nil {
				fmt.Fprintf(rt.stdout, "  %-14s %-8s %8s  uninstalled %s\n", "", "└ orphan", "", version.OrphanedAt.Format("2006-01-02 15:04"))
			}
		}
	}
	verb := "would reclaim"
	if !report.DryRun {
		verb = "reclaimed"
	}
	fmt.Fprintf(rt.stdout, "\n%s %s — %s stripped, %s removed (inactive), %s removed (orphaned — plugin uninstalled), %d dead marker(s) swept\n",
		verb, reclaim.Human(report.Reclaimable()), reclaim.Human(report.StripBytes),
		reclaim.Human(report.RemoveBytes), reclaim.Human(report.OrphanBytes), report.SweepMarkers)
	if report.DryRun {
		fmt.Fprintln(rt.stdout, "nothing was deleted — re-run with --apply")
		return
	}
	fmt.Fprintf(rt.stdout, "actually freed %s\n", reclaim.Human(report.Reclaimed))
	for _, outcome := range report.Outcomes {
		if outcome.Error != "" {
			fmt.Fprintf(rt.stdout, "  FAILED %s %s: %s\n", outcome.Move, outcome.Path, firstLine(outcome.Error))
		}
	}
}
