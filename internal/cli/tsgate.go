package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/tsgate"
	"github.com/henderson-tech/vybava/internal/vconfig"
	"github.com/spf13/cobra"
)

func (rt *runtime) tsgateApplet() *cobra.Command {
	command := rt.tsgateCommand("tsgate")
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetOut(rt.stdout)
	command.SetErr(rt.stderr)
	command.PersistentFlags().BoolVar(&rt.json, "json", false, "emit stable JSON output")
	return command
}

// tsgateConfig is vybava.config.ts's `tsgate` section.
type tsgateConfig struct {
	Programs []string `json:"programs"` // tsconfigs, relative to the config
	Compiler string   `json:"compiler"` // the TS 7 dependency name
}

func (rt *runtime) tsgateCommand(use string) *cobra.Command {
	var build bool
	var compiler string
	command := &cobra.Command{
		Use:   use + " [tsconfig...] [-- tsc args]",
		Short: "Typecheck on the TypeScript 7 native compiler while typescript stays 6 for the tools",
		Long: `Runs a program's typecheck on TypeScript 7 (tsc -p <tsconfig> --noEmit) while the
repo's ` + "`typescript`" + ` stays on 6 or 5.9 for every tool that imports the compiler API
TS 7 does not ship. TS 7 is an alias devDependency ("typescript7":
"npm:typescript@7.0.2"); tsgate finds it by version and execs its native binary,
because its bin is also named tsc and node_modules/.bin cannot hold both.

A tsconfig chain TS 7 refuses (baseUrl, moduleResolution node10, outDir without
rootDir) is flattened into .<name>.ts7.json beside the tsconfig on every run, so
it cannot drift; gitignore .*.ts7.json. Other removed options stay TS 7's own
error: move them in the tsconfig. With no tsconfig named, runs vybava.config.ts's
tsgate.programs. Contract: docs/tsgate.md.`,
		Example: `  tsgate tsconfig.json                       # the gate a typecheck script calls
  tsgate apps/api/tsconfig.spec.json -- --extendedDiagnostics
  tsgate -b tsconfig.json                    # build mode on a tsconfig TS 7 reads as it is
  tsgate plan tsconfig.json                  # which compiler, and the derived config if any
  tsgate parity tsconfig.json                # the current compiler vs TS 7, diagnostic by diagnostic`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			programs, extra, deps, err := rt.tsgatePrograms(cmd, args, compiler)
			if err != nil {
				return err
			}
			code := 0
			if rt.json {
				results := []tsgate.Result{}
				for _, program := range programs {
					r, err := tsgate.Check(program, tsgate.Options{Dependencies: deps, Build: build, Args: extra})
					if err != nil {
						return err
					}
					results = append(results, r)
					if r.Exit != 0 && code == 0 {
						code = r.Exit
					}
				}
				if err := writeJSON(rt.stdout, results); err != nil {
					return err
				}
				if code != 0 {
					return runx.ExitError{Code: code}
				}
				return nil
			}
			for _, program := range programs {
				if len(programs) > 1 {
					fmt.Fprintf(rt.stderr, "tsgate: %s\n", program)
				}
				exit, err := tsgate.Run(program, tsgate.Options{Dependencies: deps, Build: build, Args: extra, Stdout: rt.stdout, Stderr: rt.stderr})
				if err != nil {
					return err
				}
				if exit != 0 && code == 0 {
					code = exit
				}
			}
			if code != 0 {
				return runx.ExitError{Code: code}
			}
			return nil
		},
	}
	command.Flags().BoolVarP(&build, "build", "b", false, "tsc -b on the tsconfig itself (it must need no derivation)")
	command.PersistentFlags().StringVar(&compiler, "compiler", "", "the TS 7 dependency name (default: typescript7, @typescript/native, typescript)")

	plan := &cobra.Command{
		Use:   "plan [tsconfig...]",
		Short: "Show the compiler and the config TS 7 would read, and why a derivation is needed",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			programs, _, deps, err := rt.tsgatePrograms(cmd, args, compiler)
			if err != nil {
				return err
			}
			type entry struct {
				tsgate.Plan
				Compiler *tsgate.Compiler `json:"compiler"`
				Error    string           `json:"error,omitempty"`
			}
			var out []entry
			for _, program := range programs {
				p, err := tsgate.PlanProgram(program)
				if err != nil {
					return err
				}
				e := entry{Plan: p}
				if c, err := tsgate.FindTS7(filepath.Dir(p.Tsconfig), deps); err == nil {
					e.Compiler = &c
				} else {
					e.Error = err.Error()
				}
				out = append(out, e)
			}
			if rt.json {
				return writeJSON(rt.stdout, out)
			}
			for _, e := range out {
				fmt.Fprintln(rt.stdout, e.Tsconfig)
				if e.Compiler != nil {
					fmt.Fprintf(rt.stdout, "  compiler  %s -> %s %s  %s\n", e.Compiler.Dependency, e.Compiler.Package, e.Compiler.Version, e.Compiler.Command[0])
				} else {
					fmt.Fprintf(rt.stdout, "  compiler  ✗ %s\n", e.Error)
				}
				if !e.Derive {
					fmt.Fprintln(rt.stdout, "  reads     the tsconfig as it is")
					continue
				}
				fmt.Fprintf(rt.stdout, "  reads     %s (derived on every run)\n", e.Target)
				for _, r := range e.Reasons {
					fmt.Fprintf(rt.stdout, "  because   %s\n", r)
				}
			}
			return nil
		},
	}

	parity := &cobra.Command{
		Use:   "parity [tsconfig...] [-- tsc args]",
		Short: "Run the classic compiler and TS 7 on each program and compare diagnostics (exit 1 on a difference)",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			programs, extra, deps, err := rt.tsgatePrograms(cmd, args, compiler)
			if err != nil {
				return err
			}
			reports := []tsgate.ParityReport{}
			same := true
			for _, program := range programs {
				r, err := tsgate.Parity(program, tsgate.Options{Dependencies: deps, Args: extra})
				if err != nil {
					return err
				}
				reports = append(reports, r)
				same = same && r.Same
			}
			if rt.json {
				if err := writeJSON(rt.stdout, reports); err != nil {
					return err
				}
			} else {
				for _, r := range reports {
					rt.tsgateParityReport(r)
				}
			}
			if !same {
				return ErrFindings
			}
			return nil
		},
	}
	command.AddCommand(plan, parity)
	return command
}

// tsgatePrograms splits argv at `--` into tsconfigs and compiler arguments,
// falling back to the config's tsgate.programs, and resolves the TS 7
// dependency names (--compiler, else the config's, else the defaults).
func (rt *runtime) tsgatePrograms(cmd *cobra.Command, args []string, compiler string) (programs, extra, deps []string, err error) {
	programs = args
	if dash := cmd.ArgsLenAtDash(); dash >= 0 {
		programs, extra = args[:dash], args[dash:]
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, nil, err
	}
	var cfg tsgateConfig
	var root string
	if c, loadErr := vconfig.Load(cwd); loadErr == nil {
		root = c.Root
		if err := c.Section("tsgate", &cfg); err != nil && !errors.Is(err, vconfig.ErrNoSection) {
			return nil, nil, nil, err
		}
	} else if !errors.Is(loadErr, vconfig.ErrNotFound) {
		if len(programs) == 0 {
			return nil, nil, nil, loadErr
		}
		fmt.Fprintf(rt.stderr, "tsgate: vybava.config: %v (continuing with the named tsconfigs)\n", loadErr)
	}
	if len(programs) == 0 {
		for _, p := range cfg.Programs {
			programs = append(programs, filepath.Join(root, filepath.FromSlash(p)))
		}
	}
	if len(programs) == 0 {
		return nil, nil, nil, fmt.Errorf("name a tsconfig, or list tsgate.programs in vybava.config.ts")
	}
	switch {
	case compiler != "":
		deps = []string{compiler}
	case cfg.Compiler != "":
		deps = []string{cfg.Compiler}
	}
	return programs, extra, deps, nil
}

func (rt *runtime) tsgateParityReport(r tsgate.ParityReport) {
	side := func(s tsgate.Side) {
		fmt.Fprintf(rt.stdout, "  %-12s %-8s %6.1f s  exit %d  %d diagnostics\n",
			s.Compiler.Package, s.Compiler.Version, float64(s.WallMS)/1000, s.Exit, s.Diagnostics)
	}
	fmt.Fprintln(rt.stdout, r.Tsconfig)
	side(r.Baseline)
	side(r.TS7)
	if r.Same {
		fmt.Fprintln(rt.stdout, "  ✓ same diagnostics")
		return
	}
	for _, d := range r.OnlyBaseline {
		fmt.Fprintf(rt.stdout, "  only %s  %s\n", r.Baseline.Compiler.Version, strings.TrimSpace(d.String()))
	}
	for _, d := range r.OnlyTS7 {
		fmt.Fprintf(rt.stdout, "  only %s  %s\n", r.TS7.Compiler.Version, strings.TrimSpace(d.String()))
	}
}
