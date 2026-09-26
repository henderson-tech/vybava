package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/claudeguards"
	"github.com/henderson-tech/vybava/internal/codexusage"
	"github.com/henderson-tech/vybava/internal/redact"
	"github.com/henderson-tech/vybava/internal/secretscan"
	"github.com/henderson-tech/vybava/internal/state"
	"github.com/spf13/cobra"
)

func (rt *runtime) redactApplet() *cobra.Command {
	command := rt.redactCommand("redact")
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetOut(rt.stdout)
	command.SetErr(rt.stderr)
	command.PersistentFlags().BoolVar(&rt.json, "json", false, "emit stable JSON output")
	return command
}

func (rt *runtime) redactCommand(use string) *cobra.Command {
	var (
		target      redact.Target
		all, apply  bool
		since       string
		knownEnv    []string
		knownDotenv []string
		only        []string
	)
	command := &cobra.Command{
		Use:   use + " [path...]",
		Short: "Find and redact secrets leaked into Claude Code and Codex conversation history",
		Long: `Scans agent history for secret material — provider tokens, credential and
secret-named env assignments, URL credentials, auth headers, and the
"len 44 prefix abcdefg…" fragments an agent prints believing them harmless —
and reports each as file · line · detector · masked shape. It never prints a
value. --apply overwrites every span in place with a same-length
[REDACTED:<detector>] marker: JSONL stays valid, byte offsets hold, a live
session can keep appending, and no copy of the original is kept.

With no target it takes the current Claude session ($CLAUDE_CODE_SESSION_ID)
— its subagents, workflow journals, tool results and task output included.

Known values: --known-env NAME reads a value from the environment (let onyx
inject it: run_command with env_refs), --known-dotenv FILE takes the value of
every secret-named assignment in a .env. Either finds a leaked value no
pattern recognises; values never leave the process.`,
		Example: `  redact                                    # this session: report only (exit 1 on findings)
  redact --session 2bbb9875-… --apply        # a session and everything under it
  redact --project ~/Work/app --since week   # a repo's Claude + Codex history
  redact --all --json                        # every Claude and Codex transcript
  redact --all --known-dotenv .env --apply   # redact this .env's values wherever they leaked`,
		RunE: func(_ *cobra.Command, args []string) error {
			target.Paths = args
			if all {
				target.Claude, target.Codex = true, true
			}
			if since != "" {
				at, err := codexusage.ParseSince(since, time.Now())
				if err != nil {
					return err
				}
				target.Since = at
			}
			if target.Empty() {
				id := os.Getenv("CLAUDE_CODE_SESSION_ID")
				if id == "" {
					return errors.New("name a target: --session, --project, --all, --claude, --codex or a path")
				}
				target.Sessions = []string{id}
			}
			known := &secretscan.Known{}
			for _, name := range knownEnv {
				value, ok := os.LookupEnv(name)
				if !ok {
					return fmt.Errorf("--known-env %s: not set", name)
				}
				if !known.Add(value) {
					return fmt.Errorf("--known-env %s: shorter than %d characters or a placeholder, refused", name, secretscan.MinKnown)
				}
			}
			for _, path := range knownDotenv {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if _, err := known.AddDotenv(data); err != nil {
					return fmt.Errorf("--known-dotenv %s: %w", path, err)
				}
			}
			roots, err := redact.DefaultRoots()
			if err != nil {
				return err
			}
			files, unreadable, err := roots.Files(target)
			if err != nil {
				return err
			}
			classes, err := redactClasses(only)
			if err != nil {
				return err
			}
			opts := redact.Options{Apply: apply, Classes: classes, Known: known}
			if apply {
				audit, err := openRedactAudit()
				if err != nil {
					return err
				}
				defer audit.Close()
				opts.Audit = audit
			}
			report := redact.Run(files, opts)
			report.Unreadable += unreadable
			if rt.json {
				if err := writeJSON(rt.stdout, report); err != nil {
					return err
				}
			} else {
				rt.redactReport(report)
			}
			switch {
			case report.Errors > 0 || report.Unreadable > 0:
				return fmt.Errorf("%d file(s) could not be scanned or written, %d unreadable — the report is incomplete", report.Errors, report.Unreadable)
			case !apply && report.Spans > 0, apply && report.Changed > 0:
				return ErrFindings
			}
			return nil
		},
	}
	flags := command.Flags()
	flags.StringSliceVar(&target.Sessions, "session", nil, "Claude session id (with subagents, workflows, task output) or Codex thread id (repeatable)")
	flags.StringSliceVar(&target.Projects, "project", nil, "project directory: its Claude sessions, worktrees included, and Codex rollouts (repeatable)")
	flags.BoolVar(&target.Claude, "claude", false, "all Claude Code history")
	flags.BoolVar(&target.Codex, "codex", false, "all Codex history")
	flags.BoolVar(&all, "all", false, "all Claude Code and Codex history")
	flags.StringVar(&since, "since", "", "only files modified since: 3h, today, week, 2026-09-01")
	flags.BoolVar(&apply, "apply", false, "overwrite every finding in place (default: report only)")
	flags.StringSliceVar(&knownEnv, "known-env", nil, "environment variable holding an exact value to find (repeatable)")
	flags.StringSliceVar(&knownDotenv, "known-dotenv", nil, ".env file whose secret-named values to find (repeatable)")
	flags.StringSliceVar(&only, "only", nil, "detector classes to take: tokens, credentials, env, fragments (default: all)")
	return command
}

// openRedactAudit appends to the audit log beside Výbava's state: one line
// per file an apply touched — path, count, detectors, never a value.
func openRedactAudit() (*os.File, error) {
	store, err := state.DefaultStore()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(filepath.Dir(store.Path), "redact-audit.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

func (rt *runtime) redactReport(r redact.Report) {
	var counts []string
	for d, n := range r.Counts {
		counts = append(counts, fmt.Sprintf("%s %d", d, n))
	}
	sort.Strings(counts)
	fmt.Fprintf(rt.stdout, "REDACT  %s · %d files · %d with findings · %d spans", r.Mode, r.Files, len(r.Leaky), r.Spans)
	if r.Mode == "apply" {
		fmt.Fprintf(rt.stdout, " · %d redacted", r.Redacted)
		if r.Changed > 0 {
			fmt.Fprintf(rt.stdout, " · %d changed underneath (rerun)", r.Changed)
		}
	}
	if r.Known > 0 {
		fmt.Fprintf(rt.stdout, " · %d known values", r.Known)
	}
	if r.Unreadable > 0 {
		fmt.Fprintf(rt.stdout, " · %d unreadable", r.Unreadable)
	}
	fmt.Fprintln(rt.stdout)
	if len(counts) > 0 {
		fmt.Fprintf(rt.stdout, "        %s\n", strings.Join(counts, " · "))
	}
	home, _ := os.UserHomeDir()
	for _, f := range r.Leaky {
		path := f.Path
		if home != "" && strings.HasPrefix(path, home+"/") {
			path = "~" + strings.TrimPrefix(path, home)
		}
		fmt.Fprintf(rt.stdout, "\n%s  (%d", path, f.Spans)
		if r.Mode == "apply" {
			fmt.Fprintf(rt.stdout, ", %d redacted", f.Redacted)
		}
		fmt.Fprintln(rt.stdout, ")")
		if f.Error != "" {
			fmt.Fprintf(rt.stdout, "  ✗ %s\n", f.Error)
		}
		for _, g := range f.Findings {
			fmt.Fprintf(rt.stdout, "  %6d  %-22s %s\n", g.Line, g.Detector, g.Shape)
		}
		if f.Truncated {
			fmt.Fprintf(rt.stdout, "  … %d more\n", f.Spans-len(f.Findings))
		}
	}
	if r.Mode == "scan" && r.Spans > 0 {
		fmt.Fprintln(rt.stdout, "\nNothing was changed. Rerun with --apply to redact in place.")
	}
}

// redactSession runs one pass over a Claude session and everything under it
// — the shared step of `claude-guards ctx` (scan) and its SessionEnd hook
// (apply, audited).
func redactSession(session string, apply bool) (redact.Report, error) {
	roots, err := redact.DefaultRoots()
	if err != nil {
		return redact.Report{}, err
	}
	files, unreadable, err := roots.Files(redact.Target{Sessions: []string{session}})
	if err != nil {
		return redact.Report{}, err
	}
	opts := redact.Options{Apply: apply}
	if apply {
		audit, err := openRedactAudit()
		if err != nil {
			return redact.Report{}, err
		}
		defer audit.Close()
		opts.Audit = audit
	}
	report := redact.Run(files, opts)
	report.Unreadable += unreadable
	return report, nil
}

// sessionLeaks scans one Claude session for `claude-guards ctx`. A session it
// cannot scan is a warning on stderr and nil: the context report stands on
// its own.
func sessionLeaks(session string, stderr io.Writer) *claudeguards.Leaks {
	report, err := redactSession(session, false)
	if err != nil {
		fmt.Fprintf(stderr, "claude-guards ctx: leak scan skipped: %v\n", err)
		return nil
	}
	return &claudeguards.Leaks{Spans: report.Spans, Files: len(report.Leaky), Unscanned: report.Errors + report.Unreadable, Counts: report.Counts, Session: session}
}

// redactClasses maps --only names onto secretscan classes; none means All.
func redactClasses(only []string) (secretscan.Class, error) {
	names := map[string]secretscan.Class{
		"tokens": secretscan.Tokens, "credentials": secretscan.Credentials,
		"env": secretscan.EnvValues, "fragments": secretscan.Fragments,
	}
	var classes secretscan.Class
	for _, name := range only {
		class, ok := names[name]
		if !ok {
			return 0, fmt.Errorf("--only %s: not a class (tokens, credentials, env, fragments)", name)
		}
		classes |= class
	}
	return classes, nil
}
