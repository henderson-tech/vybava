package cli

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/henderson-tech/vybava/internal/blip"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/spf13/cobra"
)

func (rt *runtime) blipApplet() *cobra.Command {
	command := rt.blipCommand("blip")
	command.SetOut(rt.stdout)
	command.SetErr(rt.stderr)
	return command
}

var blipGlobalVerbs = map[string]bool{"up": true, "ls": true, "down": true, "serve": true, "help": true}
var blipNameVerbs = map[string]bool{"set": true, "ok": true, "cut": true, "status": true, "log": true, "record": true, "authz": true}

const blipUsage = `blip up <name> --listen <addr> --to http://host:port | tcp://host:port
blip <name> set delay 800ms [--jitter 400ms] | drop | error 503 [--body '{}'] | timeout | slow 20kbps | flap 5s/10s
   scoping on any set: --match '/api/orders*' --method POST --after 3 --for 30s --rate 0.5
blip <name> cut | ok | status | log [--tail] [--last N]
blip <name> record on|off|clear
blip <name> authz --as none|'header:Authorization=Bearer …'|'cookie:sid=…'|env:VAR [--expect 401,403,404] [--only <glob>] [--exclude <glob>] [--mutations]
blip ls | down <name> | down --all
Every verb takes --json.`

// blipCommand is the outer shell: it owns no flags of its own (so
// `blip <name> <verb> --flag` reaches the verb intact) and rewrites
// `<name> <verb> …` into `<verb> …` with the name bound, then runs the inner
// verb tree. Domain logic lives in internal/blip.
func (rt *runtime) blipCommand(use string) *cobra.Command {
	command := &cobra.Command{
		Use:                use,
		Short:              "Chaos proxy: one named HTTP or TCP proxy per `up`, faults toggled live, never touches the machine's network",
		Long:               blipUsage,
		Args:               cobra.ArbitraryArgs,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name := ""
			head := -1
			for i, a := range args {
				if a == "-h" || a == "--help" {
					_, err := fmt.Fprintln(rt.stdout, blipUsage)
					return err
				}
				if !strings.HasPrefix(a, "-") {
					head = i
					break
				}
			}
			if head < 0 {
				_, err := fmt.Fprintln(rt.stdout, blipUsage)
				return err
			}
			if !blipGlobalVerbs[args[head]] && !blipNameVerbs[args[head]] {
				name = args[head]
				args = append(append([]string{}, args[:head]...), args[head+1:]...)
			}
			inner := rt.blipVerbs(&name)
			inner.SetArgs(args)
			inner.SetOut(rt.stdout)
			inner.SetErr(rt.stderr)
			err := inner.Execute()
			var exit runx.ExitError
			if err == nil || errors.As(err, &exit) {
				return err
			}
			// cobra's own parse errors (unknown verb/flag) become a diagnostic
			// with the corrected invocation — never a bare error or usage dump.
			verb := ""
			for _, a := range args {
				if !strings.HasPrefix(a, "-") {
					verb = a
					break
				}
			}
			s := &runx.Session{Tool: "blip", JSON: rt.json || hasJSONFlag(args), Verb: verb, Stdout: rt.stdout, Stderr: rt.stderr}
			derr := runx.DiagError{Diag: runx.Diagnostic{Code: blip.DiagFaultInvalid, Severity: "error", Detail: err.Error(), Fix: "blip --help"}}
			if code := s.Finish(derr); code != 0 {
				return runx.ExitError{Code: code}
			}
			return nil
		},
	}
	return command
}

func (rt *runtime) blipVerbs(name *string) *cobra.Command {
	root := &cobra.Command{Use: "blip", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().BoolVar(&rt.json, "json", rt.json, "emit the versioned envelope as JSON")
	self := []string{"blip"}
	if filepath.Base(os.Args[0]) == "blip" {
		self = nil
	}
	session := func(cmd *cobra.Command) *runx.Session {
		return &runx.Session{Tool: "blip", JSON: rt.json, Verb: cmd.Name(), Stdout: rt.stdout, Stderr: rt.stderr}
	}
	finish := func(s *runx.Session, res blip.Result, err error) error {
		if err == nil {
			if rt.json {
				err = s.Emit(runx.Envelope{OK: true, Data: res.Data, Diagnostics: res.Diagnostics, Next: res.Next})
			} else {
				for _, l := range res.Lines {
					fmt.Fprintln(rt.stdout, l)
				}
				err = s.Emit(runx.Envelope{OK: true, Diagnostics: res.Diagnostics, Next: res.Next})
			}
		} else if res.Data != nil {
			// A verb that failed with partial state still owes it — plus the
			// diagnostic it failed with and its fix as next.
			diags, next := res.Diagnostics, res.Next
			var derr runx.DiagError
			if errors.As(err, &derr) {
				diags = append(diags, derr.Diag)
				if len(next) == 0 && derr.Diag.Fix != "" {
					next = []string{derr.Diag.Fix}
				}
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
	open := func() (*blip.Tool, error) { return blip.Open(self) }
	named := func(cmd *cobra.Command) (*runx.Session, *blip.Tool, error) {
		s := session(cmd)
		if *name == "" {
			return s, nil, runx.DiagError{Diag: runx.Diagnostic{Code: blip.DiagNotRunning, Severity: "error",
				Detail: cmd.Name() + " needs a proxy name", Fix: "blip <name> " + cmd.Name()}}
		}
		t, err := open()
		return s, t, err
	}

	var listen, to string
	upCmd := &cobra.Command{
		Use: "up <name>", Short: "Start a named proxy (daemonized) and print the URL to point the app at", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := session(cmd)
			t, err := open()
			if err != nil {
				return finish(s, blip.Result{}, err)
			}
			res, err := t.Up(args[0], listen, to)
			return finish(s, res, err)
		},
	}
	upCmd.Flags().StringVar(&listen, "listen", "", "address to listen on (:9091, 127.0.0.1:5433)")
	upCmd.Flags().StringVar(&to, "to", "", "upstream: http://host:port or tcp://host:port")

	var serveName string // own variable: StringVar resets its target to the default
	serveCmd := &cobra.Command{
		Use: "serve", Hidden: true, Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			t, err := open()
			if err != nil {
				return err
			}
			return t.Serve(serveName, listen, to)
		},
	}
	serveCmd.Flags().StringVar(&serveName, "name", "", "")
	serveCmd.Flags().StringVar(&listen, "listen", "", "")
	serveCmd.Flags().StringVar(&to, "to", "", "")

	lsCmd := &cobra.Command{
		Use: "ls", Short: "List proxies", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			t, err := open()
			if err != nil {
				return finish(s, blip.Result{}, err)
			}
			res, err := t.Ls()
			return finish(s, res, err)
		},
	}

	var all bool
	downCmd := &cobra.Command{
		Use: "down [name]", Short: "Stop a proxy (or --all) and clean its state", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := session(cmd)
			t, err := open()
			if err != nil {
				return finish(s, blip.Result{}, err)
			}
			var res blip.Result
			switch {
			case all:
				res, err = t.DownAll()
			case len(args) == 1:
				res, err = t.Down(args[0])
			default:
				err = runx.DiagError{Diag: runx.Diagnostic{Code: blip.DiagNotRunning, Severity: "error", Detail: "down needs a name or --all", Fix: "blip down --all"}}
			}
			return finish(s, res, err)
		},
	}
	downCmd.Flags().BoolVar(&all, "all", false, "take every proxy down")

	var scope blip.ScopeFlags
	setCmd := &cobra.Command{
		Use: "set <fault> [arg]", Short: "Replace the active fault", Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, t, err := named(cmd)
			if err != nil {
				return finish(s, blip.Result{}, err)
			}
			res, err := t.Set(*name, args, scope)
			return finish(s, res, err)
		},
	}
	setCmd.Flags().StringVar(&scope.Match, "match", "", "path glob (* crosses /), http only")
	setCmd.Flags().StringVar(&scope.Method, "method", "", "HTTP method, http only")
	setCmd.Flags().Int64Var(&scope.After, "after", 0, "apply only after N matching requests/connections")
	setCmd.Flags().StringVar(&scope.For, "for", "", "auto-clear after this duration")
	setCmd.Flags().StringVar(&scope.Rate, "rate", "", "probability 0 < p <= 1 (default 1)")
	setCmd.Flags().StringVar(&scope.Jitter, "jitter", "", "delay: extra uniform jitter")
	setCmd.Flags().StringVar(&scope.Body, "body", "", "error: response body (JSON gets application/json)")

	simple := func(use, short string, run func(*blip.Tool) (blip.Result, error)) *cobra.Command {
		return &cobra.Command{Use: use, Short: short, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			s, t, err := named(cmd)
			if err != nil {
				return finish(s, blip.Result{}, err)
			}
			res, err := run(t)
			return finish(s, res, err)
		}}
	}
	okCmd := simple("ok", "Clear the fault and release held requests", func(t *blip.Tool) (blip.Result, error) { return t.Ok(*name) })
	cutCmd := simple("cut", "Close every established connection once; fault unchanged", func(t *blip.Tool) (blip.Result, error) { return t.Cut(*name) })
	statusCmd := simple("status", "Mode, addresses, fault and counters", func(t *blip.Tool) (blip.Result, error) { return t.Status(*name) })

	var tail bool
	var last int
	logCmd := &cobra.Command{
		Use: "log", Short: "Per-request/connection log lines (--tail follows)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, t, err := named(cmd)
			if err != nil {
				return finish(s, blip.Result{}, err)
			}
			stop := make(chan struct{})
			if tail {
				sig := make(chan os.Signal, 1)
				signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
				go func() { <-sig; close(stop) }()
			}
			res, err := t.Log(*name, last, tail, rt.stdout, stop)
			return finish(s, res, err)
		},
	}
	logCmd.Flags().BoolVar(&tail, "tail", false, "follow the log")
	logCmd.Flags().IntVar(&last, "last", 50, "lines to show first")

	recordCmd := &cobra.Command{
		Use: "record on|off|clear", Short: "Store each proxied HTTP request for authz replay", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, t, err := named(cmd)
			if err != nil {
				return finish(s, blip.Result{}, err)
			}
			res, err := t.Record(*name, args[0])
			return finish(s, res, err)
		},
	}

	var as, expect, only, exclude string
	var mutations bool
	authzCmd := &cobra.Command{
		Use: "authz", Short: "Replay the recording against the upstream as another identity; report what was not refused", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, t, err := named(cmd)
			if err != nil {
				return finish(s, blip.Result{}, err)
			}
			if as == "" {
				return finish(s, blip.Result{}, runx.DiagError{Diag: runx.Diagnostic{Code: blip.DiagIdentityInvalid, Severity: "error", Detail: "authz needs --as", Fix: "blip " + *name + " authz --as none"}})
			}
			id, err := blip.ParseIdentity(as)
			if err != nil {
				return finish(s, blip.Result{}, err)
			}
			exp, err := blip.ParseExpect(expect)
			if err != nil {
				return finish(s, blip.Result{}, runx.DiagError{Diag: runx.Diagnostic{Code: blip.DiagFaultInvalid, Severity: "error", Detail: err.Error(), Fix: "blip " + *name + " authz --as " + as + " --expect 401,403,404"}})
			}
			res, err := t.Authz(*name, blip.AuthzOptions{Identity: id, Expect: exp, Only: only, Exclude: exclude, Mutations: mutations})
			return finish(s, res, err)
		},
	}
	authzCmd.Flags().StringVar(&as, "as", "", "none | header:<Name>=<value> | cookie:<name>=<value> | env:<VAR>")
	authzCmd.Flags().StringVar(&expect, "expect", "401,403,404", "statuses that count as correctly refused")
	authzCmd.Flags().StringVar(&only, "only", "", "replay only paths matching this glob")
	authzCmd.Flags().StringVar(&exclude, "exclude", "", "skip paths matching this glob")
	authzCmd.Flags().BoolVar(&mutations, "mutations", false, "also replay POST/PUT/PATCH/DELETE")
	root.AddCommand(upCmd, serveCmd, lsCmd, downCmd, setCmd, okCmd, cutCmd, statusCmd, logCmd, recordCmd, authzCmd)
	return root
}

func hasJSONFlag(args []string) bool {
	for _, a := range args {
		if a == "--json" {
			return true
		}
	}
	return false
}
