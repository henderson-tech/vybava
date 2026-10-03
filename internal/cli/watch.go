package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/henderson-tech/vybava/internal/cmux"
	"github.com/henderson-tech/vybava/internal/fleet"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/watch"
	"github.com/spf13/cobra"
)

// Closed diagnostics of `vybava watch`.
const (
	diagWatchDaemonDown     = "WATCH_DAEMON_DOWN"
	diagWatchInvalid        = "WATCH_REQUEST_INVALID"
	diagWatchUnknownSub     = "WATCH_UNKNOWN_SUBSCRIPTION"
	diagWatchBinaryUnstable = "WATCH_BINARY_UNSTABLE"
)

// watchDeps is everything the watch verbs touch outside the package, so a
// test drives fake probes on a temp socket and renders the agent plan
// without launchd.
type watchDeps struct {
	home       string
	paths      watch.Paths
	probes     func() []watch.Probe
	uid        int
	executable func() (string, error)
	loaded     func(context.Context) bool
	runPlan    func(context.Context, []watch.Step, io.Writer) error
	pathEnv    string
	cwd        func() (string, error)
	// serveTasks register periodic work beside the probes when `serve`
	// starts (engine.Every), or start a follower bound to the daemon's
	// context; empty by default.
	serveTasks []func(context.Context, *watch.Engine)
}

func (rt *runtime) watchApplet() *cobra.Command {
	c := rt.watchCommand("watch")
	c.SetOut(rt.stdout)
	c.SetErr(rt.stderr)
	c.SilenceErrors, c.SilenceUsage = true, true
	c.PersistentFlags().BoolVar(&rt.json, "json", false, "emit the versioned envelope as JSON")
	return c
}

func (rt *runtime) watchCommand(use string) *cobra.Command {
	home, _ := os.UserHomeDir()
	uid := os.Getuid()
	return rt.watchCommandWith(use, watchDeps{
		home:       home,
		paths:      watch.DefaultPaths(home),
		probes:     func() []watch.Probe { return watch.DefaultProbes(watch.ExecRunner) },
		uid:        uid,
		executable: stableExecutable,
		loaded:     func(ctx context.Context) bool { return watch.AgentLoaded(ctx, uid) },
		runPlan:    watch.RunPlan,
		pathEnv:    os.Getenv("PATH"),
		cwd:        os.Getwd,
		serveTasks: []func(context.Context, *watch.Engine){func(ctx context.Context, e *watch.Engine) {
			e.Every("fleet-summary", 15*time.Second, func(ctx context.Context) error {
				return publishFleetSummary(ctx, home)
			})
			publisher, cx := fleetPublisher(home)
			e.Every("fleet-snapshot", 15*time.Second, publisher.Publish)
			e.Every("fleet-codex", time.Minute, func(ctx context.Context) error {
				publisher.RefreshCodex(ctx)
				return publisher.Publish(ctx)
			})
			go publisher.Follow(ctx, cx.Follow, time.Second, func(err error) {
				fmt.Fprintf(os.Stderr, "watch: fleet snapshot: %v\n", err)
			})
		}},
	})
}

// publishFleetSummary writes the file every session's fleet mod reads, so
// ~45 sessions never each run `fleet --json` on a timer.
func publishFleetSummary(ctx context.Context, home string) error {
	snap, _, err := fleet.Read(ctx, fleet.Env{Home: home, Now: time.Now()})
	if err != nil {
		return err
	}
	return fleet.WriteSummary(fleet.SummaryPath(home), fleet.Summarize(snap))
}

// fleetPublisher writes snapshot.json for Fleet.app: Claude rows every 15 s
// and on every cmux waiting event, Codex rows (a ~10 s lsof read) once a
// minute — one reader machine-wide instead of one per app window.
func fleetPublisher(home string) (*fleet.Publisher, cmux.Client) {
	run := func(ctx context.Context, name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).Output()
	}
	cx := cmux.Client{Socket: cmux.SocketPath(os.Getenv, home)}
	return &fleet.Publisher{
		Env:       fleet.Env{Home: home, Cmux: cx},
		Path:      fleet.PublishedPath(home),
		Clock:     time.Now,
		ReadCodex: fleet.CodexUsage(home, run),
	}, cx
}

// stableExecutable is the real binary behind the applet link — what the
// LaunchAgent must run. A `go run` build lives in a temp dir that vanishes,
// and a Homebrew binary in a versioned Caskroom/Cellar dir the next `brew
// upgrade` deletes: there the agent runs Homebrew's bin link instead, which
// every upgrade repoints.
func stableExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	if strings.Contains(exe, "/go-build") {
		return "", runx.DiagError{Diag: runx.Diagnostic{Code: diagWatchBinaryUnstable, Severity: "error",
			Detail: exe + " is a go run build that disappears; the LaunchAgent needs an installed binary",
			Fix:    "vybava watch agent install --bin ~/.local/bin/vybava"}}
	}
	return stableLink(exe, homebrewLinks), nil
}

// homebrewLinks are where Homebrew links vybava on Apple silicon and Intel.
var homebrewLinks = []string{"/opt/homebrew/bin/vybava", "/usr/local/bin/vybava"}

// stableLink returns the first link resolving to exe when exe sits in a
// versioned package dir; any other exe is already stable.
func stableLink(exe string, links []string) string {
	if !strings.Contains(exe, "/Caskroom/") && !strings.Contains(exe, "/Cellar/") {
		return exe
	}
	for _, link := range links {
		if target, err := filepath.EvalSymlinks(link); err == nil && target == exe {
			return link
		}
	}
	return exe
}

func (rt *runtime) watchCommandWith(use string, deps watchDeps) *cobra.Command {
	c := &cobra.Command{
		Use:   use,
		Short: "One machine-wide poller for PRs, CI, Eve, devbox, vitrinka and deployik; sessions subscribe instead of sleep-looping",
		Long: `watch serve (the LaunchAgent vybava.watchd) probes every distinct target once
per interval however many sessions subscribe, under one GitHub budget, and
queues per-session events (change, met, error, expired). add/ls/rm/next talk
to it over ~/.local/state/vybava/watch/watchd.sock; until blocks until a
condition holds and probes directly when no daemon runs. Targets, conditions
and the socket API: docs/watch.md.`,
		Example: `  watch until pr:155 checks-settled           # blocks; one line per change; exit 0 when met
  watch until devbox-run:my-ws idle --timeout 30m
  watch add --session $CLAUDE_CODE_SESSION_ID --target pr:155 --until merged
  watch next --session $CLAUDE_CODE_SESSION_ID --timeout 25s --json
  watch agent install --dry-run`,
	}
	session := func(cmd *cobra.Command) *runx.Session {
		return &runx.Session{Tool: "watch", JSON: rt.json, Verb: cmd.Name(), Stdout: rt.stdout, Stderr: rt.stderr}
	}
	finish := func(s *runx.Session, err error) error {
		if code := s.Finish(watchDiag(err)); code != 0 {
			return runx.ExitError{Code: code}
		}
		return nil
	}
	// emit prints lines for a human or the data for a machine, plus next.
	emit := func(s *runx.Session, data any, lines []string, next ...string) error {
		if rt.json {
			return s.Emit(runx.Envelope{OK: true, Data: data, Next: next})
		}
		for _, l := range lines {
			fmt.Fprintln(rt.stdout, l)
		}
		return s.Emit(runx.Envelope{OK: true, Next: next})
	}
	client := func() *watch.Client { return watch.NewClient(deps.paths.Socket) }

	var tick time.Duration
	var ghCapacity, ghPerHour float64
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Run the daemon in the foreground (the LaunchAgent runs this)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The one-daemon lock comes first: a second daemon reading
			// state.json before it would serve a snapshot the first one
			// is still changing.
			ln, err := watch.Listen(deps.paths.Socket)
			if err != nil {
				return err
			}
			e, err := watch.NewEngine(deps.probes(), watch.Options{
				Store:  watch.Store{Path: deps.paths.State},
				Budget: watch.NewBudget(ghCapacity, ghPerHour),
				Log:    rt.stderr,
			})
			if err != nil {
				return errors.Join(err, ln.Close())
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			for _, register := range deps.serveTasks {
				register(ctx, e)
			}
			return watch.Serve(ctx, e, ln, tick, rt.stderr)
		},
	}
	serve.Flags().DurationVar(&tick, "tick", time.Second, "how often due probes are checked")
	serve.Flags().Float64Var(&ghCapacity, "gh-burst", 300, "GitHub budget: units available at once (a PR probe costs 5)")
	serve.Flags().Float64Var(&ghPerHour, "gh-per-hour", 600, "GitHub budget: units refilled per hour, shared by every PR target")

	status := &cobra.Command{
		Use:   "status",
		Short: "Is the daemon up, and what does it hold",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			h, err := client().Health(cmd.Context())
			if err != nil {
				return finish(s, err)
			}
			line := fmt.Sprintf("watchd up (pid %d): %d subscription(s) on %d target(s), GitHub budget %.0f", h.PID, h.Subscriptions, h.Targets, h.Budget)
			return finish(s, emit(s, h, []string{line}))
		},
	}

	var addReq watch.AddRequest
	add := &cobra.Command{
		Use:   "add",
		Short: "Subscribe a session to a target until a condition holds (returns at once)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			req := addReq
			if req.Dir == "" {
				dir, err := deps.cwd()
				if err != nil {
					return finish(s, err)
				}
				req.Dir = dir
			}
			res, err := client().Add(cmd.Context(), req)
			if err != nil {
				return finish(s, err)
			}
			lines := []string{fmt.Sprintf("subscribed %s: %s until %s", res.Subscription.ID, res.Subscription.Target, res.Subscription.Until)}
			lines = append(lines, eventLines(res.Events)...)
			next := fmt.Sprintf("vybava watch next --session %s --timeout 25s", req.Session)
			return finish(s, emit(s, res, lines, next))
		},
	}
	add.Flags().StringVar(&addReq.Session, "session", "", "the subscriber (a Claude session id)")
	add.Flags().StringVar(&addReq.Target, "target", "", "kind:ref — pr:155, devbox:b, devbox-run:<workspace>, vitrinka:fixit/4759, deployik:luko/production")
	add.Flags().StringVar(&addReq.Until, "until", "", "the condition (per kind; changed and <field>=<value> always work)")
	add.Flags().StringVar(&addReq.Dir, "dir", "", "the directory probes run in (default: the current one; a PR needs a checkout of its repo)")
	add.Flags().DurationVar(&addReq.TTL, "ttl", watch.DefaultTTL, "drop the subscription after this long")
	for _, f := range []string{"session", "target", "until"} {
		_ = add.MarkFlagRequired(f)
	}

	var lsSession string
	ls := &cobra.Command{
		Use:   "ls",
		Short: "List subscriptions (one session's or all) and their targets' last reading",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			l, err := client().List(cmd.Context(), lsSession)
			if err != nil {
				return finish(s, err)
			}
			var lines []string
			for _, sub := range l.Subscriptions {
				lines = append(lines, fmt.Sprintf("%s  %s  %s until %s  (expires %s)", sub.ID, sub.Session, sub.Target, sub.Until, sub.ExpiresAt.Local().Format(time.DateTime)))
			}
			for _, t := range l.Targets {
				line := fmt.Sprintf("  %s ×%d: %s", t.Target, t.Subscribers, t.Summary)
				if t.LastError != "" {
					line += fmt.Sprintf(" — failing ×%d: %s", t.Failures, t.LastError)
				}
				lines = append(lines, line)
			}
			if len(lines) == 0 {
				lines = []string{"no subscriptions"}
			}
			return finish(s, emit(s, l, lines))
		},
	}
	ls.Flags().StringVar(&lsSession, "session", "", "only this session's subscriptions")

	rm := &cobra.Command{
		Use:   "rm <subscription-id>",
		Short: "Drop a subscription",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := session(cmd)
			if err := client().Remove(cmd.Context(), args[0]); err != nil {
				return finish(s, err)
			}
			return finish(s, emit(s, map[string]string{"removed": args[0]}, []string{"removed " + args[0]}))
		},
	}

	var nextSession string
	var nextAfter int64
	var nextTimeout time.Duration
	next := &cobra.Command{
		Use:   "next",
		Short: "Acknowledge events up to --after and long-poll for newer ones (≤ 60 s)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			events, err := client().Events(cmd.Context(), nextSession, nextAfter, nextTimeout)
			if err != nil {
				return finish(s, err)
			}
			after := nextAfter
			for _, ev := range events {
				after = max(after, ev.Seq)
			}
			data := map[string]any{"events": events, "after": after}
			again := fmt.Sprintf("vybava watch next --session %s --after %d --timeout %s", nextSession, after, nextTimeout)
			return finish(s, emit(s, data, eventLines(events), again))
		},
	}
	next.Flags().StringVar(&nextSession, "session", "", "the subscriber")
	next.Flags().Int64Var(&nextAfter, "after", 0, "the last seq already handled (acknowledged and deleted)")
	next.Flags().DurationVar(&nextTimeout, "timeout", 25*time.Second, "how long to wait for an event (capped at 60s)")
	_ = next.MarkFlagRequired("session")

	var untilDir string
	var untilTimeout time.Duration
	until := &cobra.Command{
		Use:   "until <kind:ref> <condition>",
		Short: "Block until the condition holds; one line per change (--json: one event object per line). Exit 0 met, 124 timeout",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := untilDir
			if dir == "" {
				d, err := deps.cwd()
				if err != nil {
					return err
				}
				dir = d
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			code, err := watch.Until(ctx, client(), deps.probes(), watch.UntilOptions{
				Target: args[0], Until: args[1], Dir: dir, Timeout: untilTimeout, JSON: rt.json, Out: rt.stdout, Err: rt.stderr,
			})
			if err != nil {
				return err
			}
			if code != 0 {
				return runx.ExitError{Code: code}
			}
			return nil
		},
	}
	until.Flags().StringVar(&untilDir, "dir", "", "the directory probes run in (default: the current one)")
	until.Flags().DurationVar(&untilTimeout, "timeout", 0, "give up after this long with exit 124 (0 waits until met)")

	agent := &cobra.Command{Use: "agent", Short: "Install or remove the vybava.watchd LaunchAgent"}
	var dryRun bool
	var bin string
	plan := func(cmd *cobra.Command, steps []watch.Step) error {
		s := session(cmd)
		var lines []string
		for _, st := range steps {
			lines = append(lines, "  "+st.Command()+"    # "+st.Why)
		}
		if dryRun {
			lines = append([]string{"would run:"}, lines...)
			return finish(s, emit(s, map[string]any{"plan": steps, "applied": false}, lines))
		}
		if err := deps.runPlan(cmd.Context(), steps, rt.stderr); err != nil {
			return finish(s, err)
		}
		lines = append([]string{"ran:"}, lines...)
		return finish(s, emit(s, map[string]any{"plan": steps, "applied": true}, lines, "vybava watch status"))
	}
	install := &cobra.Command{
		Use:   "install",
		Short: "Write ~/Library/LaunchAgents/vybava.watchd.plist and (re)start it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			exe := bin
			if exe == "" {
				var err error
				if exe, err = deps.executable(); err != nil {
					return finish(session(cmd), err)
				}
			}
			return plan(cmd, watch.AgentInstallPlan(deps.home, deps.uid, exe, deps.pathEnv, deps.loaded(cmd.Context())))
		},
	}
	install.Flags().StringVar(&bin, "bin", "", "the vybava binary the agent runs (default: this one, symlinks resolved)")
	uninstall := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop the daemon and remove the LaunchAgent (subscriptions stay on disk)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return plan(cmd, watch.AgentUninstallPlan(deps.home, deps.uid, deps.loaded(cmd.Context())))
		},
	}
	agent.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "print the plan without running it")
	agent.AddCommand(install, uninstall)

	c.AddCommand(serve, status, add, ls, rm, next, until, agent)
	return c
}

func eventLines(events []watch.Event) []string {
	var lines []string
	for _, ev := range events {
		line := fmt.Sprintf("#%d %s %s %s", ev.Seq, ev.Kind, ev.Target, ev.Summary)
		if ev.Error != "" {
			line = fmt.Sprintf("#%d %s %s %s", ev.Seq, ev.Kind, ev.Target, ev.Error)
		}
		lines = append(lines, strings.TrimSpace(line))
	}
	return lines
}

// watchDiag gives the daemon's refusals their closed codes.
func watchDiag(err error) error {
	var apiErr *watch.APIError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, watch.ErrDaemonDown):
		return runx.DiagError{Diag: runx.Diagnostic{Code: diagWatchDaemonDown, Severity: "error", Detail: err.Error(),
			Fix: "vybava watch agent install"}}
	case errors.As(err, &apiErr) && apiErr.Status == 404:
		return runx.DiagError{Diag: runx.Diagnostic{Code: diagWatchUnknownSub, Severity: "error", Detail: apiErr.Message, Fix: "vybava watch ls"}}
	case errors.As(err, &apiErr) && apiErr.Status < 500:
		return runx.DiagError{Diag: runx.Diagnostic{Code: diagWatchInvalid, Severity: "error", Detail: apiErr.Message, Fix: "vybava watch --help"}}
	}
	return err
}
