package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/tokentime"
	"github.com/henderson-tech/vybava/internal/transcripts"
	"github.com/spf13/cobra"
)

// tokentime diagnostic codes.
const (
	diagIndexBusy     = "INDEX_BUSY"
	diagIndexPartial  = "INDEX_PARTIAL"
	diagFileError     = "FILE_ERROR"
	diagStaleTail     = "STALE_TAIL"
	diagPriceGap      = "PRICE_INCOMPLETE"
	diagInterrupted   = "INDEX_INTERRUPTED"
	diagUnpricedModel = "UNPRICED_MODEL"
	diagBadFlag       = "BAD_FLAG"
	diagUnknownProj   = "UNKNOWN_PROJECT"
	diagNoStore       = "NO_STORE"
	diagStaleSchema   = "STALE_SCHEMA"
)

func (rt *runtime) tokentimeApplet() *cobra.Command {
	cmd := rt.tokentimeCommand("tokentime")
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(rt.stdout)
	cmd.SetErr(rt.stderr)
	cmd.PersistentFlags().BoolVar(&rt.json, "json", false, "emit machine-readable output")
	return cmd
}

// tokentimeCommand wires the token-accounting verbs; the domain lives in
// internal/tokentime. Every verb emits one runx envelope.
func (rt *runtime) tokentimeCommand(use string) *cobra.Command {
	root := &cobra.Command{
		Use:   use,
		Short: "Where your AI tokens went — per project, model and hour, for every Claude Code session and Codex thread",
		Long: "tokentime indexes Claude Code transcripts (~/.claude/projects) and Codex rollouts (~/.codex)\n" +
			"incrementally into permanent hour × project × model buckets, then rolls them up:\n" +
			"  tokentime index            catch up on everything written since the last pass\n" +
			"  tokentime rollup --json    days, hours, projects, models, lifetime — with API-equivalent usd\n" +
			"  tokentime project --from D --to D --json   this repository (or --project NAME) across a range of local days\n" +
			"  tokentime status           what is indexed, what is pending\n" +
			"  tokentime prices           the per-model price table and its override file",
	}
	var stateDir, claudeRoot, codexDir string
	root.PersistentFlags().StringVar(&stateDir, "state-dir", "", "state directory (default ~/.local/share/vybava/tokentime)")
	root.PersistentFlags().StringVar(&claudeRoot, "claude-root", "", "Claude Code projects directory (default ~/.claude/projects)")
	root.PersistentFlags().StringVar(&codexDir, "codex-dir", "", "Codex directory holding sessions/ (default ~/.codex)")

	session := func(cmd *cobra.Command) *runx.Session {
		return &runx.Session{Tool: "tokentime", JSON: rt.json, Verb: cmd.Name(), Stdout: rt.stdout, Stderr: rt.stderr}
	}
	finish := func(s *runx.Session, data any, diags []runx.Diagnostic, next []string, err error) error {
		env := runx.Envelope{OK: err == nil, Verb: s.Verb, Data: data, Diagnostics: diags, Next: next}
		if env.Diagnostics == nil {
			env.Diagnostics = []runx.Diagnostic{}
		}
		if env.Next == nil {
			env.Next = []string{}
		}
		var diagErr runx.DiagError
		if errors.As(err, &diagErr) {
			env.Diagnostics = append(env.Diagnostics, diagErr.Diag)
			if diagErr.Diag.Fix != "" {
				env.Next = append(env.Next, diagErr.Diag.Fix)
			}
		} else if err != nil {
			env.Diagnostics = append(env.Diagnostics, runx.Diagnostic{Code: runx.DiagInfraError, Severity: "error", Detail: err.Error()})
		}
		if emitErr := s.Emit(env); emitErr != nil {
			return emitErr
		}
		if code := s.Finish(err); code != 0 {
			return runx.ExitError{Code: code}
		}
		return nil
	}
	paths := func() (state string, opts tokentime.Options, err error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", opts, err
		}
		state = stateDir
		if state == "" {
			if state, err = tokentime.DefaultStateDir(); err != nil {
				return "", opts, err
			}
		}
		opts.ClaudeRoot, opts.CodexDir = claudeRoot, codexDir
		if opts.ClaudeRoot == "" {
			opts.ClaudeRoot = filepath.Join(home, ".claude", "projects")
		}
		if opts.CodexDir == "" {
			opts.CodexDir = filepath.Join(home, ".codex")
		}
		for _, p := range []*string{&state, &opts.ClaudeRoot, &opts.CodexDir} {
			if *p, err = expandHome(*p); err != nil {
				return "", opts, err
			}
		}
		return state, opts, nil
	}
	// storeErr names a store no read can serve: never indexed, or older than
	// its queries. Only an index pass creates or migrates one.
	storeErr := func(err error) error {
		switch {
		case errors.Is(err, tokentime.ErrNoStore):
			return runx.DiagError{Diag: runx.Diagnostic{Code: diagNoStore, Severity: "error",
				Detail: err.Error() + " — nothing has been indexed yet", Fix: "tokentime index"}}
		case errors.Is(err, tokentime.ErrStaleSchema):
			return runx.DiagError{Diag: runx.Diagnostic{Code: diagStaleSchema, Severity: "error",
				Detail: err.Error() + " — one index pass migrates it", Fix: "tokentime index"}}
		}
		return err
	}
	priceDiags := func(p tokentime.Prices) []runx.Diagnostic {
		if len(p.Incomplete) == 0 {
			return nil
		}
		return []runx.Diagnostic{{Code: diagPriceGap, Severity: "warning",
			Detail: "override rows for models without a built-in price leave rates out, which price at $0: " + strings.Join(p.Incomplete, "; "),
			Fix:    "complete them in " + p.OverridePath}}
	}
	unpricedDiags := func(models []string, p tokentime.Prices) []runx.Diagnostic {
		if len(models) == 0 {
			return nil
		}
		return []runx.Diagnostic{{Code: diagUnpricedModel, Severity: "warning",
			Detail: "no price for " + strings.Join(models, ", ") + "; their tokens are left out of every usd figure",
			Fix:    "add them to " + p.OverridePath}}
	}
	// A pass stopped by SIGTERM or SIGINT commits what it read and exits; the
	// next pass continues from there.
	stoppable := func() (context.Context, context.CancelFunc) {
		return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	}
	interrupted := runx.DiagError{Diag: runx.Diagnostic{Code: diagInterrupted, Severity: "error",
		Detail: "stopped by a signal; everything read so far is committed", Fix: "tokentime index"}}
	indexDiags := func(r tokentime.IndexReport) ([]runx.Diagnostic, []string) {
		var diags []runx.Diagnostic
		var next []string
		for _, e := range r.FileErrors {
			diags = append(diags, runx.Diagnostic{Code: diagFileError, Severity: "warning", Detail: e})
		}
		if n := len(r.StaleTails); n > 0 {
			shown := r.StaleTails[:min(n, 3)]
			diags = append(diags, runx.Diagnostic{Code: diagStaleTail, Severity: "info",
				Detail: fmt.Sprintf("%d file(s) end in an unfinished record older than 10 minutes (%s, not counted as pending): %s",
					n, humanBytes(r.StaleTailBytes), strings.Join(shown, ", "))})
		}
		if r.PendingBytes > 0 {
			diags = append(diags, runx.Diagnostic{Code: diagIndexPartial, Severity: "info",
				Detail: fmt.Sprintf("%s still unread; the next pass continues where this one stopped", humanBytes(r.PendingBytes)), Fix: "tokentime index"})
			next = append(next, "tokentime index")
		}
		return diags, next
	}

	var budget string
	index := &cobra.Command{
		Use: "index", Short: "Catch up on every transcript and rollout written since the last pass", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			limit, err := parseBytes(budget)
			if err != nil {
				return finish(s, nil, nil, nil, runx.DiagError{Diag: runx.Diagnostic{Code: diagBadFlag, Severity: "error", Detail: err.Error(), Fix: "tokentime index --budget 256MiB"}})
			}
			state, opts, err := paths()
			if err != nil {
				return finish(s, nil, nil, nil, err)
			}
			store, err := tokentime.Open(state)
			if err != nil {
				return finish(s, nil, nil, nil, err)
			}
			defer store.Close()
			opts.Budget = limit
			ctx, stop := stoppable()
			defer stop()
			opts.Context = ctx
			report, err := store.Index(opts)
			if errors.Is(err, context.Canceled) {
				return finish(s, report, nil, nil, interrupted)
			}
			if errors.Is(err, tokentime.ErrBusy) {
				return finish(s, nil, nil, nil, runx.DiagError{Diag: runx.Diagnostic{Code: diagIndexBusy, Severity: "error", Detail: err.Error(), Fix: "tokentime status"}})
			}
			if err != nil {
				return finish(s, nil, nil, nil, err)
			}
			diags, next := indexDiags(report)
			return finish(s, report, diags, next, nil)
		},
	}
	index.Flags().StringVar(&budget, "budget", "", "stop after reading this much (e.g. 256MiB); default reads everything pending")

	var days, hours int
	var indexBudget string
	var noIndex bool
	rollup := &cobra.Command{
		Use: "rollup", Short: "Days, hours, projects and models with API-equivalent usd (runs a bounded index pass first)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			limit, err := parseBytes(indexBudget)
			if err != nil {
				return finish(s, nil, nil, nil, runx.DiagError{Diag: runx.Diagnostic{Code: diagBadFlag, Severity: "error", Detail: err.Error(), Fix: "tokentime rollup --index-budget 64MiB"}})
			}
			state, opts, err := paths()
			if err != nil {
				return finish(s, nil, nil, nil, err)
			}
			store, err := tokentime.Open(state)
			if err != nil {
				return finish(s, nil, nil, nil, err)
			}
			defer store.Close()
			var diags []runx.Diagnostic
			var next []string
			if !noIndex {
				opts.Budget = limit
				ctx, stop := stoppable()
				defer stop()
				opts.Context = ctx
				report, err := store.Index(opts)
				switch {
				case errors.Is(err, context.Canceled):
					return finish(s, nil, nil, nil, interrupted)
				case errors.Is(err, tokentime.ErrBusy):
					// Another pass is catching up; read what it has committed.
					diags = append(diags, runx.Diagnostic{Code: diagIndexBusy, Severity: "info", Detail: err.Error()})
				case err != nil:
					return finish(s, nil, nil, nil, err)
				default:
					diags, next = indexDiags(report)
				}
			}
			// With the lock busy (or --no-index) the store is served as the
			// last pass left it — an older schema too, when its queries still
			// run — and never migrated here.
			out, err := store.Rollup(tokentime.RollupOptions{Days: days, Hours: hours})
			if err != nil {
				return finish(s, nil, diags, next, storeErr(err))
			}
			prices, _ := tokentime.LoadPrices(state)
			diags = append(diags, priceDiags(prices)...)
			diags = append(diags, unpricedDiags(out.Unpriced, prices)...)
			return finish(s, out, diags, next, nil)
		},
	}
	rollup.Flags().IntVar(&days, "days", 90, "local days to roll up, today included")
	rollup.Flags().IntVar(&hours, "hours", 336, "local hours to roll up, this hour included")
	rollup.Flags().StringVar(&indexBudget, "index-budget", "64MiB", "bound the index pass run first")
	rollup.Flags().BoolVar(&noIndex, "no-index", false, "roll up what is already indexed")

	var projRoot, projName, projFrom, projTo, projBucket string
	project := &cobra.Command{
		Use: "project", Short: "One project across a range of local days: totals, models, sessions and a zero-filled series (read-only, no index pass)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			badFlag := func(detail string) error {
				return finish(s, nil, nil, nil, runx.DiagError{Diag: runx.Diagnostic{Code: diagBadFlag, Severity: "error", Detail: detail,
					Fix: "tokentime project --project <name> --from YYYY-MM-DD --to YYYY-MM-DD --json"}})
			}
			// Every flag is validated before the store is opened: a bad one is
			// BAD_FLAG and writes nothing.
			byRoot, byName := cmd.Flags().Changed("root"), cmd.Flags().Changed("project")
			if byRoot && byName {
				return badFlag("--root and --project both select the project; pass one")
			}
			if projFrom == "" || projTo == "" {
				return badFlag("--from and --to are required")
			}
			rng, err := tokentime.ParseRange(projFrom, projTo, tokentime.Bucket(projBucket))
			if err != nil {
				return badFlag(err.Error())
			}
			sel := tokentime.ProjectOptions{Name: projName, Range: rng}
			switch {
			case byName:
				if projName == "" {
					return badFlag(`--project is a name the rollup shows ("unknown" for responses recorded without a cwd)`)
				}
			case byRoot:
				// An explicit empty --root is the rollup's "unknown" project:
				// responses recorded without a cwd.
				if sel.Root, err = expandHome(projRoot); err != nil {
					return finish(s, nil, nil, nil, err)
				}
				if projRoot != "" && !filepath.IsAbs(sel.Root) {
					return badFlag(`--root is an absolute repository root, or "" for the rollup's unknown project`)
				}
			default:
				// Neither: the repository the cwd is in, by the rule the
				// indexer files every response under — a worktree is its repo.
				cwd, err := os.Getwd()
				if err != nil {
					return badFlag("no --project or --root, and the current directory is unreadable: " + err.Error())
				}
				root, inRepo := transcripts.GitRoot(cwd)
				if !inRepo {
					return badFlag(fmt.Sprintf("no --project or --root, and %s is not inside a git repository", cwd))
				}
				sel.Root = root
			}
			state, _, err := paths()
			if err != nil {
				return finish(s, nil, nil, nil, err)
			}
			// Read-only: never creates the directory or database, never migrates.
			store, err := tokentime.OpenReadOnly(state)
			if err != nil {
				return finish(s, nil, nil, nil, storeErr(err))
			}
			defer store.Close()
			out, err := store.Project(sel)
			switch {
			case errors.Is(err, tokentime.ErrUnknownProject):
				return finish(s, nil, nil, nil, runx.DiagError{Diag: runx.Diagnostic{Code: diagUnknownProj, Severity: "error",
					Detail: err.Error() + " — projects are listed by the rollup", Fix: "tokentime rollup --json --no-index"}})
			case err != nil:
				return finish(s, nil, nil, nil, err)
			}
			prices, _ := tokentime.LoadPrices(state)
			return finish(s, out, append(priceDiags(prices), unpricedDiags(out.Unpriced, prices)...), nil, nil)
		},
	}
	project.Flags().StringVar(&projRoot, "root", "", `the project's repository root, as the rollup reports it ("" for its unknown project); default: the repository of the current directory`)
	project.Flags().StringVar(&projName, "project", "", `the project's name as the rollup shows it (e.g. FixIt, ADF/forge, unknown)`)
	project.Flags().StringVar(&projFrom, "from", "", "first local day, YYYY-MM-DD")
	project.Flags().StringVar(&projTo, "to", "", "last local day, YYYY-MM-DD (included)")
	project.Flags().StringVar(&projBucket, "bucket", "", "series bucket: hour, day or month (default hour for one day, month past 62 days, else day)")

	status := &cobra.Command{
		Use: "status", Short: "What is indexed and what is still pending", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			state, opts, err := paths()
			if err != nil {
				return finish(s, nil, nil, nil, err)
			}
			store, err := tokentime.Open(state)
			if err != nil {
				return finish(s, nil, nil, nil, err)
			}
			defer store.Close()
			st, err := store.Status()
			if err != nil {
				return finish(s, nil, nil, nil, storeErr(err))
			}
			st.ClaudeRoot, st.CodexDir = opts.ClaudeRoot, opts.CodexDir
			var next []string
			if st.PendingBytes > 0 || st.LastIndexAt == "" {
				next = append(next, "tokentime index")
			}
			return finish(s, st, nil, next, nil)
		},
	}

	prices := &cobra.Command{
		Use: "prices", Short: "The per-model price table (USD per million tokens) and its override file", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			state, _, err := paths()
			if err != nil {
				return finish(s, nil, nil, nil, err)
			}
			p, err := tokentime.LoadPrices(state)
			if err != nil {
				return finish(s, nil, nil, nil, err)
			}
			models := make([]string, 0, len(p.Table))
			for m := range p.Table {
				models = append(models, m)
			}
			sort.Strings(models)
			type priced struct {
				Model string `json:"model"`
				tokentime.Price
			}
			rows := make([]priced, 0, len(models))
			for _, m := range models {
				rows = append(rows, priced{Model: m, Price: p.Table[m]})
			}
			overridden := p.Overridden
			if overridden == nil {
				overridden = []string{}
			}
			return finish(s, map[string]any{"asOf": tokentime.PricesAsOf, "overridePath": p.OverridePath, "overridden": overridden, "models": rows}, priceDiags(p), nil, nil)
		},
	}

	root.AddCommand(index, rollup, project, status, prices)
	return root
}

// parseBytes reads "", "0", "4096", "64MiB", "1GiB" (KiB/MiB/GiB, also KB/MB/GB as binary).
func parseBytes(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	units := []struct {
		suffix string
		scale  int64
	}{{"gib", 1 << 30}, {"mib", 1 << 20}, {"kib", 1 << 10}, {"gb", 1 << 30}, {"mb", 1 << 20}, {"kb", 1 << 10}, {"b", 1}}
	lower := strings.ToLower(s)
	scale := int64(1)
	for _, u := range units {
		if strings.HasSuffix(lower, u.suffix) {
			lower, scale = strings.TrimSpace(strings.TrimSuffix(lower, u.suffix)), u.scale
			break
		}
	}
	n, err := strconv.ParseInt(lower, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%q is not a byte size (e.g. 64MiB)", s)
	}
	return n * scale, nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
