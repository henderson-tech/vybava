package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/henderson-tech/vybava/internal/repolicy"
	"github.com/spf13/cobra"
)

func (rt *runtime) repolicyApplet() *cobra.Command {
	command := rt.repolicyCommand("repolicy")
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetOut(rt.stdout)
	command.SetErr(rt.stderr)
	command.PersistentFlags().BoolVar(&rt.json, "json", false, "emit stable JSON output")
	return command
}

func (rt *runtime) repolicyCommand(use string) *cobra.Command {
	var (
		policyPath string
		limit      int
		archived   bool
	)
	command := &cobra.Command{
		Use:   use,
		Short: "Hold GitHub repository settings to a declared policy across whole owners",
		Long: `GitHub inherits only the default branch NAME from an organization: settings
like "automatically delete head branches" are per-repository, so every new
repository starts off-policy and nobody notices until merged branches pile up.

repolicy declares the desired state once and converges whole owners to it.
Settings the policy does not name are never read and never written.`,
		Example: `  repolicy audit henderson-tech LEFTEQ      # report drift, exit 1 if any
  repolicy apply henderson-tech             # converge the drifting repositories
  repolicy audit --policy repos.yaml        # owners and settings from a file
  repolicy audit henderson-tech --json      # stable output for agents and CI`,
	}

	load := func() (repolicy.Policy, error) {
		if policyPath == "" {
			return repolicy.DefaultPolicy(), nil
		}
		return repolicy.LoadPolicy(policyPath)
	}
	options := func() repolicy.Options { return repolicy.Options{Limit: limit, IncludeArchived: archived} }

	audit := &cobra.Command{
		Use:   "audit [owner...]",
		Short: "Report repositories that drift from the policy (exit 1 when any do)",
		RunE: func(_ *cobra.Command, owners []string) error {
			policy, err := load()
			if err != nil {
				return err
			}
			report, err := repolicy.Audit(repolicy.ExecRunner{}, policy, owners, options())
			if err != nil {
				return err
			}
			if rt.json {
				if err := writeJSON(rt.stdout, report); err != nil {
					return err
				}
			} else {
				rt.repolicyReport(report)
			}
			if len(report.Drift) > 0 {
				return ErrFindings
			}
			return nil
		},
	}

	apply := &cobra.Command{
		Use:   "apply [owner...]",
		Short: "Converge every drifting repository to the policy",
		RunE: func(_ *cobra.Command, owners []string) error {
			policy, err := load()
			if err != nil {
				return err
			}
			report, err := repolicy.Apply(repolicy.ExecRunner{}, policy, owners, options())
			if err != nil {
				return err
			}
			if rt.json {
				if err := writeJSON(rt.stdout, report); err != nil {
					return err
				}
			} else {
				rt.repolicyReport(report)
			}
			if failures := report.Failures(); len(failures) > 0 {
				return fmt.Errorf("%d repositories refused the change (admin rights?): %s",
					len(failures), strings.Join(repolicyNames(failures), ", "))
			}
			return nil
		},
	}

	for _, sub := range []*cobra.Command{audit, apply} {
		sub.Flags().StringVar(&policyPath, "policy", "", "YAML policy file (default: deleteBranchOnMerge: true)")
		sub.Flags().IntVar(&limit, "limit", 500, "repositories to read per owner")
		sub.Flags().BoolVar(&archived, "include-archived", false, "also read archived repositories (they refuse writes)")
	}
	command.AddCommand(audit, apply)
	return command
}

func (rt *runtime) repolicyReport(report repolicy.Report) {
	fmt.Fprintf(rt.stdout, "REPOLICY  %s  ·  %d repositories  ·  %s%s\n",
		strings.Join(report.Owners, ", "), report.Checked, repolicySettings(report.Settings), repolicyLabels(report.Labels))
	if len(report.Skipped) > 0 {
		fmt.Fprintf(rt.stdout, "excluded: %s\n", strings.Join(report.Skipped, ", "))
	}

	if len(report.Drift) == 0 {
		fmt.Fprintln(rt.stdout, "\non policy — nothing to converge")
	} else {
		fmt.Fprintf(rt.stdout, "\nDRIFT (%d)\n", len(report.Drift))
		for _, drift := range report.Drift {
			fmt.Fprintf(rt.stdout, "  %-44s %-22s %t → %t\n", drift.Repo, drift.Setting, drift.Got, drift.Want)
		}
	}

	if len(report.Applied) > 0 {
		fmt.Fprintf(rt.stdout, "\nAPPLIED (%d)\n", len(report.Applied))
		for _, change := range report.Applied {
			mark := "✓"
			detail := strings.Join(change.Settings, ", ")
			if !change.OK {
				mark, detail = "✗", change.Error
			}
			fmt.Fprintf(rt.stdout, "  %s %-44s %s\n", mark, change.Repo, detail)
		}
	} else if len(report.Drift) > 0 {
		fmt.Fprintf(rt.stdout, "\n%s converges them\n", strings.TrimSpace(repolicyApplyHint(report)))
	}

	for _, warning := range report.Warnings {
		fmt.Fprintln(rt.stderr, "note:", warning)
	}
}

// repolicyLabels names the required labels in the header, after the settings.
func repolicyLabels(labels []repolicy.Label) string {
	if len(labels) == 0 {
		return ""
	}
	names := make([]string, 0, len(labels))
	for _, l := range labels {
		names = append(names, l.Name)
	}
	return "  ·  labels " + strings.Join(names, ", ")
}

func repolicyApplyHint(report repolicy.Report) string {
	return "repolicy apply " + strings.Join(report.Owners, " ")
}

func repolicySettings(settings map[string]bool) string {
	keys := make([]string, 0, len(settings))
	for key := range settings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, key := range keys {
		pairs = append(pairs, fmt.Sprintf("%s=%t", key, settings[key]))
	}
	return strings.Join(pairs, " ")
}

func repolicyNames(changes []repolicy.Change) []string {
	out := make([]string, 0, len(changes))
	for _, change := range changes {
		out = append(out, change.Repo)
	}
	return out
}
