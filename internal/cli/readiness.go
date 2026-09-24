package cli

import (
	"errors"
	"io/fs"
	"os"

	assets "github.com/henderson-tech/vybava"
	"github.com/henderson-tech/vybava/internal/readiness"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/spf13/cobra"
)

func (rt *runtime) readinessApplet() *cobra.Command {
	command := rt.readinessCommand("readiness")
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetOut(rt.stdout)
	command.SetErr(rt.stderr)
	command.PersistentFlags().BoolVar(&rt.json, "json", false, "emit the versioned envelope as JSON")
	return command
}

// readinessCommand wires the release-readiness verbs; the skill of the same
// family (skills/release-readiness) orders them.
func (rt *runtime) readinessCommand(use string) *cobra.Command {
	command := &cobra.Command{
		Use:   use,
		Short: "Release readiness: check the project adapter, freeze ranges, seed and render a run directory",
		Long: "readiness is the deterministic layer of the release-readiness skill. It reads\n" +
			"the `readiness` section of vybava.config.ts, resolves each repo's\n" +
			"production..integration range, seeds a run directory with the skill's\n" +
			"scripts and ledgers, and renders lane rules, lane bodies and agent briefs\n" +
			"from run.json, lanes.json and inventory.json.",
	}
	session := func(cmd *cobra.Command) *runx.Session {
		return &runx.Session{Tool: "readiness", JSON: rt.json, Verb: cmd.Name(), Stdout: rt.stdout, Stderr: rt.stderr}
	}
	finish := func(s *runx.Session, res readiness.Result, err error) error {
		if err == nil {
			err = s.Emit(runx.Envelope{OK: true, Data: res.Data, Diagnostics: res.Diagnostics, Next: res.Next})
		} else if res.Data != nil {
			// A verb that failed mid-flight owes its partial state AND the
			// failure: Finish prints nothing more once an envelope is out.
			diags, next := res.Diagnostics, res.Next
			var de runx.DiagError
			if errors.As(err, &de) {
				diags = append(diags, de.Diag)
				if len(next) == 0 && de.Diag.Fix != "" {
					next = []string{de.Diag.Fix}
				}
			} else {
				diags = append(diags, runx.Diagnostic{Code: runx.DiagInfraError, Severity: "error", Detail: err.Error()})
			}
			_ = s.Emit(runx.Envelope{OK: false, Data: res.Data, Diagnostics: diags, Next: next})
		}
		if code := s.Finish(err); code != 0 {
			return runx.ExitError{Code: code}
		}
		return nil
	}
	open := func() (*readiness.Tool, error) {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		payload, err := fs.Sub(assets.FS, "skills/release-readiness")
		if err != nil {
			return nil, err
		}
		return readiness.Open(cwd, payload)
	}
	run := func(verb func(*readiness.Tool) (readiness.Result, error)) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			t, err := open()
			if err != nil {
				return finish(s, readiness.Result{}, err)
			}
			res, err := verb(t)
			return finish(s, res, err)
		}
	}

	var noFetch bool
	checkCmd := &cobra.Command{
		Use:   "check",
		Short: "Validate the readiness section against the repos and claude-guards' simCap",
		Args:  cobra.NoArgs,
		RunE:  run(func(t *readiness.Tool) (readiness.Result, error) { return t.Check(!noFetch) }),
	}
	checkCmd.Flags().BoolVar(&noFetch, "no-fetch", false, "resolve refs from the last fetched state")

	rangeCmd := &cobra.Command{
		Use:   "range",
		Short: "Resolve production..integration and its commit count for every repo",
		Args:  cobra.NoArgs,
		RunE:  run(func(t *readiness.Tool) (readiness.Result, error) { return t.Range(!noFetch) }),
	}
	rangeCmd.Flags().BoolVar(&noFetch, "no-fetch", false, "resolve refs from the last fetched state")

	var dir, date string
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Create a run directory: run.json with frozen ranges, inventory args, scripts, ledgers",
		Args:  cobra.NoArgs,
		RunE:  run(func(t *readiness.Tool) (readiness.Result, error) { return t.Init(dir, date, !noFetch) }),
	}
	initCmd.Flags().StringVar(&dir, "dir", "", "run directory (default ~/Exports/<exports>/release-readiness-<date>)")
	initCmd.Flags().StringVar(&date, "date", "", "run date, YYYY-MM-DD (default today)")
	initCmd.Flags().BoolVar(&noFetch, "no-fetch", false, "resolve refs from the last fetched state")

	var check bool
	renderCmd := &cobra.Command{
		Use:   "render",
		Short: "Render lane-rules.md, lane bodies and agent briefs from the run's manifests",
		Args:  cobra.NoArgs,
		RunE:  run(func(t *readiness.Tool) (readiness.Result, error) { return t.Render(dir, check) }),
	}
	renderCmd.Flags().StringVar(&dir, "dir", "", "run directory (required)")
	renderCmd.Flags().BoolVar(&check, "check", false, "report drift without writing")
	_ = renderCmd.MarkFlagRequired("dir")

	command.AddCommand(checkCmd, rangeCmd, initCmd, renderCmd)
	return command
}
