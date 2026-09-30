package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/henderson-tech/vybava/internal/polishkit"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/spf13/cobra"
)

func (rt *runtime) polishKitApplet() *cobra.Command {
	command := rt.polishKitCommand("polish-kit")
	command.SetOut(rt.stdout)
	command.SetErr(rt.stderr)
	return command
}

const polishKitUsage = `polish-kit plan     [--base <ref>] [--target app,ui] [--intensity quick|default|full] [--findings <ref>]
polish-kit lanes    [--target app] [--lane <id>]
polish-kit lanes set <lane> [--theme light|dark] [--nav gesture|3button] [--text <size>] [--reset]
polish-kit run init [--pass N] [--target app] [--lanes a,b] [--screens x,y] [--intensity …] [--base <ref>] [--force]
polish-kit run add-cell --kind matrix --lane <id> --flow "<title>" --tier "<tier>" [--pass N]
polish-kit cell <id> pass|fail|skip [--shot <path>] [--note <text>] [--finding <ref>] [--pass N]
polish-kit status   [--pass N]
polish-kit shoot <lane> [--pass N] [--screens x,y] [--themes light,dark] [--nav …] [--text …]
polish-kit sheet    [--pass N] [--screen <id>] [--lanes a,b]
polish-kit report   [--pass N] [--previous N | --no-delta]
Every verb takes --json.`

// polishKitCommand is the outer shell: it owns no flags (so a verb's flags
// reach it intact) and turns a cobra parse error into a `usage` diagnostic
// with the corrected invocation, never a usage dump. Domain logic lives in
// internal/polishkit.
func (rt *runtime) polishKitCommand(use string) *cobra.Command {
	command := &cobra.Command{
		Use:                use,
		Short:              "Polish skill's deterministic layer: target inference, device lanes, the pass ledger, native shots, contact sheets, report",
		Long:               polishKitUsage,
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, a := range args {
				if a == "-h" || a == "--help" {
					_, err := fmt.Fprintln(rt.stdout, polishKitUsage)
					return err
				}
			}
			if len(args) == 0 {
				_, err := fmt.Fprintln(rt.stdout, polishKitUsage)
				return err
			}
			inner := rt.polishKitVerbs()
			inner.SetArgs(args)
			inner.SetOut(rt.stdout)
			inner.SetErr(rt.stderr)
			err := inner.Execute()
			var exit runx.ExitError
			if err == nil || errors.As(err, &exit) {
				return err
			}
			verb := ""
			for _, a := range args {
				if !strings.HasPrefix(a, "-") {
					verb = a
					break
				}
			}
			s := &runx.Session{Tool: "polish-kit", JSON: rt.json || hasJSONFlag(args), Verb: verb, Stdout: rt.stdout, Stderr: rt.stderr}
			derr := runx.DiagError{Diag: runx.Diagnostic{Code: polishkit.DiagUsage, Severity: "error", Detail: err.Error(), Fix: "polish-kit --help"}}
			if code := s.Finish(derr); code != 0 {
				return runx.ExitError{Code: code}
			}
			return nil
		},
	}
	return command
}

func (rt *runtime) polishKitVerbs() *cobra.Command {
	root := &cobra.Command{Use: "polish-kit", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().BoolVar(&rt.json, "json", rt.json, "emit the versioned envelope as JSON")
	session := func(cmd *cobra.Command) *runx.Session {
		return &runx.Session{Tool: "polish-kit", JSON: rt.json, Verb: cmd.Name(), Stdout: rt.stdout, Stderr: rt.stderr}
	}
	finish := func(s *runx.Session, res polishkit.Result, err error) error {
		failedDiag := false
		for _, d := range res.Diagnostics {
			failedDiag = failedDiag || d.Severity == "error"
		}
		if err == nil {
			if rt.json {
				err = s.Emit(runx.Envelope{OK: !failedDiag, Data: res.Data, Diagnostics: res.Diagnostics, Next: res.Next})
			} else {
				for _, l := range res.Lines {
					fmt.Fprintln(rt.stdout, l)
				}
				err = s.Emit(runx.Envelope{OK: !failedDiag, Diagnostics: res.Diagnostics, Next: res.Next})
			}
			if err == nil && failedDiag {
				err = runx.ExitError{Code: 2}
			}
		} else if res.Data != nil {
			// A verb that failed with partial state still owes it, plus the
			// diagnostic it failed with and its fix as next.
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
			env := runx.Envelope{OK: false, Diagnostics: diags, Next: next}
			if rt.json {
				env.Data = res.Data
			} else {
				for _, l := range res.Lines {
					fmt.Fprintln(rt.stdout, l)
				}
			}
			_ = s.Emit(env)
		}
		if code := s.Finish(err); code != 0 {
			return runx.ExitError{Code: code}
		}
		return nil
	}
	run := func(verb func(*polishkit.Tool, []string) (polishkit.Result, error)) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			s := session(cmd)
			cwd, err := os.Getwd()
			if err != nil {
				return finish(s, polishkit.Result{}, err)
			}
			t, err := polishkit.Open(cwd, rt.version, rt.stderr)
			if err != nil {
				return finish(s, polishkit.Result{}, err)
			}
			res, err := verb(t, args)
			return finish(s, res, err)
		}
	}
	ctx := context.Background()

	var plan polishkit.PlanOptions
	var planTargets string
	planCmd := &cobra.Command{
		Use: "plan", Short: "Infer targets from the diff, the cwd and --target; list lanes and touched screens", Args: cobra.NoArgs,
		RunE: run(func(t *polishkit.Tool, _ []string) (polishkit.Result, error) {
			plan.Targets = polishkit.SplitList(planTargets)
			return t.Plan(ctx, plan)
		}),
	}
	planCmd.Flags().StringVar(&plan.Base, "base", "", "diff base ref (default: the config's, origin/main)")
	planCmd.Flags().StringVar(&planTargets, "target", "", "targets to include first (comma-separated: app,ui,api)")
	planCmd.Flags().StringVar(&plan.Intensity, "intensity", "", "quick | default | full")
	planCmd.Flags().StringVar(&plan.Findings, "findings", "", "vitrinka board slug, PR URL or file the pass works from (recorded, not read)")

	var lanes polishkit.LanesOptions
	var lanesTargets string
	lanesCmd := &cobra.Command{
		Use: "lanes", Short: "Resolve every declared lane to a live device now", Args: cobra.NoArgs,
		RunE: run(func(t *polishkit.Tool, _ []string) (polishkit.Result, error) {
			lanes.Targets = polishkit.SplitList(lanesTargets)
			return t.Lanes(ctx, lanes)
		}),
	}
	lanesCmd.Flags().StringVar(&lanesTargets, "target", "", "only lanes of these targets (comma-separated)")
	lanesCmd.Flags().StringVar(&lanes.Lane, "lane", "", "only this lane")

	var set polishkit.SetOptions
	setCmd := &cobra.Command{
		Use: "set <lane>", Short: "Apply device state: theme, nav mode, text size, or --reset", Args: cobra.ExactArgs(1),
		RunE: run(func(t *polishkit.Tool, args []string) (polishkit.Result, error) {
			set.Lane = args[0]
			return t.Set(ctx, set)
		}),
	}
	setCmd.Flags().StringVar(&set.Theme, "theme", "", "light | dark")
	setCmd.Flags().StringVar(&set.Nav, "nav", "", "gesture | 3button (android)")
	setCmd.Flags().StringVar(&set.Text, "text", "", "iOS content size name or Android font_scale")
	setCmd.Flags().BoolVar(&set.Reset, "reset", false, "restore light, gesture and the default text size")
	lanesCmd.AddCommand(setCmd)

	runCmd := &cobra.Command{Use: "run", Short: "The pass ledger: init a pass, add matrix cells"}
	var initOpts polishkit.InitOptions
	var initTargets, initLanes, initScreens string
	initCmd := &cobra.Command{
		Use: "init", Short: "Write <out>/pass-<n>/run.json with the plan snapshot and the chrome cell table (idempotent; --force recreates)", Args: cobra.NoArgs,
		RunE: run(func(t *polishkit.Tool, _ []string) (polishkit.Result, error) {
			initOpts.Plan.Targets = polishkit.SplitList(initTargets)
			initOpts.Lanes = polishkit.SplitList(initLanes)
			initOpts.Screens = polishkit.SplitList(initScreens)
			return t.Init(ctx, initOpts)
		}),
	}
	initCmd.Flags().IntVar(&initOpts.Pass, "pass", 0, "pass number (default: the next one)")
	initCmd.Flags().StringVar(&initTargets, "target", "", "targets to include first (comma-separated)")
	initCmd.Flags().StringVar(&initLanes, "lanes", "", "only these lanes (comma-separated; default: the targets' lanes)")
	initCmd.Flags().StringVar(&initScreens, "screens", "", "only these screens (comma-separated; default: the plan's touched screens)")
	initCmd.Flags().StringVar(&initOpts.Plan.Intensity, "intensity", "", "quick | default | full")
	initCmd.Flags().StringVar(&initOpts.Plan.Base, "base", "", "diff base ref")
	initCmd.Flags().StringVar(&initOpts.Plan.Findings, "findings", "", "findings reference recorded in the plan")
	initCmd.Flags().BoolVar(&initOpts.Force, "force", false, "recreate an existing pass's run.json (shots are kept)")

	var add polishkit.AddCellOptions
	addCmd := &cobra.Command{
		Use: "add-cell", Short: "Append a matrix cell (lane x flow x adverse-condition tier)", Args: cobra.NoArgs,
		RunE: run(func(t *polishkit.Tool, _ []string) (polishkit.Result, error) { return t.AddCell(add) }),
	}
	addCmd.Flags().IntVar(&add.Pass, "pass", 0, "pass number (default: the latest)")
	addCmd.Flags().StringVar(&add.Kind, "kind", "matrix", "cell kind (matrix)")
	addCmd.Flags().StringVar(&add.Lane, "lane", "", "lane id")
	addCmd.Flags().StringVar(&add.Flow, "flow", "", "flow title")
	addCmd.Flags().StringVar(&add.Tier, "tier", "", "adverse-condition tier row title (from the skill's reference)")
	runCmd.AddCommand(initCmd, addCmd)

	var cell polishkit.CellOptions
	cellCmd := &cobra.Command{
		Use: "cell <id> pass|fail|skip", Short: "Record a verdict; fail needs --shot", Args: cobra.ExactArgs(2),
		RunE: run(func(t *polishkit.Tool, args []string) (polishkit.Result, error) {
			cell.ID, cell.Verdict = args[0], args[1]
			return t.SetCell(cell)
		}),
	}
	cellCmd.Flags().IntVar(&cell.Pass, "pass", 0, "pass number (default: the latest)")
	cellCmd.Flags().StringVar(&cell.Shot, "shot", "", "screenshot path (required for fail)")
	cellCmd.Flags().StringVar(&cell.Note, "note", "", "what was seen")
	cellCmd.Flags().StringVar(&cell.Finding, "finding", "", "vitrinka annotation id or finding text")

	var statusPass int
	statusCmd := &cobra.Command{
		Use: "status", Short: "Counts per lane, kind and verdict; pending cells with their cell commands", Args: cobra.NoArgs,
		RunE: run(func(t *polishkit.Tool, _ []string) (polishkit.Result, error) { return t.Status(statusPass) }),
	}
	statusCmd.Flags().IntVar(&statusPass, "pass", 0, "pass number (default: the latest)")

	var shoot polishkit.ShootOptions
	var shootScreens, shootThemes, shootNav, shootText string
	shootCmd := &cobra.Command{
		Use: "shoot <lane>", Short: "Shoot every chrome cell of a simulator/device lane into <pass>/shots/<lane>/", Args: cobra.ExactArgs(1),
		RunE: run(func(t *polishkit.Tool, args []string) (polishkit.Result, error) {
			shoot.Lane = args[0]
			shoot.Screens, shoot.Themes = polishkit.SplitList(shootScreens), polishkit.SplitList(shootThemes)
			shoot.Nav, shoot.Text = polishkit.SplitList(shootNav), polishkit.SplitList(shootText)
			return t.Shoot(ctx, shoot)
		}),
	}
	shootCmd.Flags().IntVar(&shoot.Pass, "pass", 0, "pass number (default: the latest)")
	shootCmd.Flags().StringVar(&shootScreens, "screens", "", "only these screens (comma-separated)")
	shootCmd.Flags().StringVar(&shootThemes, "themes", "", "only these themes: light,dark")
	shootCmd.Flags().StringVar(&shootNav, "nav", "", "only these nav modes: gesture,3button")
	shootCmd.Flags().StringVar(&shootText, "text", "", "only these text sizes (default names the default size)")

	var sheet polishkit.SheetOptions
	var sheetLanes string
	sheetCmd := &cobra.Command{
		Use: "sheet", Short: "Per screen: a contact sheet (lanes x states) and an edges sheet (3x corners, top/bottom bands)", Args: cobra.NoArgs,
		RunE: run(func(t *polishkit.Tool, _ []string) (polishkit.Result, error) {
			sheet.Lanes = polishkit.SplitList(sheetLanes)
			return t.Sheet(sheet)
		}),
	}
	sheetCmd.Flags().IntVar(&sheet.Pass, "pass", 0, "pass number (default: the latest)")
	sheetCmd.Flags().StringVar(&sheet.Screen, "screen", "", "only this screen")
	sheetCmd.Flags().StringVar(&sheetLanes, "lanes", "", "only these lanes (comma-separated)")

	var report polishkit.ReportOptions
	var noDelta bool
	reportCmd := &cobra.Command{
		Use: "report", Short: "Markdown: a table per lane, the matrix, failing cells, delta vs the previous pass (also <pass>/report.md)", Args: cobra.NoArgs,
		RunE: run(func(t *polishkit.Tool, _ []string) (polishkit.Result, error) {
			if noDelta {
				report.Previous = -1
			}
			return t.Report(report)
		}),
	}
	reportCmd.Flags().IntVar(&report.Pass, "pass", 0, "pass number (default: the latest)")
	reportCmd.Flags().IntVar(&report.Previous, "previous", 0, "pass to diff against (default: the one before)")
	reportCmd.Flags().BoolVar(&noDelta, "no-delta", false, "skip the delta")

	root.AddCommand(planCmd, lanesCmd, runCmd, cellCmd, statusCmd, shootCmd, sheetCmd, reportCmd)
	return root
}
