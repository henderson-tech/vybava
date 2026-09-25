package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/henderson-tech/vybava/internal/skipci"
	"github.com/spf13/cobra"
)

func (rt *runtime) skipCIApplet() *cobra.Command {
	command := rt.skipCICommand("skipci")
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetOut(rt.stdout)
	command.SetErr(rt.stderr)
	command.PersistentFlags().BoolVar(&rt.json, "json", false, "emit stable JSON output")
	return command
}

func (rt *runtime) skipCICommand(use string) *cobra.Command {
	command := &cobra.Command{
		Use:   use,
		Short: "Hold a repo's pull_request workflows to the skip-ci label guard",
		Long: `Every job of every pull_request-triggered workflow carries one job-level
condition, so a PR labelled ` + "`skip-ci`" + ` runs nothing and its checks read as skipped:

  if: ` + skipci.Guard + `

check reports each job as guarded, missing (no if:), wrap (a single-line if:
the guard can be AND-ed onto) or manual (a multi-line condition a human edits).
apply inserts and wraps by line edits — comments and quoting elsewhere are
untouched — and never rewrites a manual job. The labels themselves are
repolicy's (its default policy carries skip-ci and eve-ignore).`,
		Example: `  skipci check                 # this repo, exit 1 when a job lacks the guard
  skipci apply ~/src/other     # rewrite the missing and wrap jobs there
  skipci check --json          # the per-job report for agents and CI`,
	}

	repoArg := func(args []string) (string, error) {
		dir := "."
		if len(args) > 0 {
			dir = args[0]
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			return "", err
		}
		if info, err := os.Stat(abs); err != nil || !info.IsDir() {
			return "", fmt.Errorf("not a directory: %s", abs)
		}
		return abs, nil
	}
	emit := func(report skipci.Report, err error) error {
		if errors.Is(err, skipci.ErrNoWorkflows) {
			if rt.json {
				return writeJSON(rt.stdout, report)
			}
			fmt.Fprintf(rt.stdout, "SKIPCI  %s  ·  no .github/workflows — nothing to guard\n", report.Repo)
			return nil
		}
		if err != nil {
			return err
		}
		if rt.json {
			if err := writeJSON(rt.stdout, report); err != nil {
				return err
			}
		} else {
			rt.skipCIReport(report)
		}
		if !report.Clean() {
			return ErrFindings
		}
		return nil
	}

	check := &cobra.Command{
		Use:   "check [repo]",
		Short: "Report jobs that lack the guard (exit 1 when any do)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			repo, err := repoArg(args)
			if err != nil {
				return err
			}
			report, err := skipci.Check(repo)
			return emit(report, err)
		},
	}
	apply := &cobra.Command{
		Use:   "apply [repo]",
		Short: "Insert the guard where missing and wrap single-line conditions; manual jobs stay",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			repo, err := repoArg(args)
			if err != nil {
				return err
			}
			report, err := skipci.Apply(repo)
			return emit(report, err)
		},
	}
	command.AddCommand(check, apply)
	return command
}

func (rt *runtime) skipCIReport(report skipci.Report) {
	fmt.Fprintf(rt.stdout, "SKIPCI  %s  ·  guarded %d · missing %d · wrap %d · manual %d · aggregate %d",
		report.Repo, report.Guarded, report.Missing, report.Wrap, report.Manual, report.Aggregate)
	if report.Applied > 0 {
		fmt.Fprintf(rt.stdout, " · applied %d", report.Applied)
	}
	fmt.Fprintln(rt.stdout)
	for _, wf := range report.Workflows {
		if wf.Error != "" {
			fmt.Fprintf(rt.stdout, "\n%s  ✗ unreadable: %s\n", wf.Path, wf.Error)
			continue
		}
		if !wf.PullRequest {
			continue
		}
		fmt.Fprintf(rt.stdout, "\n%s\n", wf.Path)
		for _, job := range wf.Jobs {
			mark := "✓"
			switch job.State {
			case skipci.Missing, skipci.Wrap:
				mark = "✗"
			case skipci.Manual:
				mark = "!"
			case skipci.Aggregate:
				mark = "~"
			}
			note := string(job.State)
			if job.Applied {
				note = "applied"
			}
			fmt.Fprintf(rt.stdout, "  %s %-28s %-8s L%d\n", mark, job.Name, note, job.Line)
		}
	}
	if report.Aggregate > 0 {
		fmt.Fprintf(rt.stdout, "\n%d aggregate job(s) run after skipped needs by design; their STEPS must read the label\n", report.Aggregate)
	}
	if report.Manual > 0 {
		fmt.Fprintf(rt.stdout, "\n%d manual job(s): AND `%s` into the multi-line if: by hand\n", report.Manual, skipci.Guard)
	} else if report.Missing+report.Wrap > 0 {
		fmt.Fprintf(rt.stdout, "\nskipci apply %s converges them\n", report.Repo)
	}
}
