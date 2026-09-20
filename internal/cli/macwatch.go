package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	goruntime "runtime"
	"syscall"
	"time"

	"github.com/henderson-tech/vybava/internal/macwatch"
	"github.com/spf13/cobra"
)

func (rt *runtime) macwatchApplet() *cobra.Command {
	c := rt.macwatchCommand("macwatch")
	c.SetOut(rt.stdout)
	c.SetErr(rt.stderr)
	c.SilenceErrors, c.SilenceUsage = true, true
	c.PersistentFlags().BoolVar(&rt.json, "json", false, "emit stable JSON")
	return c
}

func (rt *runtime) macwatchCommand(use string) *cobra.Command {
	home, _ := os.UserHomeDir()
	return rt.macwatchCommandWithSampler(use, macwatch.Sampler{Home: home}, goruntime.NumCPU())
}

func (rt *runtime) macwatchCommandWithSampler(use string, sampler macwatch.Sampler, cores int) *cobra.Command {
	c := &cobra.Command{
		Use:   use,
		Short: "Sample what is loading this Mac, attributed to project, directory and Claude/Codex session",
		Long: `macOS keeps no per-process CPU history. macwatch appends one system row and
the heaviest processes to a TSV every interval, each process tagged with the
project it works in, its working directory and the claude/codex session that
spawned it; report then integrates that file into offenders, per-project and
per-session totals and a spike ledger. Columns: docs/macwatch.md.`,
		Example: `  macwatch sample                        # 2 h, every 30 s, ~/Exports/Personal/mac-monitoring/<date>-macwatch.tsv
  macwatch sample --every 10s --for 20m --out /tmp/load.tsv
  macwatch sample --once                 # one sample to stdout
  macwatch report <file.tsv>             # timeline, offenders, projects, sessions, spikes
  macwatch report <file.tsv> --md        # Markdown tables for a vitrinka artifact
  macwatch report <file.tsv> --json`,
	}

	var out string
	var every, forDuration time.Duration
	var once bool
	sample := &cobra.Command{
		Use:   "sample",
		Short: "Append samples to a TSV until --for elapses (0 = until interrupted)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if every < time.Second {
				return fmt.Errorf("--every must be at least 1s")
			}
			if once {
				s, err := takeSample(cmd.Context(), sampler)
				if err != nil {
					return err
				}
				if rt.json {
					return json.NewEncoder(rt.stdout).Encode(s)
				}
				return writeSample(rt.stdout, s)
			}
			if out == "" {
				out = defaultOut(sampler.Home, time.Now())
			}
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				return err
			}
			defer file.Close()

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if forDuration > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, forDuration)
				defer cancel()
			}
			fmt.Fprintf(rt.stderr, "macwatch: sampling every %s for %s -> %s\n", every, describeFor(forDuration), out)
			taken := 0
			ticker := time.NewTicker(every)
			defer ticker.Stop()
			for {
				s, err := takeSample(ctx, sampler)
				if err != nil && ctx.Err() == nil {
					fmt.Fprintf(rt.stderr, "macwatch: sample failed: %v\n", err)
				} else if err == nil {
					if err := writeSample(file, s); err != nil {
						return err
					}
					taken++
				}
				select {
				case <-ctx.Done():
					_, err := fmt.Fprintln(file, macwatch.DoneLine(time.Now(), taken))
					fmt.Fprintf(rt.stderr, "macwatch: %d samples written to %s\n", taken, out)
					return err
				case <-ticker.C:
				}
			}
		},
	}
	sample.Flags().StringVar(&out, "out", "", "TSV to append to (default ~/Exports/Personal/mac-monitoring/<date>-macwatch.tsv)")
	sample.Flags().DurationVar(&every, "every", 30*time.Second, "sampling interval")
	sample.Flags().DurationVar(&forDuration, "for", 2*time.Hour, "how long to sample; 0 runs until interrupted")
	sample.Flags().BoolVar(&once, "once", false, "take one sample and print it instead of appending to --out")

	var top, reportCores int
	var md bool
	report := &cobra.Command{
		Use:   "report <file.tsv>",
		Short: "Summarize a sample file: timeline, offenders, projects, sessions, spikes",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			f, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer f.Close()
			parsed, err := macwatch.Read(f)
			if err != nil {
				return fmt.Errorf("%s: %w", args[0], err)
			}
			r := macwatch.Summarize(parsed, macwatch.ReportOptions{Top: top, Cores: reportCores})
			switch {
			case rt.json:
				return writeJSON(rt.stdout, r)
			case md:
				return macwatch.WriteMarkdown(rt.stdout, r)
			default:
				return macwatch.WriteText(rt.stdout, r)
			}
		},
	}
	report.Flags().IntVar(&top, "top", 15, "offenders per ranking")
	report.Flags().IntVar(&reportCores, "cores", cores, "cores of the sampled Mac; a spike is load1 >= 2 x cores")
	report.Flags().BoolVar(&md, "md", false, "emit Markdown tables")

	c.AddCommand(sample, report)
	return c
}

func takeSample(ctx context.Context, sampler macwatch.Sampler) (macwatch.Sample, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	return sampler.Sample(ctx)
}

func writeSample(w io.Writer, s macwatch.Sample) error {
	if _, err := io.WriteString(w, s.System.TSV()+"\n"); err != nil {
		return err
	}
	for _, p := range s.Processes {
		if _, err := io.WriteString(w, p.TSV()+"\n"); err != nil {
			return err
		}
	}
	return nil
}

func defaultOut(home string, now time.Time) string {
	return filepath.Join(home, "Exports", "Personal", "mac-monitoring", now.Format("2006-01-02")+"-macwatch.tsv")
}

func describeFor(d time.Duration) string {
	if d <= 0 {
		return "ever (until interrupted)"
	}
	return d.String()
}
