package cli

import (
	"errors"
	"os"
	"os/exec"
	"syscall"

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

// gitkitCommand exposes each embedded script as `gitkit <script> [args]`.
// Script verbs never parse flags — every argument belongs to the script.
// A verb ported to Go runs in-process; the rest replace this process with
// node, so a Monitor's signals and the script's exit code reach the caller
// unchanged.
func (rt *runtime) gitkitCommand(use string) *cobra.Command {
	command := &cobra.Command{
		Use:   use,
		Short: "Deterministic git/PR layer behind the prm, push-all and sync skills",
		Long: "gitkit runs the git family's deterministic scripts (PR selectors, review\n" +
			"triage, merge preconditions, worktrees, path classification) on the\n" +
			"local Node runtime. `gitkit doctor` checks that runtime.",
	}
	session := func(cmd *cobra.Command) *runx.Session {
		return &runx.Session{Tool: "gitkit", JSON: rt.json, Verb: cmd.Name(), Stdout: rt.stdout, Stderr: rt.stderr}
	}
	finish := func(s *runx.Session, err error) error {
		var nodeErr gitkit.NodeError
		if errors.As(err, &nodeErr) {
			err = runx.DiagError{Diag: runx.Diagnostic{Code: nodeErr.Code, Severity: "error", Detail: nodeErr.Detail, Fix: nodeErr.Fix}}
		}
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
		Short: "Check the Node runtime and materialize the script payload",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			node, err := gitkit.ResolveNode(exec.LookPath, gitkit.NodeVersion)
			if err != nil {
				return finish(s, err)
			}
			bin, err := rt.gitkitBin()
			if err != nil {
				return finish(s, err)
			}
			return finish(s, s.Emit(runx.Envelope{OK: true, Data: map[string]any{
				"node": node, "payload": bin, "digest": gitkit.Digest(), "scripts": gitkit.Scripts(),
			}}))
		},
	})
	for _, script := range gitkit.Scripts() {
		command.AddCommand(&cobra.Command{
			Use:                script + " [args...]",
			Short:              "Run " + script + ".ts",
			DisableFlagParsing: true,
			RunE: func(cmd *cobra.Command, args []string) error {
				if verb, ok := gitkit.Native(script); ok {
					if code := verb(args, rt.stdout, rt.stderr); code != 0 {
						return runx.ExitError{Code: code}
					}
					return nil
				}
				node, err := gitkit.ResolveNode(exec.LookPath, gitkit.NodeVersion)
				if err != nil {
					return finish(session(cmd), err)
				}
				bin, err := rt.gitkitBin()
				if err != nil {
					return finish(session(cmd), err)
				}
				return syscall.Exec(node.Path, gitkit.Argv(node, bin, script, args), os.Environ())
			},
		})
	}
	return command
}

func (rt *runtime) gitkitBin() (string, error) {
	root, err := gitkit.DefaultCacheRoot()
	if err != nil {
		return "", err
	}
	return gitkit.Materialize(root)
}
