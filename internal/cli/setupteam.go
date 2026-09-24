package cli

import (
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/henderson-tech/vybava/internal/catalog"
	"github.com/henderson-tech/vybava/internal/installer"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/toolsetup"
	"github.com/henderson-tech/vybava/internal/ui"
	"github.com/spf13/cobra"
)

// setupTeamCommand is the one-click colleague bootstrap: a catalog group's
// tools through their own channels, its skills and applets through the
// installer. A terminal gets a checklist; --yes/--json run the defaults and
// hand every human-only step back as `next`.
func (rt *runtime) setupTeamCommand() *cobra.Command {
	var group string
	var only, with []string
	var yes, update, dryRun bool
	command := &cobra.Command{
		Use:   "team",
		Short: "Set up a henderson-tech Mac: vault, browser, vitrinka, switcheroo, Pultík and the git family",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			s := &runx.Session{Tool: "vybava", JSON: rt.json, Verb: "setup team", Stdout: rt.stdout, Stderr: rt.stderr}
			fail := func(code, detail, fix string) error {
				return runx.ExitError{Code: s.Finish(runx.DiagError{Diag: runx.Diagnostic{Code: code, Severity: "error", Detail: detail, Fix: fix}})}
			}
			items, err := rt.catalog.Resolve([]string{group})
			if err != nil {
				return fail("SETUP_UNKNOWN_GROUP", err.Error(), "vybava catalog list")
			}
			home, err := os.UserHomeDir()
			if err != nil {
				return runx.ExitError{Code: s.Finish(err)}
			}
			env := toolsetup.DefaultEnv(home, toolsetup.NewHTTPShelf(), toolsetup.ExecRunner, toolsetup.ExecOutput, exec.LookPath)

			selected, unknown := toolsetup.Selection(items, only, with)
			if len(unknown) > 0 {
				return fail("SETUP_UNKNOWN_ITEM", strings.Join(unknown, ", ")+" is not in group "+group, "vybava catalog list")
			}
			interactive := !yes && !rt.json && isTerminal(os.Stdin) && len(only) == 0
			if interactive {
				ids, confirmed, err := ui.Checklist("Výbava — set up this Mac ("+group+")", rt.checklistRows(env, items, selected))
				if err != nil || !confirmed {
					return err
				}
				selected, _ = toolsetup.Selection(items, ids, nil)
			}

			var tools, packages []catalog.Item
			for _, item := range selected {
				if item.Kind == catalog.KindTool {
					tools = append(tools, item)
				} else {
					packages = append(packages, item)
				}
			}
			catalogTools := map[string]catalog.Item{}
			for _, item := range rt.catalog.Items {
				if item.Kind == catalog.KindTool {
					catalogTools[item.ID] = item
				}
			}
			outcomes, err := toolsetup.ApplyAll(env, tools, catalogTools, toolsetup.Options{Update: update, DryRun: dryRun, Interactive: interactive})
			if err != nil {
				return runx.ExitError{Code: s.Finish(err)}
			}
			operations, err := rt.installer.Plan(packages, installer.Options{})
			if err != nil {
				return runx.ExitError{Code: s.Finish(err)}
			}
			if err := rt.installer.Apply(operations, dryRun); err != nil {
				return runx.ExitError{Code: s.Finish(err)}
			}

			var results []toolsetup.Result
			var diagnostics []runx.Diagnostic
			var next []string
			for _, outcome := range outcomes {
				res := outcome.Result
				// A fresh install owes its guided steps (shell wiring, sign-in):
				// run them now for a human, in order, otherwise hand them back.
				owed := res.Setup[:0:0]
				for i, argv := range res.SetupArgv {
					if !interactive || toolsetup.ExecRunner(argv, true) != nil {
						owed = append(owed, res.Setup[i:]...)
						break
					}
				}
				res.Setup = owed
				next = append(next, owed...)
				results = append(results, res)
				if d := outcome.Diag; d != nil {
					severity := "error"
					if d.Code == toolsetup.DiagNeedsHuman {
						severity = "warning"
					}
					diagnostics = append(diagnostics, runx.Diagnostic{Code: d.Code, Severity: severity, Detail: d.Detail, Fix: d.Fix})
					if d.Fix != "" && !slices.Contains(next, d.Fix) {
						next = append(next, d.Fix)
					}
				}
			}
			if gap := toolsetup.PathGap(env, os.Getenv("PATH")); gap != nil {
				diagnostics = append(diagnostics, runx.Diagnostic{Code: gap.Code, Severity: "warning", Detail: gap.Detail, Fix: gap.Fix})
				next = append(next, gap.Fix)
			}
			ok := true
			for _, d := range diagnostics {
				ok = ok && d.Severity != "error"
			}
			if err := s.Emit(runx.Envelope{OK: ok, Data: map[string]any{
				"group": group, "dry_run": dryRun, "tools": results, "packages": operations,
			}, Diagnostics: diagnostics, Next: next}); err != nil {
				return err
			}
			if !ok {
				return runx.ExitError{Code: 2}
			}
			return nil
		},
	}
	command.Flags().StringVar(&group, "group", "henderson", "catalog group to set up")
	command.Flags().StringSliceVar(&only, "only", nil, "apply exactly these items (comma separated)")
	command.Flags().StringSliceVar(&with, "with", nil, "also apply these optional items, e.g. --with devbox")
	command.Flags().BoolVar(&yes, "yes", false, "no checklist: apply the defaults non-interactively")
	command.Flags().BoolVar(&update, "update", false, "move installed tools to their channel's latest")
	command.Flags().BoolVar(&dryRun, "dry-run", false, "report what would change without changing anything")
	return command
}

func (rt *runtime) checklistRows(env toolsetup.Env, items, selected []catalog.Item) []ui.Row {
	installed := map[string]bool{}
	if current, err := rt.installer.Store.Load(); err == nil {
		for _, record := range current.Installed {
			installed[record.ItemID] = true
		}
	}
	checked := map[string]bool{}
	for _, item := range selected {
		checked[item.ID] = true
	}
	rows := make([]ui.Row, 0, len(items))
	for _, item := range items {
		row := ui.Row{ID: item.ID, Kind: string(item.Kind), Description: item.Description, Checked: checked[item.ID]}
		if item.Tool != nil {
			row.Installed, _ = toolsetup.Probe(env, *item.Tool)
			row.Optional = item.Tool.Optional
		} else {
			row.Installed = installed[item.ID]
		}
		rows = append(rows, row)
	}
	return rows
}

func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}
