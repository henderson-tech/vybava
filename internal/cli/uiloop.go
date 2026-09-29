package cli

import (
	"context"
	"errors"
	"os"

	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/uiloop"
	"github.com/spf13/cobra"
)

func (rt *runtime) uiLoopApplet() *cobra.Command {
	command := rt.uiLoopCommand("ui-loop")
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetOut(rt.stdout)
	command.SetErr(rt.stderr)
	command.PersistentFlags().BoolVar(&rt.json, "json", false, "emit the versioned envelope as JSON")
	return command
}

// uiLoopCommand wires the ui-loop verbs; the vitrinka map / review-loop
// workflows order them (docs/uiloop.md).
func (rt *runtime) uiLoopCommand(use string) *cobra.Command {
	command := &cobra.Command{
		Use:   use,
		Short: "UI polish loop: sync the capture harness, run passes, split-publish to vitrinka, scoreboard",
		Long: "ui-loop is the deterministic layer of the UI polish loop. It reads the\n" +
			"`uiLoop` section of vybava.config.ts, syncs the embedded TypeScript/Playwright\n" +
			"harness into <dir>/vendor, writes each pass's run.json and runs the repo's\n" +
			"Playwright against it, and turns a pass directory into vitrinka sets and a\n" +
			"scoreboard. Command output streams to stderr; stdout carries the envelope.",
	}
	session := func(cmd *cobra.Command) *runx.Session {
		return &runx.Session{Tool: "ui-loop", JSON: rt.json, Verb: cmd.Name(), Stdout: rt.stdout, Stderr: rt.stderr}
	}
	finish := func(s *runx.Session, res uiloop.Result, err error) error {
		failedDiag := false
		for _, d := range res.Diagnostics {
			failedDiag = failedDiag || d.Severity == "error"
		}
		if err == nil {
			err = s.Emit(runx.Envelope{OK: !failedDiag, Data: res.Data, Diagnostics: res.Diagnostics, Next: res.Next})
			if err == nil && failedDiag {
				err = runx.ExitError{Code: 2}
			}
		} else if res.Data != nil {
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
	run := func(verb func(*uiloop.Tool) (uiloop.Result, error)) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			cwd, err := os.Getwd()
			if err != nil {
				return finish(s, uiloop.Result{}, err)
			}
			t, err := uiloop.Open(cwd, rt.version, rt.stderr)
			if err != nil {
				return finish(s, uiloop.Result{}, err)
			}
			res, err := verb(t)
			return finish(s, res, err)
		}
	}

	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Scaffold <dir> (project.ts, screens/), the spec template and the out gitignore; sync the harness",
		Args:  cobra.NoArgs,
		RunE:  run(func(t *uiloop.Tool) (uiloop.Result, error) { return t.Init() }),
	}

	var force bool
	syncCmd := &cobra.Command{
		Use:   "sync",
		Short: "Write the embedded harness into <dir>/vendor with a stamp (version + sha256 per file)",
		Args:  cobra.NoArgs,
		RunE:  run(func(t *uiloop.Tool) (uiloop.Result, error) { return t.Sync(force) }),
	}
	syncCmd.Flags().BoolVar(&force, "force", false, "overwrite vendored files that were edited in the repo")

	var noTS bool
	checkCmd := &cobra.Command{
		Use:   "check",
		Short: "Report vendor drift, config validity, manifest problems and a stale app map — each separately",
		Args:  cobra.NoArgs,
		RunE:  run(func(t *uiloop.Tool) (uiloop.Result, error) { return t.Check(noTS) }),
	}
	checkCmd.Flags().BoolVar(&noTS, "no-ts", false, "skip the manifest and app-map parts (they run the repo's tsRunner)")

	mapCmd := &cobra.Command{
		Use:   "map",
		Short: "Render the app map (uiLoop.appMap) from the screen manifest",
		Args:  cobra.NoArgs,
		RunE:  run(func(t *uiloop.Tool) (uiloop.Result, error) { return t.Map() }),
	}

	var (
		opts                          uiloop.RunOptions
		apps, only, viewports, themes string
		areas, sets, backlog          string
		pass, previous, retries       int
		dryRun, forcePublish, noDelta bool
	)
	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Write <out>/pass-<n>/run.json and run (or --print) the repo's Playwright capture",
		Long: "run resolves the selection and the pass, writes run.json and runs the\n" +
			"capture from the repo root. The command needs no vybava: print it with\n" +
			"--print and run it where the app is reachable (a Devbox container), or\n" +
			"let --wrap run it there (--wrap \"devbox run -- {cmd}\").",
		Args: cobra.NoArgs,
		RunE: run(func(t *uiloop.Tool) (uiloop.Result, error) {
			opts.Selection.Apps = uiloop.SplitList(apps)
			opts.Selection.Only = uiloop.SplitList(only)
			opts.Selection.Viewports = uiloop.SplitList(viewports)
			opts.Selection.Themes = uiloop.SplitList(themes)
			opts.Pass = pass
			return t.Run(context.Background(), opts)
		}),
	}
	runCmd.Flags().StringVar(&apps, "app", "", "only these apps (comma-separated)")
	runCmd.Flags().StringVar(&only, "only", "", "only these screen ids or areas (comma-separated; a trailing * is a prefix)")
	runCmd.Flags().StringVar(&viewports, "viewports", "", "only these viewports (comma-separated)")
	runCmd.Flags().StringVar(&themes, "themes", "", "only these themes: light,dark")
	runCmd.Flags().BoolVar(&opts.Selection.Destructive, "destructive", false, "also shoot destructive recipes (last)")
	runCmd.Flags().BoolVar(&opts.Selection.Resume, "resume", false, "finish a cut-short pass: keep ok/unreachable shots, retake the rest")
	runCmd.Flags().IntVar(&pass, "pass", 0, "pass number (default: the next one; with --resume the latest)")
	runCmd.Flags().IntVar(&opts.Workers, "workers", 2, "Playwright workers")
	runCmd.Flags().IntVar(&opts.BuildWait, "build-wait", 300, "seconds a shot waits for a red dev server to turn green")
	runCmd.Flags().BoolVar(&opts.Print, "print", false, "write run.json and print the command without running it")
	runCmd.Flags().StringVar(&opts.Wrap, "wrap", "", `run the command through another; {cmd} is the quoted capture command (e.g. "devbox run -- {cmd}")`)

	splitCmd := &cobra.Command{
		Use:   "split",
		Short: "Plan a pass's vitrinka sets: area × viewport × theme, ≤ publish.maxFiles and ≤ publish.maxBytes each",
		Args:  cobra.NoArgs,
		RunE: run(func(t *uiloop.Tool) (uiloop.Result, error) {
			return t.Split(uiloop.SplitOptions{Pass: pass, Areas: uiloop.SplitList(areas)})
		}),
	}
	splitCmd.Flags().IntVar(&pass, "pass", 0, "pass number (default: the latest)")
	splitCmd.Flags().StringVar(&areas, "areas", "", "only these areas (comma-separated)")

	publishCmd := &cobra.Command{
		Use:   "publish",
		Short: "Adopt and push each planned set with the vitrinka CLI (retry, then halve)",
		Args:  cobra.NoArgs,
		RunE: run(func(t *uiloop.Tool) (uiloop.Result, error) {
			return t.Publish(context.Background(), uiloop.PublishOptions{
				Pass: pass, Areas: uiloop.SplitList(areas), Sets: uiloop.SplitList(sets),
				Retries: retries, Force: forcePublish, DryRun: dryRun,
			})
		}),
	}
	publishCmd.Flags().IntVar(&pass, "pass", 0, "pass number (default: the latest)")
	publishCmd.Flags().StringVar(&areas, "areas", "", "only these areas (comma-separated)")
	publishCmd.Flags().StringVar(&sets, "sets", "", "only these set keys (comma-separated)")
	publishCmd.Flags().IntVar(&retries, "retries", 3, "push attempts per set before it is halved")
	publishCmd.Flags().BoolVar(&forcePublish, "force", false, "re-push sets the index records as pushed")
	publishCmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the vitrinka commands without running them")

	scoreboardCmd := &cobra.Command{
		Use:   "scoreboard",
		Short: "Fold a pass's records and review backlog into scoreboard.json + scoreboard.md, with a delta",
		Args:  cobra.NoArgs,
		RunE: run(func(t *uiloop.Tool) (uiloop.Result, error) {
			prev := previous
			if noDelta {
				prev = -1
			}
			return t.Scoreboard(uiloop.ScoreboardOptions{Pass: pass, Backlog: backlog, Previous: prev})
		}),
	}
	scoreboardCmd.Flags().IntVar(&pass, "pass", 0, "pass number (default: the latest)")
	scoreboardCmd.Flags().StringVar(&backlog, "backlog", "", "review backlog JSON (default: <passDir>/review/backlog.json when present)")
	scoreboardCmd.Flags().IntVar(&previous, "previous", 0, "pass to compute the delta against (default: the one before)")
	scoreboardCmd.Flags().BoolVar(&noDelta, "no-delta", false, "skip the delta")

	command.AddCommand(initCmd, syncCmd, checkCmd, mapCmd, runCmd, splitCmd, publishCmd, scoreboardCmd)
	return command
}
