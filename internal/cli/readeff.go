package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/henderson-tech/vybava/internal/readeff"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/spf13/cobra"
)

// readeff diagnostic codes.
const (
	diagReadWarning = "UNREADABLE_TRANSCRIPT"
	diagNoSession   = "NO_SESSION"
	diagAmbiguous   = "AMBIGUOUS_SESSION"
)

// readeffFlagErr refuses a count flag below its floor before any scan runs.
func readeffFlagErr(flags map[string][2]int) error {
	for _, name := range []string{"days", "big", "window", "top"} {
		if v, ok := flags[name]; ok && v[0] < v[1] {
			return runx.DiagError{Diag: runx.Diagnostic{Code: diagBadFlag, Severity: "error",
				Detail: fmt.Sprintf("--%s must be at least %d (got %d)", name, v[1], v[0])}}
		}
	}
	return nil
}

func (rt *runtime) readeffApplet() *cobra.Command {
	cmd := rt.readeffCommand("readeff")
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(rt.stdout)
	cmd.SetErr(rt.stderr)
	cmd.PersistentFlags().BoolVar(&rt.json, "json", false, "emit machine-readable output")
	return cmd
}

// readeffCommand wires the navigation-efficiency verbs; the domain lives in
// internal/readeff. Every verb emits one runx envelope; without --json the
// data renders as a human report instead.
func (rt *runtime) readeffCommand(use string) *cobra.Command {
	root := &cobra.Command{
		Use:   use,
		Short: "How agents navigate code — what their reads and searches put into context, per repo, session and file",
		Long: "readeff scans Claude Code transcripts (~/.claude/projects) and Codex rollouts (~/.codex) on demand\n" +
			"and reports what reads and searches cost against what was changed. Nothing is stored; no command\n" +
			"text or output is shown, only counts and paths.\n" +
			"  readeff report              this repository (--all for every one), last 14 days\n" +
			"  readeff files --top 20      the files read most, with re-reads\n" +
			"  readeff session <id>        one session call by call: what each read and search counted as",
	}
	var claudeRoot, codexDir, project string
	var days, bigFile, window int
	var all bool
	pf := root.PersistentFlags()
	pf.StringVar(&claudeRoot, "claude-root", "", "Claude Code projects directory (default ~/.claude/projects)")
	pf.StringVar(&codexDir, "codex-dir", "", "Codex directory holding sessions/ (default ~/.codex)")
	pf.IntVar(&days, "days", 14, "sessions whose transcript was written in the last N days")
	pf.StringVar(&project, "project", "", "repository directory to report on (default: the current one)")
	pf.BoolVar(&all, "all", false, "every repository, not just this one")
	pf.IntVar(&bigFile, "big", readeff.DefaultConfig.BigFile, "a whole-file read above this many lines counts as big")
	pf.IntVar(&window, "window", readeff.DefaultConfig.Window, "calls after a search in which reading a hit counts")

	session := func(cmd *cobra.Command) *runx.Session {
		return &runx.Session{Tool: "readeff", JSON: rt.json, Verb: cmd.Name(), Stdout: rt.stdout, Stderr: rt.stderr}
	}
	finish := func(s *runx.Session, data any, diags []runx.Diagnostic, err error) error {
		env := runx.Envelope{OK: err == nil, Verb: s.Verb, Data: data, Diagnostics: diags}
		var diagErr runx.DiagError
		if errors.As(err, &diagErr) {
			env.Diagnostics = append(env.Diagnostics, diagErr.Diag)
		} else if err != nil {
			env.Diagnostics = append(env.Diagnostics, runx.Diagnostic{Code: runx.DiagInfraError, Severity: "error", Detail: err.Error()})
		}
		if !rt.json {
			env.Data = nil // the verb already rendered it for a human
		}
		if emitErr := s.Emit(env); emitErr != nil {
			return emitErr
		}
		if code := s.Finish(err); code != 0 {
			return runx.ExitError{Code: code}
		}
		return nil
	}
	cfg := func() readeff.Config { return readeff.Config{BigFile: bigFile, Window: window} }
	// scan reads the window. sessionID narrows it to one session and every
	// repository, since a session id is already unique.
	scan := func(sessionID string, each func(readeff.Session)) (readeff.Options, []runx.Diagnostic, error) {
		if err := readeffFlagErr(map[string][2]int{"days": {days, 1}, "big": {bigFile, 0}, "window": {window, 1}}); err != nil {
			return readeff.Options{}, nil, err
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return readeff.Options{}, nil, err
		}
		opts := readeff.Options{ClaudeRoot: claudeRoot, CodexDir: codexDir, Since: time.Now().AddDate(0, 0, -days), Session: sessionID}
		if opts.ClaudeRoot == "" {
			opts.ClaudeRoot = filepath.Join(home, ".claude", "projects")
		}
		if opts.CodexDir == "" {
			opts.CodexDir = filepath.Join(home, ".codex")
		}
		for _, p := range []*string{&opts.ClaudeRoot, &opts.CodexDir} {
			if *p, err = expandHome(*p); err != nil {
				return opts, nil, err
			}
		}
		if !all && sessionID == "" {
			dir := project
			if dir == "" {
				dir = "."
			}
			if opts.Repo, err = readeff.RepoRoot(dir); err != nil {
				return opts, nil, err
			}
		}
		warnings, err := readeff.Scan(opts, each)
		var diags []runx.Diagnostic
		for _, w := range warnings {
			diags = append(diags, runx.Diagnostic{Code: diagReadWarning, Severity: "warning", Detail: w})
		}
		return opts, diags, err
	}

	report := &cobra.Command{
		Use: "report", Short: "Navigation cost against change, per agent, repository and file", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			b := readeff.NewBuilder(cfg())
			opts, diags, err := scan("", b.Add)
			if err != nil {
				return finish(s, nil, diags, err)
			}
			r := b.Report(15)
			r.Since, r.Repo = opts.Since, opts.Repo
			if !rt.json {
				readeff.RenderReport(rt.stdout, r)
			}
			return finish(s, r, diags, nil)
		},
	}
	var top int
	files := &cobra.Command{
		Use: "files", Short: "The files read most, with how often they were re-read", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			if err := readeffFlagErr(map[string][2]int{"top": {top, 1}}); err != nil {
				return finish(s, nil, nil, err)
			}
			b := readeff.NewBuilder(cfg())
			opts, diags, err := scan("", b.Add)
			if err != nil {
				return finish(s, nil, diags, err)
			}
			list := b.Files(top)
			if !rt.json {
				readeff.RenderFiles(rt.stdout, list, opts.Repo)
			}
			return finish(s, list, diags, nil)
		},
	}
	files.Flags().IntVar(&top, "top", 20, "how many files to list")
	trace := &cobra.Command{
		Use: "session <id>", Short: "One session call by call: what each read and search counted as", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := session(cmd)
			var found []readeff.Session
			_, diags, err := scan(args[0], func(sess readeff.Session) { found = append(found, sess) })
			if err != nil {
				return finish(s, nil, diags, err)
			}
			for _, sess := range found {
				if sess.ID == args[0] { // an exact id beats its subagents' prefix matches
					found = []readeff.Session{sess}
					break
				}
			}
			switch n := len(found); {
			case n == 0:
				return finish(s, nil, diags, runx.DiagError{Diag: runx.Diagnostic{Code: diagNoSession, Severity: "error",
					Detail: fmt.Sprintf("no session %q with tool calls in the last %d days", args[0], days), Fix: "readeff session <id> --days 60"}})
			case n > 1:
				return finish(s, nil, diags, runx.DiagError{Diag: runx.Diagnostic{Code: diagAmbiguous, Severity: "error",
					Detail: fmt.Sprintf("%q matches %d sessions (a session and its subagents share an id)", args[0], n), Fix: "readeff session " + found[1].ID}})
			}
			t := readeff.Trace(found[0], cfg())
			if !rt.json {
				readeff.RenderTrace(rt.stdout, t, cfg())
			}
			return finish(s, t, diags, nil)
		},
	}
	root.AddCommand(report, files, trace)
	return root
}
