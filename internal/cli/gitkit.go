package cli

import (
	"github.com/henderson-tech/vybava/internal/gitkit"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/spf13/cobra"
)

func (rt *runtime) gitkitApplet() *cobra.Command {
	command := rt.gitkitCommand("gitkit")
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetOut(rt.stdout)
	command.SetErr(rt.stderr)
	command.PersistentFlags().BoolVar(&rt.json, "json", false, "emit the versioned envelope as JSON")
	return command
}

// gitkitCommand exposes each verb as `gitkit <script> [args]`. Script verbs
// never parse flags — every argument belongs to the verb, which owns its
// stdout, stderr and exit code exactly as the TypeScript scripts it
// replaced did.
func (rt *runtime) gitkitCommand(use string) *cobra.Command {
	command := &cobra.Command{
		Use:   use,
		Short: "Deterministic git/PR layer behind the prm, push-all and sync skills",
		Long: "gitkit runs the git family's deterministic scripts (PR selectors, review\n" +
			"triage, merge preconditions, worktrees, path classification) in-process.\n" +
			"`gitkit doctor` lists them.",
	}
	session := func(cmd *cobra.Command) *runx.Session {
		return &runx.Session{Tool: "gitkit", JSON: rt.json, Verb: cmd.Name(), Stdout: rt.stdout, Stderr: rt.stderr}
	}
	finish := func(s *runx.Session, err error) error {
		if code := s.Finish(err); code != 0 {
			return runx.ExitError{Code: code}
		}
		return nil
	}
	command.RunE = func(cmd *cobra.Command, args []string) error {
		s := session(cmd)
		if len(args) > 0 {
			return finish(s, runx.DiagError{Diag: runx.Diagnostic{
				Code: gitkit.DiagUnknownScript, Severity: "error",
				Detail: "unknown gitkit script " + args[0], Fix: "vybava gitkit --json",
			}})
		}
		return finish(s, s.Emit(runx.Envelope{OK: true, Data: map[string]any{"scripts": gitkit.Scripts()}, Next: []string{"vybava gitkit doctor"}}))
	}
	command.Args = cobra.ArbitraryArgs

	command.AddCommand(&cobra.Command{
		Use:   "doctor",
		Short: "List the gitkit verbs (all run in-process; no runtime to check)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			return finish(s, s.Emit(runx.Envelope{OK: true, Data: map[string]any{"scripts": gitkit.Scripts()}}))
		},
	})
	for _, script := range gitkit.Scripts() {
		command.AddCommand(&cobra.Command{
			Use:                script + " [args...]",
			Short:              "Run the " + script + " verb",
			DisableFlagParsing: true,
			RunE: func(cmd *cobra.Command, args []string) error {
				verb, _ := gitkit.Native(script)
				if code := verb(args, rt.stdout, rt.stderr); code != 0 {
					return runx.ExitError{Code: code}
				}
				return nil
			},
		})
	}
	return command
}
