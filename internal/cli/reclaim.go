package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/reclaim"
	"github.com/spf13/cobra"
)

func (rt *runtime) reclaimApplet() *cobra.Command {
	command := rt.reclaimCommand("reclaim")
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetOut(rt.stdout)
	command.SetErr(rt.stderr)
	command.PersistentFlags().BoolVar(&rt.json, "json", false, "emit stable JSON output")
	return command
}

func (rt *runtime) reclaimCommand(use string) *cobra.Command {
	var (
		tier     int
		until    string
		dryRun   bool
		only     []string
		skip     []string
		keepDays int
		list     bool
	)
	command := &cobra.Command{
		Use:   use,
		Short: "Emergency disk reclaim — delete regenerating caches biggest-first, no scan, no prompt",
		Long: `Frees disk space on a dev machine in seconds by running a fixed ladder of
deletions, each of which regenerates on its own. Nothing is scanned first;
steps in a tier run concurrently and every finished step prints the volume's
free space at that moment, so partial wins land while the rest is working.

Tiers: 1 build/package caches (Go, Docker build cache + images, bun, npm,
DerivedData, gradle, pnpm, cargo, pip/uv) · 2 tool/app caches, brew, logs,
orphaned Playwright revisions, dead simulators · 3 aggressive but reversible
(iOS DeviceSupport, device-less simulator runtimes, simulator logs, aged
Messages/app sandbox temp, Trash). Default runs all three.

Never touched: Docker volumes and containers, screen recordings, anything
unclassified — those are surfaced as by-hand notes at the end.`,
		Example: `  reclaim                 # everything reversible, biggest-first
  reclaim --until 100G    # stop as soon as 100G is free
  reclaim --tier 1        # only the huge build/package caches
  reclaim --dry-run       # size the ladder, delete nothing
  reclaim --list          # print the ladder`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			env := reclaim.Env{
				Home: home, Volume: home, Now: time.Now(), GOOS: goruntime.GOOS,
				LookPath: exec.LookPath,
				Free:     reclaim.Free,
				Exec: func(ctx context.Context, name string, args ...string) ([]byte, error) {
					cmd := exec.CommandContext(ctx, name, args...)
					// Never inherit the caller's project cwd: pnpm/npm/yarn refuse to run
					// inside a repo whose packageManager pins another tool.
					cmd.Dir = home
					out, err := cmd.CombinedOutput()
					if err != nil {
						if line := firstLine(strings.TrimSpace(string(out))); line != "" {
							err = fmt.Errorf("%w: %s", err, line)
						}
					}
					return out, err
				},
				Stderr: func(s string) { fmt.Fprintln(rt.stderr, s) },
			}
			opts := reclaim.Options{MaxTier: reclaim.Tier(tier), DryRun: dryRun, Only: only, Skip: skip, KeepDays: keepDays}
			if until != "" {
				n, err := reclaim.ParseHuman(until)
				if err != nil {
					return err
				}
				opts.Until = n
			}
			if list {
				return rt.reclaimList(reclaim.Plan(env, opts))
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			var progress reclaim.Progress
			if !rt.json {
				free, total, err := reclaim.Free(home)
				if err != nil {
					return err
				}
				fmt.Fprintf(rt.stdout, "BEFORE  free %s of %s%s\n", reclaim.Human(free), reclaim.Human(total), dryLabel(dryRun))
				progress = &reclaimPrinter{rt: rt}
			}
			report, err := reclaim.Run(ctx, env, opts, progress)
			if rt.json {
				return writeJSON(rt.stdout, report)
			}
			if err != nil {
				fmt.Fprintf(rt.stdout, "interrupted: %v\n", err)
			}
			rt.reclaimSummary(report)
			return nil
		},
	}
	command.Flags().IntVar(&tier, "tier", 3, "highest tier to run (1 bulk caches · 2 tool caches · 3 aggressive)")
	command.Flags().StringVar(&until, "until", "", "stop once this much is free, e.g. 100G")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "size each step, delete nothing")
	command.Flags().StringSliceVar(&only, "only", nil, "run only these step ids")
	command.Flags().StringSliceVar(&skip, "skip", nil, "skip these step ids")
	command.Flags().IntVar(&keepDays, "keep-days", 60, "aged steps keep files newer than this")
	command.Flags().BoolVar(&list, "list", false, "print the ladder and exit")
	command.AddCommand(rt.bunPruneCommand())
	return command
}

func (rt *runtime) bunPruneCommand() *cobra.Command {
	var (
		apply  bool
		minAge time.Duration
		top    int
	)
	command := &cobra.Command{
		Use:   "bun-prune [checkout]",
		Short: "Report, then with --apply delete, a checkout's unreachable node_modules/.bun directories and .old_modules leftovers",
		Long: `bun never deletes a node_modules/.bun entry its lockfile stopped naming, and a
linker switch leaves node_modules/.old_modules-<hash> behind. This walks one
checkout (default: the current directory) from its roots - node_modules, each
workspace package's node_modules and .bun/node_modules - through every
project-local entry's own links, and lists the project-local .bun directories
nothing reaches, with sizes (hardlinks counted once; df after --apply is the
truth). Unreachable symlinks into the global store are only counted: bun links
every lockfile package, so a fresh install has hundreds, and each frees nothing.
Entries only an ios/Podfile.lock still names are kept and listed apart: the
Pods project builds from them until the next pod install.

Report only by default. --apply refuses while a bun install-family process
(install, add, remove, update, link, pm, patch, init, create and their aliases,
after any global flags) has its cwd in the checkout. Any other bun there (a
dev server, a script such as a session launcher, bunx) only resolves modules
and does not count, nor does one inside a nested checkout with its own .git and
package.json, such as a worktree at .worktrees/<slug>: bun installs that
project, not this one. It keeps anything modified within --min-age, walks
again right before deleting (refusing if what it read changed meanwhile, as an
install that ran during the walk leaves it), and runs at background priority
(the dry run does not: the throttle starves a read-only walk on a loaded Mac).
The global store (~/.bun/install/cache/links) is never touched.`,
		Example: `  reclaim bun-prune ~/Work/app              # dry run
  reclaim bun-prune ~/Work/app --apply      # delete what the dry run listed
  reclaim bun-prune . --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			checkout := "."
			if len(args) == 1 {
				checkout = args[0]
			}
			// Only the delete runs in the background band: on a loaded Mac
			// (load ~250 on 14 cores) its I/O throttle starved even the
			// read-only walk of a fresh worktree past two minutes.
			if apply {
				if err := reclaim.Background(); err != nil {
					fmt.Fprintf(rt.stderr, "warning: background priority not set: %v\n", err)
				}
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			report, err := reclaim.BunPrune(ctx, reclaim.BunPruneOptions{Checkout: checkout, Apply: apply, MinAge: minAge})
			if err != nil {
				return err
			}
			if rt.json {
				return writeJSON(rt.stdout, report)
			}
			rt.bunPruneSummary(report, top, minAge)
			return nil
		},
	}
	command.Flags().BoolVar(&apply, "apply", false, "delete what the report lists (default: report only)")
	command.Flags().DurationVar(&minAge, "min-age", 24*time.Hour, "keep candidates modified more recently than this")
	command.Flags().IntVar(&top, "top", 15, "unreachable entries to list by size")
	return command
}

func (rt *runtime) bunPruneSummary(r reclaim.BunPruneReport, top int, minAge time.Duration) {
	mode := "dry run - nothing is deleted"
	if r.Applied {
		mode = "applied"
	}
	fmt.Fprintf(rt.stdout, "bun-prune %s  (%s)\n", r.Checkout, mode)
	fmt.Fprintf(rt.stdout, ".bun entries %d · reachable %d · unreachable dirs %d · unreachable symlinks %d (kept: bun links every lockfile package, each frees nothing)\n",
		r.Entries, r.Reachable, len(r.Unreachable), r.UnreachableLinks)
	if !r.Applied {
		for i, e := range r.Unreachable {
			if i == top {
				fmt.Fprintf(rt.stdout, "  … %d more (--top, --json)\n", len(r.Unreachable)-top)
				break
			}
			fmt.Fprintf(rt.stdout, "  %8s  %s  %s\n", reclaim.Human(e.Bytes), e.Modified.Format("2006-01-02"), e.Name)
		}
	}
	for _, e := range r.Leftovers {
		size := reclaim.Human(e.Bytes)
		if r.Applied {
			size = "deleted" // --apply does not size first; the df delta below is the figure
		}
		fmt.Fprintf(rt.stdout, "  %8s  %s  node_modules/%s (leftover)\n", size, e.Modified.Format("2006-01-02"), e.Name)
	}
	if len(r.Young) > 0 {
		fmt.Fprintf(rt.stdout, "kept %d candidates modified within %s\n", len(r.Young), minAge)
	}
	if len(r.Native) > 0 {
		size := ""
		if !r.Applied {
			size = " (" + reclaim.Human(r.NativeBytes) + ")"
		}
		fmt.Fprintf(rt.stdout, "kept %d entries%s that only %s names: its Pods build from them; `pod install` re-points it, then prune again\n",
			len(r.Native), size, strings.Join(r.NativeManifests, ", "))
	}
	if r.Applied {
		fmt.Fprintf(rt.stdout, "deleted %s logical · df %s\n", reclaim.Human(r.Bytes), reclaim.Signed(r.FreeAfter-r.FreeBefore))
		for _, e := range r.Errors {
			fmt.Fprintf(rt.stdout, "  error: %s\n", e)
		}
		return
	}
	fmt.Fprintf(rt.stdout, "total %s logical (hardlinks once; APFS clones share blocks, so df after --apply is the truth)\n", reclaim.Human(r.Bytes))
	if len(r.Unreachable)+len(r.Leftovers) > 0 {
		fmt.Fprintf(rt.stdout, "next: reclaim bun-prune %s --apply\n", r.Checkout)
	}
}

func dryLabel(dry bool) string {
	if dry {
		return "   (dry run — nothing is deleted)"
	}
	return ""
}

type reclaimPrinter struct{ rt *runtime }

func (p *reclaimPrinter) Step(r reclaim.Result) {
	switch r.Status {
	case reclaim.StatusSkipped:
		fmt.Fprintf(p.rt.stdout, "[%d] %-44s  skipped: %s\n", r.Tier, trunc(r.Title, 44), r.Reason)
	case reclaim.StatusFailed:
		fmt.Fprintf(p.rt.stdout, "[%d] %-44s  FAILED  %s\n", r.Tier, trunc(r.Title, 44), firstLine(r.Error))
	default:
		size := reclaim.Human(r.Bytes)
		if r.Bytes == 0 && r.Reason != "" {
			size = "?"
		}
		fmt.Fprintf(p.rt.stdout, "[%d] %-44s  %8s  %5.1fs  free %s\n", r.Tier, trunc(r.Title, 44), size, r.Seconds, reclaim.Human(r.FreeAfter))
	}
}

func (p *reclaimPrinter) TierDone(tier reclaim.Tier, free int64, elapsed time.Duration) {
	fmt.Fprintf(p.rt.stdout, "── tier %d done in %.1fs · free %s\n", tier, elapsed.Seconds(), reclaim.Human(free))
}

func (rt *runtime) reclaimSummary(report reclaim.Report) {
	fmt.Fprintf(rt.stdout, "AFTER   free %s of %s  (%s in %.0fs)\n", reclaim.Human(report.FreeAfter), reclaim.Human(report.Total), reclaim.Signed(report.Freed()), report.Seconds)
	if report.Reached {
		fmt.Fprintf(rt.stdout, "target %s reached — remaining steps skipped\n", reclaim.Human(report.Until))
	}
	if len(report.Notes) > 0 {
		fmt.Fprintln(rt.stdout, "NOT DELETED — by hand:")
		for _, n := range report.Notes {
			fmt.Fprintf(rt.stdout, "  %-32s %8s  %s\n", n.Title, reclaim.Human(n.Bytes), n.Detail)
			if n.Action != "" {
				fmt.Fprintf(rt.stdout, "  %-32s %8s  %s\n", "", "", n.Action)
			}
		}
	}
	fmt.Fprintln(rt.stdout, "note: Docker/OrbStack sparse images return host space ~1 min after a prune — df lags.")
}

func (rt *runtime) reclaimList(plan []reclaim.Step) error {
	if rt.json {
		return writeJSON(rt.stdout, plan)
	}
	for _, s := range plan {
		what := strings.Join(s.Paths, " ")
		switch {
		case what != "":
		case s.Needs != "":
			what = "(" + s.Needs + ")"
		default:
			what = "(built-in)"
		}
		fmt.Fprintf(rt.stdout, "[%d] %-16s %-44s  regenerates: %s\n      %s\n", s.Tier, s.ID, trunc(s.Title, 44), s.Regenerates, what)
	}
	return nil
}

func trunc(s string, n int) string {
	runs := []rune(s)
	if len(runs) <= n {
		return s
	}
	return string(runs[:n-1]) + "…"
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
