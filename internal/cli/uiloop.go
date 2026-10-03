package cli

import (
	"context"
	"errors"
	"os"
	"time"

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

	var stage string
	doctorCmd := &cobra.Command{
		Use:   "doctor",
		Short: "Preflight a stage: check's findings, the state contract, app reachability and the newest pass — each with its fix",
		Long: "doctor runs every check a review-loop stage depends on and reports each as\n" +
			"ok, warn, fail or skip with a fix. ok is false (exit 2) iff a check fails; a\n" +
			"check the --for stage does not need (check's harness and manifest, app\n" +
			"reachability: review and fix run neither) warns instead of failing.",
		Args: cobra.NoArgs,
		RunE: run(func(t *uiloop.Tool) (uiloop.Result, error) {
			return t.Doctor(context.Background(), uiloop.DoctorOptions{For: stage})
		}),
	}
	doctorCmd.Flags().StringVar(&stage, "for", "", "the stage to preflight: capture, review, fix or verify (default: every stage)")

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
		passGiven                     bool
		dryRun, forcePublish, noDelta bool
		follow                        bool
		from                          string
		interval, untilIdle           time.Duration
		// owner names a lease's holder (docs/uiloop.md "Pass leases"); every
		// verb has its own ttl, since a shared flag variable takes the last default.
		owner                          string
		publishTTL, synthTTL, claimTTL time.Duration
		claim                          int
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
			if err := uiloop.CheckPassFlag(pass, passGiven); err != nil {
				return uiloop.Result{}, err
			}
			return t.Run(context.Background(), opts)
		}),
	}
	runCmd.Flags().StringVar(&apps, "app", "", "only these apps (comma-separated)")
	runCmd.Flags().StringVar(&only, "only", "", "only these screen ids or areas (comma-separated; a trailing * is a prefix)")
	runCmd.Flags().StringVar(&viewports, "viewports", "", "only these viewports (comma-separated)")
	runCmd.Flags().StringVar(&themes, "themes", "", "only these themes: light,dark")
	runCmd.Flags().BoolVar(&opts.Selection.Destructive, "destructive", false, "also shoot destructive recipes (last)")
	runCmd.Flags().BoolVar(&opts.Selection.Resume, "resume", false, "finish a cut-short pass: keep ok/unreachable shots, retake the rest")
	runCmd.Flags().IntVar(&pass, "pass", 0, "pass number (default: the next one; with --resume the latest with shots, the pass state reads)")
	runCmd.Flags().IntVar(&opts.Workers, "workers", 2, "Playwright workers")
	runCmd.Flags().IntVar(&opts.BuildWait, "build-wait", 300, "seconds a shot waits for a red dev server to turn green")
	runCmd.Flags().BoolVar(&opts.Print, "print", false, "write run.json and print the command without running it (the capture lease ends with this command, so a printed line runs unleased; --wrap holds it)")
	runCmd.Flags().StringVar(&opts.Wrap, "wrap", "", `run the command through another; {cmd} is the quoted capture command (e.g. "devbox run -- {cmd}")`)

	splitCmd := &cobra.Command{
		Use:   "split",
		Short: "Plan a pass's vitrinka sets: one per area (so one board), sectioned by viewport × theme",
		Args:  cobra.NoArgs,
		RunE: run(func(t *uiloop.Tool) (uiloop.Result, error) {
			if err := uiloop.CheckPassFlag(pass, passGiven); err != nil {
				return uiloop.Result{}, err
			}
			return t.Split(uiloop.SplitOptions{Pass: pass, Areas: uiloop.SplitList(areas)})
		}),
	}
	splitCmd.Flags().IntVar(&pass, "pass", 0, "pass number (default: the latest)")
	splitCmd.Flags().StringVar(&areas, "areas", "", "only these areas (comma-separated)")

	publishCmd := &cobra.Command{
		Use:   "publish",
		Short: "Adopt and push each planned set with the vitrinka CLI (retried on failure); --follow publishes beside a running capture",
		Long: "publish adopts each planned set and pushes it. With --follow it runs on the\n" +
			"Mac beside a capture on a box: every --interval it rsyncs the pass back\n" +
			"(--from, default publish.from + /<out>/pass-<n>/), publishes each shot that\n" +
			"became final, and stops once the run's done.json has arrived and nothing\n" +
			"new has for --until-idle.",
		Args: cobra.NoArgs,
		RunE: run(func(t *uiloop.Tool) (uiloop.Result, error) {
			if err := uiloop.CheckPassFlag(pass, passGiven); err != nil {
				return uiloop.Result{}, err
			}
			if follow {
				if sets != "" || forcePublish || dryRun {
					return uiloop.Result{}, uiloop.SelectionError("--follow publishes whole sets as they fill: drop --sets, --force and --dry-run", "vybava ui-loop publish --follow")
				}
				return t.Follow(context.Background(), uiloop.FollowOptions{
					Pass: pass, Areas: uiloop.SplitList(areas), From: from, Interval: interval, UntilIdle: untilIdle, Retries: retries,
					Owner: owner, TTL: publishTTL,
				})
			}
			return t.Publish(context.Background(), uiloop.PublishOptions{
				Pass: pass, Areas: uiloop.SplitList(areas), Sets: uiloop.SplitList(sets),
				Retries: retries, Force: forcePublish, DryRun: dryRun, Owner: owner, TTL: publishTTL,
			})
		}),
	}
	publishCmd.Flags().IntVar(&pass, "pass", 0, "pass number (default: the latest with shots; with --follow the running capture's, else the latest)")
	publishCmd.Flags().StringVar(&areas, "areas", "", "only these areas (comma-separated)")
	publishCmd.Flags().StringVar(&sets, "sets", "", "only these set keys (comma-separated)")
	publishCmd.Flags().IntVar(&retries, "retries", 3, "push attempts per set")
	publishCmd.Flags().BoolVar(&forcePublish, "force", false, "re-push sets the index records as pushed")
	publishCmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the vitrinka commands without running them")
	publishCmd.Flags().BoolVar(&follow, "follow", false, "publish beside a running capture: fetch, publish what became final, repeat")
	publishCmd.Flags().StringVar(&from, "from", "", "--follow: the pass directory on the box, user@host:path (default: publish.from + /<out>/pass-<n>/)")
	publishCmd.Flags().DurationVar(&interval, "interval", 30*time.Second, "--follow: time between fetches")
	publishCmd.Flags().DurationVar(&untilIdle, "until-idle", 10*time.Minute, "--follow: stop this long after the last new shot once the run is done")
	publishCmd.Flags().StringVar(&owner, "owner", "", "name this publisher in the pass's publish lease, held until it exits (default: ui-loop publish)")
	publishCmd.Flags().DurationVar(&publishTTL, "ttl", uiloop.DefaultLeaseTTL, "the publish lease's ttl (--follow renews it every tick)")

	scoreboardCmd := &cobra.Command{
		Use:   "scoreboard",
		Short: "Fold a pass's records and review backlog into scoreboard.json + scoreboard.md, with a delta",
		Args:  cobra.NoArgs,
		RunE: run(func(t *uiloop.Tool) (uiloop.Result, error) {
			if err := uiloop.CheckPassFlag(pass, passGiven); err != nil {
				return uiloop.Result{}, err
			}
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

	var (
		stageCap, batchSize, maxLanes     int
		primitives                        string
		splitID, stallID, blockID, reason string
	)
	withPass := func(verb func(*uiloop.Tool) (uiloop.Result, error)) func(*cobra.Command, []string) error {
		return run(func(t *uiloop.Tool) (uiloop.Result, error) {
			if err := uiloop.CheckPassFlag(pass, passGiven); err != nil {
				return uiloop.Result{}, err
			}
			return verb(t)
		})
	}
	stateCmd := &cobra.Command{
		Use:   "state",
		Short: "A pass's state as counts (shots, screens, published, review batches, backlog, checkpoints) and the next stage",
		Args:  cobra.NoArgs,
		RunE: withPass(func(t *uiloop.Tool) (uiloop.Result, error) {
			return t.State(uiloop.StateOptions{Pass: pass, Cap: stageCap, Primitives: uiloop.SplitList(primitives)})
		}),
	}
	stateCmd.Flags().IntVar(&pass, "pass", 0, "pass number (default: the latest)")
	stateCmd.Flags().IntVar(&stageCap, "cap", 6, "the pass cap the next stage respects")
	stateCmd.Flags().StringVar(&primitives, "primitives", "", "directory prefixes that hold shared primitives (comma-separated; default: uiLoop.primitives)")

	batchesCmd := &cobra.Command{
		Use:   "batches",
		Short: "Plan the pass's review batches (per-screen digests; screens whose pixels did not move carry the previous review) and persist them to review/batches.json; --stall, --split and --block act on one batch a reviewer stalled on",
		Args:  cobra.NoArgs,
		RunE: withPass(func(t *uiloop.Tool) (uiloop.Result, error) {
			return t.Batches(uiloop.BatchesOptions{Pass: pass, Size: batchSize, Areas: uiloop.SplitList(areas), Claim: claim, Owner: owner, TTL: claimTTL,
				Split: splitID, Stall: stallID, Block: blockID, Reason: reason})
		}),
	}
	batchesCmd.Flags().IntVar(&pass, "pass", 0, "pass number (default: the latest with shots)")
	batchesCmd.Flags().IntVar(&batchSize, "size", 0, "screens per batch (default: the size batches.json was made with, else 14)")
	batchesCmd.Flags().StringVar(&areas, "areas", "", "return only these areas' batches (the file always holds every batch)")
	batchesCmd.Flags().IntVar(&claim, "claim", 0, "claim up to N left batches for --owner (batch-<id> leases) and return them as claimed")
	batchesCmd.Flags().StringVar(&owner, "owner", "", "the run the claims are for (required with --claim)")
	batchesCmd.Flags().DurationVar(&claimTTL, "ttl", uiloop.DefaultClaimTTL, "how long a claim holds unless its owner claims it again")
	batchesCmd.Flags().StringVar(&stallID, "stall", "", "count a reviewer stall of this batch in review/attempts/<id>.json")
	batchesCmd.Flags().StringVar(&splitID, "split", "", "halve this batch into <id>.1 and <id>.2 and release its claim; with --claim, claim among its parts")
	batchesCmd.Flags().StringVar(&blockID, "block", "", "take this batch out of the review (needs --reason); its unread screens are listed unreviewed")
	batchesCmd.Flags().StringVar(&reason, "reason", "", "why --block blocks the batch (merge-review lists its screens as \"<id> (stalled: <reason>)\")")

	mergeCmd := &cobra.Command{
		Use:   "merge-review",
		Short: "Merge review/raw/*.json and the previous backlog into review/backlog.draft.json; list what needs judgement",
		Args:  cobra.NoArgs,
		RunE: withPass(func(t *uiloop.Tool) (uiloop.Result, error) {
			return t.MergeReview(uiloop.MergeReviewOptions{Pass: pass, Owner: owner, TTL: synthTTL})
		}),
	}
	mergeCmd.Flags().IntVar(&pass, "pass", 0, "pass number (default: the latest with shots)")
	mergeCmd.Flags().StringVar(&owner, "owner", "", "hold the pass's synth lease for this run past the command, for the synthesis (default: only while it runs)")
	mergeCmd.Flags().DurationVar(&synthTTL, "ttl", uiloop.DefaultLeaseTTL, "how long an --owner's synth lease holds")

	lanesCmd := &cobra.Command{
		Use:   "lanes",
		Short: "Plan the fix lanes by directory ownership from review/backlog.json; write fix/lanes.json",
		Args:  cobra.NoArgs,
		RunE: withPass(func(t *uiloop.Tool) (uiloop.Result, error) {
			return t.Lanes(uiloop.LanesOptions{Pass: pass, Primitives: uiloop.SplitList(primitives), Max: maxLanes})
		}),
	}
	lanesCmd.Flags().IntVar(&pass, "pass", 0, "pass number (default: the latest with shots)")
	lanesCmd.Flags().StringVar(&primitives, "primitives", "", "directory prefixes that hold shared primitives (comma-separated; default: uiLoop.primitives)")
	lanesCmd.Flags().IntVar(&maxLanes, "max", 4, "lanes per phase")

	checkpointsCmd := &cobra.Command{
		Use:   "checkpoints",
		Short: "List the fix checkpoints that count for the pass, one per item, as state and lanes count them",
		Args:  cobra.NoArgs,
		RunE: withPass(func(t *uiloop.Tool) (uiloop.Result, error) {
			return t.Checkpoints(uiloop.CheckpointsOptions{Pass: pass})
		}),
	}
	checkpointsCmd.Flags().IntVar(&pass, "pass", 0, "pass number (default: the latest with shots)")

	// --pass is shared by the pass verbs; record whether it was given so an
	// explicit 0 is refused instead of read as "not given".
	for _, c := range []*cobra.Command{runCmd, splitCmd, publishCmd, scoreboardCmd, stateCmd, batchesCmd, mergeCmd, lanesCmd, checkpointsCmd} {
		c.PreRun = func(cmd *cobra.Command, _ []string) { passGiven = cmd.Flags().Changed("pass") }
	}

	command.AddCommand(initCmd, syncCmd, checkCmd, doctorCmd, mapCmd, runCmd, splitCmd, publishCmd, scoreboardCmd, stateCmd, batchesCmd, mergeCmd, lanesCmd, checkpointsCmd)
	return command
}
