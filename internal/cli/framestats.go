package cli

import (
	"fmt"
	"slices"

	"github.com/henderson-tech/vybava/internal/framestats"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/spf13/cobra"
)

func (rt *runtime) framestatsApplet() *cobra.Command {
	command := rt.framestatsCommand("framestats")
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetOut(rt.stdout)
	command.SetErr(rt.stderr)
	command.PersistentFlags().BoolVar(&rt.json, "json", false, "emit the versioned envelope as JSON")
	return command
}

// framestatsCommand wires the two readers. Each verb runs through one
// envelope session (0 ok, 1 infra, 2 diagnostics); warnings ride a
// successful envelope beside the data.
func (rt *runtime) framestatsCommand(use string) *cobra.Command {
	command := &cobra.Command{
		Use:   use,
		Short: "Android frame metrics from gfxinfo framestats dumps and Perfetto traces",
		Long: "framestats reads the two frame records Android keeps: `dumpsys gfxinfo <pkg>\n" +
			"framestats` dumps (cadence, present intervals, frame-time percentiles,\n" +
			"per-stage medians, per-gesture first-input and release-window frames) and\n" +
			"Perfetto protobuf traces (per-frame UI doFrame and RenderThread DrawFrames,\n" +
			"slice counts per frame such as 'Texture upload', ART stop-the-world pauses,\n" +
			"FrameTimeline present and jank types). A metric without samples is null.\n" +
			"Contract: docs/framestats.md.",
		Example: `  framestats parse drag-*.framestats.txt --json
  framestats parse commits.framestats.txt --after-release-ms 250 --json
  framestats perfetto swipe.pftrace --package app.fixit.client --json
  framestats perfetto swipe.pftrace --package app.fixit.client --count ReanimatedModuleProxy::commitUpdates --json`,
	}
	session := func(cmd *cobra.Command) *runx.Session {
		return &runx.Session{Tool: "framestats", JSON: rt.json, Verb: cmd.Name(), Stdout: rt.stdout, Stderr: rt.stderr}
	}
	finish := func(s *runx.Session, data any, diags []runx.Diagnostic, err error) error {
		if err == nil {
			next := []string{}
			for _, d := range diags {
				if d.Fix != "" && !slices.Contains(next, d.Fix) {
					next = append(next, d.Fix)
				}
			}
			err = s.Emit(runx.Envelope{OK: true, Data: data, Diagnostics: diags, Next: next})
		}
		if code := s.Finish(err); code != 0 {
			return runx.ExitError{Code: code}
		}
		return nil
	}
	usage := func(detail, fix string) error {
		return runx.DiagError{Diag: runx.Diagnostic{Code: framestats.DiagUsage, Severity: "error", Detail: detail, Fix: fix}}
	}

	var label string
	var afterRelease float64
	var rows bool
	parseCmd := &cobra.Command{
		Use:   "parse <framestats.txt>...",
		Short: "Summarise gfxinfo framestats dumps (rows deduped by IntendedVsync, Flags != 0 skipped)",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s := session(cmd)
			if len(args) == 0 {
				return finish(s, nil, nil, usage("parse needs at least one framestats dump",
					"framestats parse <framestats.txt>... --json"))
			}
			summary, err := framestats.ParseFiles(args, framestats.ParseOptions{Label: label, AfterReleaseMs: afterRelease, Rows: rows})
			if err != nil {
				return finish(s, nil, nil, err)
			}
			var diags []runx.Diagnostic
			if summary.RowsMalformed > 0 {
				diags = append(diags, runx.Diagnostic{
					Code: framestats.DiagMalformedRows, Severity: "warning",
					Detail: fmt.Sprintf("%d rows were cut short, not integers or completed before their vsync; they are not measured", summary.RowsMalformed),
					Fix:    "adb shell dumpsys gfxinfo <pkg> framestats > framestats.txt",
				})
			}
			if summary.Frames == 0 {
				diags = append(diags, runx.Diagnostic{
					Code: framestats.DiagNoFrames, Severity: "warning",
					Detail: fmt.Sprintf("%d rows read, %d flagged, %d malformed, none left to measure", summary.RowsTotal, summary.RowsFlagged, summary.RowsMalformed),
					Fix:    "adb shell dumpsys gfxinfo <pkg> framestats > framestats.txt",
				})
			}
			return finish(s, summary, diags, nil)
		},
	}
	parseCmd.Flags().StringVar(&label, "label", "", "label echoed in the summary")
	parseCmd.Flags().Float64Var(&afterRelease, "after-release-ms", framestats.DefaultAfterReleaseMs, "window after each gesture's last input frame covered by the release metrics")
	parseCmd.Flags().BoolVar(&rows, "rows", false, "include one row per kept frame")

	var pkg string
	var pid int
	var counts []string
	var frames bool
	perfettoCmd := &cobra.Command{
		Use:   "perfetto <trace.pftrace>",
		Short: "Per-frame UI and RenderThread work, slice counts, ART pauses and FrameTimeline from a Perfetto trace",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s := session(cmd)
			if len(args) != 1 {
				return finish(s, nil, nil, usage("perfetto takes exactly one trace file",
					"framestats perfetto <trace.pftrace> --package <pkg> --json"))
			}
			if pkg == "" && pid <= 0 {
				return finish(s, nil, nil, usage("perfetto needs the app's --package (or its --pid)",
					fmt.Sprintf("framestats perfetto %s --package <pkg> --json", args[0])))
			}
			summary, diags, err := framestats.ReadTraceFile(args[0], framestats.TraceOptions{Package: pkg, PID: pid, Counts: counts, Frames: frames})
			if err != nil {
				return finish(s, nil, nil, err)
			}
			return finish(s, summary, diags, nil)
		},
	}
	perfettoCmd.Flags().StringVar(&pkg, "package", "", "application id whose frames to measure")
	perfettoCmd.Flags().IntVar(&pid, "pid", 0, "the app's pid, when the trace cannot name the package")
	perfettoCmd.Flags().StringArrayVar(&counts, "count", nil, "slice-name substring to count per frame (repeatable; 'Texture upload' is always counted)")
	perfettoCmd.Flags().BoolVar(&frames, "frames", false, "include one row per UI frame")

	command.AddCommand(parseCmd, perfettoCmd)
	return command
}
