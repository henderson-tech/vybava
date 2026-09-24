package cli

import (
	"errors"

	"github.com/henderson-tech/vybava/internal/mergeassist"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/spf13/cobra"
)

func (rt *runtime) mergeAssistApplet() *cobra.Command {
	cmd := rt.mergeAssistCommand("merge-assist")
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(rt.stdout)
	cmd.SetErr(rt.stderr)
	cmd.PersistentFlags().BoolVar(&rt.json, "json", false, "emit machine-readable output")
	return cmd
}

// mergeAssistCommand wires the merge verbs: one envelope under --json, the
// triage table otherwise, `next` naming the follow-up command.
func (rt *runtime) mergeAssistCommand(use string) *cobra.Command {
	root := &cobra.Command{
		Use:   use,
		Short: "Merge main without the mechanical conflicts — catalogs, generated files and migration timestamps settle themselves",
		Long: "Configure `merge` (generated paths + regen, migration dirs) in vybava.config.ts;\n" +
			"catalogs come from `lok`. Then:\n" +
			"  merge-assist setup           register the drivers in this clone (merge does it too)\n" +
			"  merge-assist merge [ref]     merge (default origin's default branch), print what is left\n" +
			"  merge-assist merge --dry-run preview with git merge-tree, touch nothing\n" +
			"  merge-assist status · regen · migrations [--apply]",
	}
	session := func(cmd *cobra.Command) *runx.Session {
		return &runx.Session{Tool: "merge-assist", JSON: rt.json, Verb: cmd.Name(), Stdout: rt.stdout, Stderr: rt.stderr}
	}
	finish := func(s *runx.Session, data any, next []string, err error) error {
		env := runx.Envelope{OK: err == nil, Verb: s.Verb, Data: data, Diagnostics: []runx.Diagnostic{}, Next: next}
		if env.Next == nil {
			env.Next = []string{}
		}
		var d *mergeassist.Diag
		if errors.As(err, &d) {
			env.Diagnostics = append(env.Diagnostics, runx.Diagnostic{Code: d.Code, Severity: "error", Detail: d.Detail, Fix: d.Fix})
			if d.Fix != "" {
				env.Next = append(env.Next, d.Fix)
			}
			err = &runx.DiagError{Diag: env.Diagnostics[0]}
		} else if err != nil {
			env.Diagnostics = append(env.Diagnostics, runx.Diagnostic{Code: runx.DiagInfraError, Severity: "error", Detail: err.Error()})
		}
		if emitErr := s.Emit(env); emitErr != nil {
			return emitErr
		}
		if code := s.Finish(err); code != 0 {
			return runx.ExitError{Code: code}
		}
		return nil
	}
	// report prints the table in text mode (the envelope then carries only
	// diagnostics and next) and the whole report under --json.
	report := func(s *runx.Session, rep mergeassist.Report, err error) error {
		var next []string
		if err == nil {
			next = reportNext(rep)
			if rep.Open > 0 {
				err = &mergeassist.Diag{Code: mergeassist.DiagOpen, Detail: "resolve the open rows (catalog clashes: lok merge <path> --prefer ours|theirs), git add them"}
			}
		}
		if rt.json {
			return finish(s, rep, next, err)
		}
		if len(rep.Rows) > 0 || rep.UpToDate || err == nil {
			if werr := rep.WriteTable(rt.stdout); werr != nil {
				return werr
			}
		}
		return finish(s, nil, next, err)
	}
	open := func() (*mergeassist.Tool, error) { return mergeassist.Open(workingDir()) }

	var opts mergeassist.MergeOptions
	merge := &cobra.Command{
		Use: "merge [ref]", Short: "Merge ref (default origin's default branch) with the drivers active; print what is left", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := session(cmd)
			t, err := open()
			if err != nil {
				return finish(s, nil, nil, err)
			}
			if len(args) == 1 {
				opts.Onto = args[0]
			}
			rep, err := t.Merge(opts)
			return report(s, rep, err)
		},
	}
	merge.Flags().BoolVar(&opts.DryRun, "dry-run", false, "preview with git merge-tree; touch nothing")
	merge.Flags().BoolVar(&opts.NoFetch, "no-fetch", false, "do not fetch the remote first")
	merge.Flags().BoolVar(&opts.NoCommit, "no-commit", false, "leave a clean merge uncommitted")
	root.AddCommand(merge)

	root.AddCommand(&cobra.Command{
		Use: "status", Short: "The triage table for the merge in progress", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			t, err := open()
			if err != nil {
				return finish(s, nil, nil, err)
			}
			rep, err := t.Status()
			return report(s, rep, err)
		},
	})

	root.AddCommand(&cobra.Command{
		Use: "regen", Short: "Run the regen commands the drivers queued, stage their output", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			t, err := open()
			if err != nil {
				return finish(s, nil, nil, err)
			}
			runs, err := t.Regen()
			if err == nil {
				for _, r := range runs {
					if !r.OK {
						err = &mergeassist.Diag{Code: mergeassist.DiagRegenFailed, Detail: r.Cmd + ": " + r.Output, Fix: "fix the cause, then merge-assist regen"}
						break
					}
				}
			}
			return finish(s, map[string]any{"runs": runs}, []string{"merge-assist status"}, err)
		},
	})

	var check bool
	setup := &cobra.Command{
		Use: "setup", Short: "Render the attribute block into the clone's info/attributes and register the git merge drivers (local, nothing tracked)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			t, err := open()
			if err != nil {
				return finish(s, nil, nil, err)
			}
			res, err := t.Setup(check)
			return finish(s, res, nil, err)
		},
	}
	setup.Flags().BoolVar(&check, "check", false, "report drift, change nothing (CI gate)")
	root.AddCommand(setup)

	var onto string
	var apply bool
	migrations := &cobra.Command{
		Use: "migrations", Short: "Renumber unmerged migrations the base overtook (plan; --apply renames and stages)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			t, err := open()
			if err != nil {
				return finish(s, nil, nil, err)
			}
			if onto == "" {
				onto = t.DefaultRef()
			}
			plans, err := t.PlanMigrations(onto, "")
			if err == nil && apply {
				plans, err = t.ApplyMigrations(plans)
			}
			if err == nil {
				for _, p := range plans {
					if p.CheckOK != nil && !*p.CheckOK {
						err = &mergeassist.Diag{Code: mergeassist.DiagCheckFailed, Detail: p.Check + ": " + p.Output}
						break
					}
				}
			}
			var next []string
			if err == nil && !apply {
				for _, p := range plans {
					if len(p.Renames) > 0 {
						next = []string{"merge-assist migrations --apply"}
						break
					}
				}
			}
			return finish(s, map[string]any{"plans": plans}, next, err)
		},
	}
	migrations.Flags().StringVar(&onto, "onto", "", "base ref (default origin's default branch)")
	migrations.Flags().BoolVar(&apply, "apply", false, "rename, rewrite and stage")
	root.AddCommand(migrations)

	root.AddCommand(&cobra.Command{
		Use: "driver <base> <ours> <theirs> <path>", Short: "Git merge driver for generated paths: take theirs, queue the regen (registered by setup)", Args: cobra.ExactArgs(4),
		RunE: func(_ *cobra.Command, args []string) error {
			conflict, err := mergeassist.Driver(workingDir(), args[0], args[1], args[2], args[3], rt.stderr)
			if err != nil {
				return err
			}
			if conflict {
				return runx.ExitError{Code: 1}
			}
			return nil
		},
	})
	return root
}

// reportNext names what the session does after reading the table.
func reportNext(rep mergeassist.Report) []string {
	var next []string
	for _, row := range rep.Rows {
		if row.Class == "regen" && row.State != mergeassist.StateAuto {
			next = append(next, "merge-assist regen")
			break
		}
	}
	for _, row := range rep.Rows {
		if row.Class == "migration" && row.State == mergeassist.StateFailed {
			next = append(next, "merge-assist migrations --apply  # reruns the failed check once fixed")
			break
		}
	}
	switch {
	case rep.DryRun && !rep.UpToDate:
		next = append(next, "merge-assist merge "+rep.Onto)
	case rep.Open > 0:
		next = append(next, "merge-assist status")
	case rep.Committed == "" && !rep.UpToDate && !rep.DryRun:
		next = append(next, "git commit --no-edit")
	}
	return next
}
