package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/henderson-tech/vybava/internal/devlab"
	"github.com/henderson-tech/vybava/internal/perflab"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/spf13/cobra"
)

func (rt *runtime) perflabApplet() *cobra.Command {
	command := rt.perflabCommand("perflab")
	command.SetOut(rt.stdout)
	command.SetErr(rt.stderr)
	return command
}

const perflabUsage = `perflab adapter check
perflab device scan|add <id>|list|show <d>|remove <d>|probe <d>
perflab device shell <d> --lease <t> -- <adb|devicectl args>
perflab device screencap <d> --lease <t> [--out f] | pull <d> --lease <t> <remote> <local>
perflab lease acquire <d> [--for 2h] [--purpose "<why>"] [--wait d] | status [<d>] | renew <d> --lease <t>
perflab lease release <d> --lease <t> | reap [--dry-run] | break <d> --reason "<why>"
perflab doctor [--device <d> --lease <t>] [--platform ios|android] [--for build|run|probe|all] [--wake]
perflab fingerprint --platform <p> [--profile perf] [--kind shell|bundled]
perflab build find|native --platform <p> [--profile perf] [--kind shell|bundled] | import <artifact> | list | gc
perflab wda find|build [--team t] [--bundle-id id] [--wait d] | import <derivedData> | list
perflab bundle export --platform <p> [--profile perf] [--ref <git ref>] [--label <name>] | list
perflab pack --native <key> --bundle <sha>
perflab install <variant|key> --device <d> --lease <t>
perflab net forward|status|stop --device <d> --lease <t> [--device-port N]
perflab app launch|link|reset --device <d> --lease <t> [--account a] [--route r] [--world w]
perflab run <scenario>... --device <d> --lease <t> --variant [label=]<id>... [--alternate] [--repeat N] [--resume <runDir>]
perflab probe rest|drag|fling|custom --device <d> --lease <t> [--package p] [--seconds 20]
perflab analyze <path>... [--marks f] [--wdio-log f --step-cycle a,b] [--window a-b --classify|--stacks] [--sql preset] [--reread]
perflab compare <runDir> [<runDir>] [--threshold 0.15] [--min-runs 2] [--allow-confound field]
perflab report [<runDir>...] [--gate] [--md file]
perflab hazards [<dir>] [--gate --baseline f] [--write-baseline f]
perflab crashes --device <d> --lease <t> [--since <RFC3339|30m>]
Global: --json, --project <dir>, --timeout <dur>, --log <file>. Contract: docs/perflab.md.`

// perflabCommand is the outer shell: no flags of its own, so a verb's flags
// reach it intact, and a cobra parse error becomes a USAGE diagnostic with
// the corrected invocation, never a usage dump. Domain logic lives in
// internal/perflab and the packages it composes.
func (rt *runtime) perflabCommand(use string) *cobra.Command {
	return &cobra.Command{
		Use:                use,
		Short:              "Physical-device performance lab for Expo / React Native: device ledger and leases, native build index, bundle variants, measured runs, trace readers",
		Long:               perflabUsage,
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
				_, err := fmt.Fprintln(rt.stdout, perflabUsage)
				return err
			}
			inner := rt.perflabVerbs()
			inner.SetArgs(args)
			inner.SetOut(rt.stdout)
			inner.SetErr(rt.stderr)
			err := inner.Execute()
			var exit runx.ExitError
			if err == nil || errors.As(err, &exit) {
				return err
			}
			verb := strings.Join(leadingWords(args, 2), " ")
			s := &runx.Session{Tool: "perflab", JSON: rt.json || hasJSONFlag(args), Verb: verb, Stdout: rt.stdout, Stderr: rt.stderr}
			derr := runx.DiagError{Diag: runx.Diagnostic{Code: perflab.DiagUsage, Severity: "error", Detail: err.Error(), Fix: usageFix(verb)}}
			if code := s.Finish(derr); code != 0 {
				return runx.ExitError{Code: code}
			}
			return nil
		},
	}
}

// leadingWords are the first n non-flag words (the verb path).
func leadingWords(args []string, n int) []string {
	var out []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") || len(out) == n {
			break
		}
		out = append(out, a)
	}
	return out
}

// usageFix is the usage line for a verb, else the help pointer. A sub-verb
// shares its group's line (`wda build` is `perflab wda find|build ...`).
func usageFix(verb string) string {
	words := strings.Fields(verb)
	for _, line := range strings.Split(perflabUsage, "\n") {
		if verb == "" {
			break
		}
		if strings.HasPrefix(line, "perflab "+verb) {
			return line + " --json"
		}
		f := strings.Fields(line)
		if len(words) == 2 && len(f) > 2 && f[1] == words[0] && slices.Contains(strings.Split(f[2], "|"), words[1]) {
			return line + " --json"
		}
	}
	return "perflab --help"
}

type perflabFlags struct {
	project string
	timeout time.Duration
	log     string
}

func (rt *runtime) perflabVerbs() *cobra.Command {
	var g perflabFlags
	root := &cobra.Command{Use: "perflab", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().BoolVar(&rt.json, "json", rt.json, "emit the versioned envelope as JSON")
	root.PersistentFlags().StringVar(&g.project, "project", "", "adapter root (default: the git toplevel of the working directory)")
	root.PersistentFlags().DurationVar(&g.timeout, "timeout", 0, "hard limit for the whole verb (default: per verb, docs/perflab.md)")
	root.PersistentFlags().StringVar(&g.log, "log", "", "tee the perflab[...] progress lines to this file")

	verbName := func(cmd *cobra.Command) string {
		name := cmd.Name()
		if p := cmd.Parent(); p != nil && p != root {
			name = p.Name() + " " + name
		}
		return name
	}
	finish := func(s *runx.Session, res perflab.Result, err error) error {
		failed := perflab.HasErrors(res.Diagnostics)
		if err == nil {
			if !rt.json {
				for _, l := range res.Lines {
					fmt.Fprintln(rt.stdout, l)
				}
				res.Data = nil
			}
			err = s.Emit(runx.Envelope{OK: !failed, Data: res.Data, Diagnostics: res.Diagnostics, Next: res.Next})
			if err == nil && failed {
				err = runx.ExitError{Code: 2}
			}
		} else if res.Data != nil || len(res.Diagnostics) > 0 {
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
			}
			_ = s.Emit(env)
		}
		if code := s.Finish(err); code != 0 {
			return runx.ExitError{Code: code}
		}
		return nil
	}
	// run opens the tool and runs a verb under a context that SIGINT and
	// SIGTERM cancel (a run's runner group and a net forward stop cleanly).
	run := func(verb func(context.Context, *perflab.Tool, []string) (perflab.Result, error)) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			s := &runx.Session{Tool: "perflab", JSON: rt.json, Verb: verbName(cmd), Stdout: rt.stdout, Stderr: rt.stderr}
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			if g.timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, g.timeout)
				defer cancel()
			}
			var log io.Writer = rt.stderr
			if g.log != "" {
				f, err := os.OpenFile(g.log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
				if err != nil {
					return finish(s, perflab.Result{}, runx.DiagError{Diag: runx.Diagnostic{Code: perflab.DiagUsage, Severity: "error", Detail: "--log: " + err.Error(), Fix: "pass a writable --log path"}})
				}
				defer f.Close()
				log = io.MultiWriter(rt.stderr, f)
			}
			t, err := perflab.Open(ctx, g.project, log)
			if err != nil {
				return finish(s, perflab.Result{}, err)
			}
			res, err := verb(ctx, t, args)
			return finish(s, res, err)
		}
	}
	lab := func(fn func(context.Context, *devlab.Lab, []string) (devlab.Result, error)) func(*cobra.Command, []string) error {
		return run(func(ctx context.Context, t *perflab.Tool, args []string) (perflab.Result, error) {
			return fn(ctx, t.Lab, args)
		})
	}

	root.AddCommand(
		rt.perflabAdapter(run),
		rt.perflabDevice(lab),
		rt.perflabLease(lab),
		rt.perflabDoctor(run),
		rt.perflabBuild(run),
		rt.perflabWDA(run),
	)
	root.AddCommand(rt.perflabRunVerbs(run)...)
	return root
}

type verbRunner = func(func(context.Context, *perflab.Tool, []string) (perflab.Result, error)) func(*cobra.Command, []string) error
type labRunner = func(func(context.Context, *devlab.Lab, []string) (devlab.Result, error)) func(*cobra.Command, []string) error

func (rt *runtime) perflabAdapter(run verbRunner) *cobra.Command {
	adapter := &cobra.Command{Use: "adapter", Short: "The project's perflab section of vybava.config.ts"}
	adapter.AddCommand(&cobra.Command{
		Use: "check", Short: "Validate the section (unknown keys and {tokens} are rejected), show every command for this worktree, decode the scenario rows", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, t *perflab.Tool, _ []string) (perflab.Result, error) {
			return t.AdapterCheck(ctx)
		}),
	})
	return adapter
}

func (rt *runtime) perflabDevice(lab labRunner) *cobra.Command {
	device := &cobra.Command{Use: "device", Short: "The machine-global ledger of physical phones"}

	var scanPlatform string
	scan := &cobra.Command{Use: "scan", Short: "List the phones attached or paired now, with suggested ledger ids", Args: cobra.NoArgs,
		RunE: lab(func(ctx context.Context, l *devlab.Lab, _ []string) (devlab.Result, error) {
			return l.Scan(ctx, devlab.ScanOptions{Platform: devlab.Platform(scanPlatform)})
		})}
	scan.Flags().StringVar(&scanPlatform, "platform", "", "ios | android (default both)")

	var add devlab.AddOptions
	var personal bool
	var notes string
	addCmd := &cobra.Command{Use: "add <id>", Short: "Register a phone (identity from the live scan); re-running updates the row", Args: cobra.ExactArgs(1)}
	addCmd.RunE = lab(func(ctx context.Context, l *devlab.Lab, args []string) (devlab.Result, error) {
		if addCmd.Flags().Changed("personal") {
			add.Personal = &personal
		}
		if addCmd.Flags().Changed("notes") {
			add.Notes = &notes
		}
		return l.Add(ctx, args[0], add)
	})
	addCmd.Flags().StringVar(&add.UDID, "udid", "", "iOS hardware UDID (xctrace, Appium, the tunnel registry)")
	addCmd.Flags().StringVar(&add.CoreDeviceID, "core-device-id", "", "iOS CoreDevice id (devicectl)")
	addCmd.Flags().StringVar(&add.Serial, "serial", "", "Android adb serial")
	addCmd.Flags().StringVar(&add.Label, "label", "", "human label")
	addCmd.Flags().IntVar(&add.ExpectHz, "expect-hz", 0, "the display rate a measurement needs (60 | 120)")
	addCmd.Flags().StringArrayVar(&add.ProtectPackages, "protect-package", nil, "a package perflab never uninstalls or clears (repeatable)")
	addCmd.Flags().BoolVar(&personal, "personal", false, "a personal phone (its own apps are protected)")
	addCmd.Flags().StringVar(&notes, "notes", "", "free text")

	var noLive bool
	list := &cobra.Command{Use: "list", Short: "Ledger rows merged with live state and the lease holder", Args: cobra.NoArgs,
		RunE: lab(func(ctx context.Context, l *devlab.Lab, _ []string) (devlab.Result, error) {
			return l.List(ctx, !noLive)
		})}
	list.Flags().BoolVar(&noLive, "no-live", false, "skip the live scan")

	show := &cobra.Command{Use: "show <device>", Short: "One row, its lease, last probe and last install", Args: cobra.ExactArgs(1),
		RunE: lab(func(_ context.Context, l *devlab.Lab, args []string) (devlab.Result, error) { return l.Show(args[0]) })}

	var rm devlab.RemoveOptions
	remove := &cobra.Command{Use: "remove <device>", Short: "Remove a row (refused while leased)", Args: cobra.ExactArgs(1),
		RunE: lab(func(_ context.Context, l *devlab.Lab, args []string) (devlab.Result, error) {
			return l.Remove(args[0], rm)
		})}
	remove.Flags().BoolVar(&rm.Yes, "yes", false, "confirm")

	var probeLease string
	probe := &cobra.Command{Use: "probe <device>", Short: "Read the device's state; in-device readings need the lease or a free device", Args: cobra.ExactArgs(1),
		RunE: lab(func(ctx context.Context, l *devlab.Lab, args []string) (devlab.Result, error) {
			return l.Probe(ctx, args[0], devlab.ProbeOptions{Lease: probeLease})
		})}
	probe.Flags().StringVar(&probeLease, "lease", "", "lease token (adds the in-device checks)")

	var shellLease string
	var shellOpts devlab.ShellOptions
	shell := &cobra.Command{Use: "shell <device> -- <args>", Short: "Run adb (Android) or devicectl (iOS) arguments against the leased device", Args: cobra.MinimumNArgs(1)}
	shell.RunE = lab(func(ctx context.Context, l *devlab.Lab, args []string) (devlab.Result, error) {
		at := shell.ArgsLenAtDash()
		if at != 1 {
			return devlab.Result{}, runx.DiagError{Diag: runx.Diagnostic{Code: perflab.DiagUsage, Severity: "error",
				Detail: "device shell takes the device, then -- and the wrapped arguments", Fix: "perflab device shell <device> --lease <token> --json -- shell dumpsys window"}}
		}
		return l.Shell(ctx, args[0], shellLease, args[1:], shellOpts)
	})
	shell.Flags().StringVar(&shellLease, "lease", "", "lease token")
	shell.Flags().StringVar(&shellOpts.Out, "out", "", "write the command's stdout to this file instead of the envelope")
	shell.Flags().DurationVar(&shellOpts.Timeout, "cmd-timeout", 0, "limit for the wrapped command (default 2m)")

	var capLease string
	var capOpts devlab.ScreencapOptions
	screencap := &cobra.Command{Use: "screencap <device>", Short: "Screenshot the leased device into a PNG", Args: cobra.ExactArgs(1),
		RunE: lab(func(ctx context.Context, l *devlab.Lab, args []string) (devlab.Result, error) {
			return l.Screencap(ctx, args[0], capLease, capOpts)
		})}
	screencap.Flags().StringVar(&capLease, "lease", "", "lease token")
	screencap.Flags().StringVar(&capOpts.Out, "out", "", "PNG path (required)")

	var pullLease string
	var pullOpts devlab.PullOptions
	pull := &cobra.Command{Use: "pull <device> <remote> <local>", Short: "Copy one file off the leased device", Args: cobra.ExactArgs(3),
		RunE: lab(func(ctx context.Context, l *devlab.Lab, args []string) (devlab.Result, error) {
			pullOpts.Remote, pullOpts.Local = args[1], args[2]
			return l.Pull(ctx, args[0], pullLease, pullOpts)
		})}
	pull.Flags().StringVar(&pullLease, "lease", "", "lease token")
	pull.Flags().StringVar(&pullOpts.DomainType, "domain-type", "", "iOS: appDataContainer | systemCrashLogs | …")
	pull.Flags().StringVar(&pullOpts.DomainID, "domain-id", "", "iOS: the bundle id for appDataContainer")

	device.AddCommand(scan, addCmd, list, show, remove, probe, shell, screencap, pull)
	return device
}

func (rt *runtime) perflabLease(lab labRunner) *cobra.Command {
	lease := &cobra.Command{Use: "lease", Short: "One token holder per physical device"}
	var acq devlab.AcquireOptions
	acquire := &cobra.Command{Use: "acquire <device>", Short: "Mint the lease token (printed once; next lines carry it)", Args: cobra.ExactArgs(1),
		RunE: lab(func(ctx context.Context, l *devlab.Lab, args []string) (devlab.Result, error) {
			return l.Acquire(ctx, args[0], acq)
		})}
	acquire.Flags().DurationVar(&acq.For, "for", 2*time.Hour, "lease length (max 8h)")
	acquire.Flags().StringVar(&acq.Purpose, "purpose", "", "why (shown to anyone the lease refuses)")
	acquire.Flags().DurationVar(&acq.Wait, "wait", 0, "wait this long for a held lease")

	status := &cobra.Command{Use: "status [<device>]", Short: "Every lease (or one): holder, since, heartbeat, expiry, last install", Args: cobra.MaximumNArgs(1),
		RunE: lab(func(_ context.Context, l *devlab.Lab, args []string) (devlab.Result, error) {
			handle := ""
			if len(args) == 1 {
				handle = args[0]
			}
			return l.Status(handle)
		})}

	var renewLease string
	var renew devlab.RenewOptions
	renewCmd := &cobra.Command{Use: "renew <device>", Short: "Extend the expiry", Args: cobra.ExactArgs(1),
		RunE: lab(func(_ context.Context, l *devlab.Lab, args []string) (devlab.Result, error) {
			return l.Renew(args[0], renewLease, renew)
		})}
	renewCmd.Flags().StringVar(&renewLease, "lease", "", "lease token")
	renewCmd.Flags().DurationVar(&renew.For, "for", 2*time.Hour, "new length from now (max 8h)")

	var relLease string
	release := &cobra.Command{Use: "release <device>", Short: "End the lease (stops the process groups it recorded)", Args: cobra.ExactArgs(1),
		RunE: lab(func(_ context.Context, l *devlab.Lab, args []string) (devlab.Result, error) {
			return l.ReleaseLease(args[0], relLease)
		})}
	release.Flags().StringVar(&relLease, "lease", "", "lease token")

	var dry bool
	reap := &cobra.Command{Use: "reap", Short: "Release every lease that is expired AND whose holder is dead", Args: cobra.NoArgs,
		RunE: lab(func(_ context.Context, l *devlab.Lab, _ []string) (devlab.Result, error) { return l.Reap(dry) })}
	reap.Flags().BoolVar(&dry, "dry-run", false, "list, release nothing")

	var brk devlab.BreakOptions
	breakCmd := &cobra.Command{Use: "break <device>", Short: "End a lease whose holder is dead or which expired (never a live holder's)", Args: cobra.ExactArgs(1),
		RunE: lab(func(_ context.Context, l *devlab.Lab, args []string) (devlab.Result, error) {
			return l.Break(args[0], brk)
		})}
	breakCmd.Flags().StringVar(&brk.Reason, "reason", "", "why (recorded)")

	lease.AddCommand(acquire, status, renewCmd, release, reap, breakCmd)
	return lease
}

func (rt *runtime) perflabDoctor(run verbRunner) *cobra.Command {
	var o perflab.DoctorOptions
	doctor := &cobra.Command{Use: "doctor", Short: "Preflight: host, tools, WDA, tunnel, API; with --device the device checks", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, t *perflab.Tool, _ []string) (perflab.Result, error) {
			return t.Doctor(ctx, o)
		})}
	doctor.Flags().StringVar(&o.Device, "device", "", "ledger id or alias")
	doctor.Flags().StringVar(&o.Lease, "lease", "", "lease token (the deep device checks)")
	doctor.Flags().StringVar(&o.Platform, "platform", "", "ios | android")
	doctor.Flags().StringVar(&o.For, "for", "all", "build | run | probe | all")
	doctor.Flags().BoolVar(&o.Wake, "wake", false, "launch the app to bring an offline iPhone back to Instruments")
	return doctor
}

func buildFlags(cmd *cobra.Command, o *perflab.BuildOptions, kind bool) {
	cmd.Flags().StringVar(&o.Platform, "platform", "", "ios | android")
	cmd.Flags().StringVar(&o.Profile, "profile", "perf", "a key of perflab.profiles")
	if kind {
		cmd.Flags().StringVar(&o.Kind, "kind", "", "shell (no JS, needs pack) | bundled (as shipped); default shell on ios, bundled on android")
	}
}

func (rt *runtime) perflabBuild(run verbRunner) *cobra.Command {
	build := &cobra.Command{Use: "build", Short: "The native build index"}
	var fo, bo, io_ perflab.BuildOptions
	find := &cobra.Command{Use: "find", Short: "Look the working tree's portable native key up", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, t *perflab.Tool, _ []string) (perflab.Result, error) {
			return t.BuildFind(ctx, fo)
		})}
	buildFlags(find, &fo, true)
	native := &cobra.Command{Use: "native", Short: "Return the index hit, or build it (host build lock, stall watchdog)", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, t *perflab.Tool, _ []string) (perflab.Result, error) {
			return t.BuildNative(ctx, bo)
		})}
	buildFlags(native, &bo, true)
	native.Flags().StringVar(&bo.Team, "team", "", "iOS signing team (default app.ios.team)")
	native.Flags().StringVar(&bo.Ref, "ref", "", "build from a detached source worktree at this git ref")
	native.Flags().DurationVar(&bo.Wait, "wait", 0, "wait for the key or host build lock")
	native.Flags().DurationVar(&bo.Stall, "stall", 10*time.Minute, "stop when no output arrives for this long")
	native.Flags().DurationVar(&bo.Timeout, "build-timeout", 60*time.Minute, "hard limit for the build")
	imp := &cobra.Command{Use: "import <artifact>", Short: "Adopt an existing .app or .apk built from this tree", Args: cobra.ExactArgs(1),
		RunE: run(func(ctx context.Context, t *perflab.Tool, args []string) (perflab.Result, error) {
			return t.BuildImport(ctx, args[0], io_)
		})}
	buildFlags(imp, &io_, true)
	list := &cobra.Command{Use: "list", Short: "Native builds, bundles and variants in the cache", Args: cobra.NoArgs,
		RunE: run(func(_ context.Context, t *perflab.Tool, _ []string) (perflab.Result, error) { return t.BuildList() })}
	var keep int
	var maxSize string
	var dry bool
	gc := &cobra.Command{Use: "gc", Short: "Delete the oldest unused entries (a lease's last install is pinned)", Args: cobra.NoArgs,
		RunE: run(func(_ context.Context, t *perflab.Tool, _ []string) (perflab.Result, error) {
			size, err := parseSize(maxSize)
			if err != nil {
				return perflab.Result{}, runx.DiagError{Diag: runx.Diagnostic{Code: perflab.DiagUsage, Severity: "error", Detail: err.Error(), Fix: "perflab build gc --keep 3 --max-size 40G --json"}}
			}
			return t.BuildGC(keep, size, dry)
		})}
	gc.Flags().IntVar(&keep, "keep", 3, "newest entries kept per group")
	gc.Flags().StringVar(&maxSize, "max-size", "40G", "cache size ceiling (K, M, G suffixes)")
	gc.Flags().BoolVar(&dry, "dry-run", false, "list, delete nothing")
	build.AddCommand(find, native, imp, list, gc)
	return build
}

// parseSize reads 40G / 512M / 1024K / a byte count.
func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	if s == "" || s == "0" {
		return 0, nil
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "G"):
		mult, s = 1<<30, strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "M"):
		mult, s = 1<<20, strings.TrimSuffix(s, "M")
	case strings.HasSuffix(s, "K"):
		mult, s = 1<<10, strings.TrimSuffix(s, "K")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("--max-size %q is not a size like 40G", s)
	}
	return n * mult, nil
}

func (rt *runtime) perflabWDA(run verbRunner) *cobra.Command {
	w := &cobra.Command{Use: "wda", Short: "The prebuilt WebDriverAgent index"}
	var team, bundle string
	var stall, timeout, wait time.Duration
	flags := func(c *cobra.Command) {
		c.Flags().StringVar(&team, "team", "", "signing team (default app.ios.team)")
		c.Flags().StringVar(&bundle, "bundle-id", "", "WDA bundle id (default <app bundle id>.WebDriverAgentRunner)")
	}
	find := &cobra.Command{Use: "find", Short: "Look the prebuilt WDA for this Xcode, driver, team and bundle id up", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, t *perflab.Tool, _ []string) (perflab.Result, error) {
			return t.WDAFind(ctx, team, bundle)
		})}
	flags(find)
	build := &cobra.Command{Use: "build", Short: "Build the WDA once (build-for-testing) under the host build lock", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, t *perflab.Tool, _ []string) (perflab.Result, error) {
			return t.WDABuild(ctx, team, bundle, stall, timeout, wait)
		})}
	flags(build)
	build.Flags().DurationVar(&stall, "stall", 10*time.Minute, "stop when no output arrives for this long")
	build.Flags().DurationVar(&timeout, "build-timeout", 20*time.Minute, "hard limit")
	build.Flags().DurationVar(&wait, "wait", 0, "wait this long for another perflab build to free the Mac-wide build lock")
	imp := &cobra.Command{Use: "import <derivedData>", Short: "Adopt a WDA derived-data dir built by this Xcode", Args: cobra.ExactArgs(1),
		RunE: run(func(ctx context.Context, t *perflab.Tool, args []string) (perflab.Result, error) {
			return t.WDAImport(ctx, args[0], team, bundle)
		})}
	flags(imp)
	list := &cobra.Command{Use: "list", Short: "Prebuilt WDAs in the cache", Args: cobra.NoArgs,
		RunE: run(func(_ context.Context, t *perflab.Tool, _ []string) (perflab.Result, error) { return t.WDAList() })}
	w.AddCommand(find, build, imp, list)
	return w
}

func (rt *runtime) perflabRunVerbs(run verbRunner) []*cobra.Command {
	var fp perflab.BuildOptions
	fingerprint := &cobra.Command{Use: "fingerprint", Short: "The working tree's portable native key (pf1)", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, t *perflab.Tool, _ []string) (perflab.Result, error) {
			return t.Fingerprint(ctx, fp)
		})}
	buildFlags(fingerprint, &fp, true)

	bundle := &cobra.Command{Use: "bundle", Short: "JS bundles (Hermes bytecode), content-addressed"}
	var be perflab.BuildOptions
	export := &cobra.Command{Use: "export", Short: "Export the JS bundle of the tree (or --ref) into the cache", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, t *perflab.Tool, _ []string) (perflab.Result, error) {
			return t.BundleExport(ctx, be)
		})}
	buildFlags(export, &be, false)
	export.Flags().StringVar(&be.Ref, "ref", "", "export from a detached source worktree at this git ref")
	export.Flags().StringVar(&be.Label, "label", "", "a name for the bundle (before, after, …)")
	export.Flags().DurationVar(&be.Timeout, "export-timeout", 10*time.Minute, "hard limit")
	var blPlatform string
	blist := &cobra.Command{Use: "list", Short: "Exported bundles", Args: cobra.NoArgs,
		RunE: run(func(_ context.Context, t *perflab.Tool, _ []string) (perflab.Result, error) {
			return t.BundleList(blPlatform)
		})}
	blist.Flags().StringVar(&blPlatform, "platform", "", "ios | android")
	bundle.AddCommand(export, blist)

	var native, bsha string
	pack := &cobra.Command{Use: "pack", Short: "Put a bundle into a native build: a content-addressed variant", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, t *perflab.Tool, _ []string) (perflab.Result, error) {
			return t.Pack(ctx, native, bsha)
		})}
	pack.Flags().StringVar(&native, "native", "", "native key (pf1-…)")
	pack.Flags().StringVar(&bsha, "bundle", "", "bundle sha")

	var insDevice, insLease string
	var insTimeout time.Duration
	install := &cobra.Command{Use: "install <variant|nativeKey>", Short: "Install on the leased device, verify and record the fence", Args: cobra.ExactArgs(1),
		RunE: run(func(ctx context.Context, t *perflab.Tool, args []string) (perflab.Result, error) {
			return t.Install(ctx, insDevice, insLease, args[0], insTimeout)
		})}
	install.Flags().StringVar(&insDevice, "device", "", "ledger id or alias")
	install.Flags().StringVar(&insLease, "lease", "", "lease token")
	install.Flags().DurationVar(&insTimeout, "install-timeout", 10*time.Minute, "hard limit")

	net := &cobra.Command{Use: "net", Short: "Android: the device port reversed and forwarded to the API"}
	var netDevice, netLease string
	var netPort int
	netFlags := func(c *cobra.Command) {
		c.Flags().StringVar(&netDevice, "device", "", "ledger id or alias")
		c.Flags().StringVar(&netLease, "lease", "", "lease token")
		c.Flags().IntVar(&netPort, "device-port", 0, "device port (default api.device.android.devicePort)")
	}
	forward := &cobra.Command{Use: "forward", Short: "Reverse + forward until SIGINT (run it as a background task)", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, t *perflab.Tool, _ []string) (perflab.Result, error) {
			return t.NetForward(ctx, netDevice, netLease, netPort)
		})}
	status := &cobra.Command{Use: "status", Short: "Each link of the forward chain", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, t *perflab.Tool, _ []string) (perflab.Result, error) {
			return t.NetStatus(ctx, netDevice, netLease, netPort)
		})}
	stopCmd := &cobra.Command{Use: "stop", Short: "Stop the recorded forwarder and remove its reverse", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, t *perflab.Tool, _ []string) (perflab.Result, error) {
			return t.NetStop(ctx, netDevice, netLease, netPort)
		})}
	for _, c := range []*cobra.Command{forward, status, stopCmd} {
		netFlags(c)
	}
	net.AddCommand(forward, status, stopCmd)

	app := &cobra.Command{Use: "app", Short: "Launch the app, deliver the sign-in link, reset the world"}
	var ao perflab.AppOptions
	appFlags := func(c *cobra.Command) {
		c.Flags().StringVar(&ao.Device, "device", "", "ledger id or alias")
		c.Flags().StringVar(&ao.Lease, "lease", "", "lease token")
	}
	launch := &cobra.Command{Use: "launch", Short: "Wake the phone and start the app", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, t *perflab.Tool, _ []string) (perflab.Result, error) {
			return t.AppLaunch(ctx, ao)
		})}
	link := &cobra.Command{Use: "link", Short: "Run hooks.signInLink and open the link on the phone (the link never prints)", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, t *perflab.Tool, _ []string) (perflab.Result, error) {
			return t.AppLink(ctx, ao)
		})}
	link.Flags().StringVar(&ao.Account, "account", "", "account to sign in")
	link.Flags().StringVar(&ao.Route, "route", "", "route to land on")
	reset := &cobra.Command{Use: "reset", Short: "Run hooks.resetWorld", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, t *perflab.Tool, _ []string) (perflab.Result, error) {
			return t.AppReset(ctx, ao)
		})}
	reset.Flags().StringVar(&ao.World, "world", "", "the world to reset to")
	for _, c := range []*cobra.Command{launch, link, reset} {
		appFlags(c)
	}
	app.AddCommand(launch, link, reset)

	var ro perflab.RunOptions
	runCmd := &cobra.Command{Use: "run <scenario>...", Short: "Measure the project's scenarios on a leased device, variant by variant", Args: cobra.ArbitraryArgs,
		RunE: run(func(ctx context.Context, t *perflab.Tool, args []string) (perflab.Result, error) {
			ro.Scenarios = args
			return t.Run(ctx, ro)
		})}
	runCmd.Flags().StringVar(&ro.Device, "device", "", "ledger id or alias")
	runCmd.Flags().StringVar(&ro.Lease, "lease", "", "lease token")
	runCmd.Flags().StringArrayVar(&ro.Variants, "variant", nil, "[label=]<variant id or native key> (repeatable)")
	runCmd.Flags().BoolVar(&ro.Alternate, "alternate", false, "interleave variants a,b,a,b (the noise rule)")
	runCmd.Flags().IntVar(&ro.Repeat, "repeat", 1, "runs per variant")
	runCmd.Flags().StringVar(&ro.Out, "out", "", "run dir (default: the adapter's out template)")
	runCmd.Flags().StringVar(&ro.Topic, "topic", "", "the {topic} of the out template")
	runCmd.Flags().StringVar(&ro.Resume, "resume", "", "continue a run dir, skipping finished blocks")
	runCmd.Flags().BoolVar(&ro.NoAnalyze, "no-analyze", false, "skip the analysis at the end")
	runCmd.Flags().DurationVar(&ro.Max, "max", 90*time.Minute, "hard limit for the whole run")
	runCmd.Flags().StringVar(&ro.Profile, "profile", "perf", "the build profile the {profile} token names")

	var po perflab.ProbeOptions
	probe := &cobra.Command{Use: "probe <rest|drag|fling|custom>", Short: "A quick device-only measurement: Perfetto around a gesture recipe (iOS: rest)", Args: cobra.ExactArgs(1),
		RunE: run(func(ctx context.Context, t *perflab.Tool, args []string) (perflab.Result, error) {
			po.Kind = args[0]
			return t.Probe(ctx, po)
		})}
	probe.Flags().StringVar(&po.Device, "device", "", "ledger id or alias")
	probe.Flags().StringVar(&po.Lease, "lease", "", "lease token")
	probe.Flags().StringVar(&po.Package, "package", "", "app id (default: the adapter's)")
	probe.Flags().IntVar(&po.Seconds, "seconds", 20, "trace length")
	probe.Flags().StringVar(&po.Label, "label", "", "a name for the screen probed")
	probe.Flags().StringVar(&po.GestureFile, "gesture-file", "", "custom: JSON list of {swipe:[x1,y1,x2,y2,ms]} | {tap:[x,y]} | {sleepMs:n} in 1080x2400 coordinates")
	probe.Flags().StringVar(&po.Out, "out", "", "run dir (default: the adapter's out template)")

	var an perflab.AnalyzeOptions
	var stepCycle string
	analyze := &cobra.Command{Use: "analyze <path>...", Short: "Every number from stored evidence: .trace, sidecars, dumps, .pftrace, run dirs", Args: cobra.ArbitraryArgs,
		RunE: run(func(ctx context.Context, t *perflab.Tool, args []string) (perflab.Result, error) {
			if stepCycle != "" {
				an.StepCycle = strings.Split(stepCycle, ",")
			}
			return t.Analyze(ctx, args, an)
		})}
	analyze.Flags().StringVar(&an.Marks, "marks", "", "marks file (JSON array or JSONL of {label, atMs})")
	analyze.Flags().StringVar(&an.WdioLog, "wdio-log", "", "the runner log (performActions taps)")
	analyze.Flags().StringVar(&stepCycle, "step-cycle", "", "one step name per tap, comma-separated")
	analyze.Flags().DurationVar(&an.TapLag, "tap-lag", 250*time.Millisecond, "host-to-touch delay")
	analyze.Flags().BoolVar(&an.Reread, "reread", false, "ignore cached xctrace exports")
	analyze.Flags().StringVar(&an.SQL, "sql", "", "a .pftrace drill-down preset (trace_processor_shell)")
	analyze.Flags().StringVar(&an.Window, "window", "", "trace-relative seconds a-b for the Time Profiler")
	analyze.Flags().BoolVar(&an.Classify, "classify", false, "bucket the window's main-thread samples by frame family")
	analyze.Flags().BoolVar(&an.Stacks, "stacks", false, "the window's heaviest leaf frames")
	analyze.Flags().StringVar(&an.Package, "package", "", "Android app id")
	analyze.Flags().StringVar(&an.Layer, "layer", "", "Android layer name to read")
	analyze.Flags().Int64Var(&an.VsyncPeriodNs, "vsync-ns", 0, "Android display period (default: from the trace)")
	analyze.Flags().StringVar(&an.RecordLog, "record-log", "", "xctrace record output (TRACE_RUN_ERRORS)")

	var co perflab.CompareOptions
	compare := &cobra.Command{Use: "compare <runDir|label> [<runDir|label>]", Short: "B against A per scenario, step and metric, under the noise rule", Args: cobra.RangeArgs(1, 2),
		RunE: run(func(ctx context.Context, t *perflab.Tool, args []string) (perflab.Result, error) {
			return t.Compare(ctx, args, co)
		})}
	compare.Flags().StringVar(&co.Scenario, "scenario", "", "only this scenario")
	compare.Flags().Float64Var(&co.Threshold, "threshold", 0.15, "relative change that counts")
	compare.Flags().IntVar(&co.MinRuns, "min-runs", 2, "runs per side")
	compare.Flags().StringArrayVar(&co.AllowConfound, "allow-confound", nil, "nativeKey | publicEnvHash | device | inputSource | productionEquivalent")
	compare.Flags().BoolVar(&co.Reread, "reread", false, "ignore cached xctrace exports")

	var rep perflab.ReportOptions
	report := &cobra.Command{Use: "report [<runDir>...]", Short: "The newest result per scenario x device against the adapter's budgets", Args: cobra.ArbitraryArgs,
		RunE: run(func(ctx context.Context, t *perflab.Tool, args []string) (perflab.Result, error) {
			return t.Report(ctx, args, rep)
		})}
	report.Flags().BoolVar(&rep.Gate, "gate", false, "exit 2 on OVER_BUDGET or NOTHING_MEASURED")
	report.Flags().StringVar(&rep.MD, "md", "", "also write the markdown here")
	report.Flags().BoolVar(&rep.Reread, "reread", false, "ignore cached xctrace exports")

	var hz perflab.HazardsOptions
	hazards := &cobra.Command{Use: "hazards [<dir>]", Short: "Static render-cost sweep: loops, layers, lists (no device)", Args: cobra.MaximumNArgs(1),
		RunE: run(func(_ context.Context, t *perflab.Tool, args []string) (perflab.Result, error) {
			if len(args) == 1 {
				hz.Root = args[0]
			}
			return t.Hazards(hz)
		})}
	hazards.Flags().BoolVar(&hz.Gate, "gate", false, "exit 2 on a site the baseline does not list")
	hazards.Flags().StringVar(&hz.Baseline, "baseline", "", "the accepted sites (JSON)")
	hazards.Flags().StringVar(&hz.WriteBaseline, "write-baseline", "", "write the current sites as the baseline")

	var cr perflab.CrashesOptions
	var since string
	crashes := &cobra.Command{Use: "crashes", Short: "Crash reports and error-boundary lines since a time", Args: cobra.NoArgs,
		RunE: run(func(ctx context.Context, t *perflab.Tool, _ []string) (perflab.Result, error) {
			if since != "" {
				at, err := perflabSince(since, t.Now())
				if err != nil {
					return perflab.Result{}, runx.DiagError{Diag: runx.Diagnostic{Code: perflab.DiagUsage, Severity: "error", Detail: err.Error(), Fix: "perflab crashes --device <d> --lease <t> --since 30m --json"}}
				}
				cr.Since = at
			}
			return t.Crashes(ctx, cr)
		})}
	crashes.Flags().StringVar(&cr.Device, "device", "", "ledger id or alias")
	crashes.Flags().StringVar(&cr.Lease, "lease", "", "lease token")
	crashes.Flags().StringVar(&since, "since", "", "RFC3339 time or a duration ago (default 1h)")
	crashes.Flags().StringVar(&cr.Out, "out", "", "where iOS reports are copied")

	return []*cobra.Command{fingerprint, bundle, pack, install, net, app, runCmd, probe, analyze, compare, report, hazards, crashes}
}

// perflabSince reads an RFC3339 time or a duration ago.
func perflabSince(s string, now time.Time) (time.Time, error) {
	if at, err := time.Parse(time.RFC3339, s); err == nil {
		return at, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return time.Time{}, fmt.Errorf("--since %q is neither RFC3339 nor a duration", s)
	}
	return now.Add(-d), nil
}
