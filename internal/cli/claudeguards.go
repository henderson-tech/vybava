package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/henderson-tech/vybava/internal/claudeguards"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/spf13/cobra"
)

func (rt *runtime) claudeGuardsApplet() *cobra.Command {
	cmd := rt.claudeGuardsCommand("claude-guards")
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(rt.stdout)
	cmd.SetErr(rt.stderr)
	cmd.PersistentFlags().BoolVar(&rt.json, "json", false, "emit machine-readable output")
	return cmd
}

// claudeGuardsCommand wires the hook verbs. The hook verbs (bash, read) speak
// Claude Code's contract — reason on stderr, exit 2 — so they never emit the
// envelope; `check` is the same decision behind the envelope for humans,
// tests and skills.
func (rt *runtime) claudeGuardsCommand(use string) *cobra.Command {
	root := &cobra.Command{
		Use:   use,
		Short: "PreToolUse guard hooks for Claude Code — destructive git/docker, secret dumps, context-budget rules",
		Long: "claude-guards enforces the hard bans of ~/.claude/CLAUDE.md at the tool boundary,\n" +
			"including under bypass permissions and inside subagents. Wire it in settings.json:\n" +
			"  PreToolUse Bash → claude-guards bash · PreToolUse Read → claude-guards read\n" +
			"  PreToolUse mcp__playwright__.*|mcp__plugin_chrome-devtools-mcp_chrome-devtools__.* → claude-guards browser\n" +
			"  SessionStart → claude-guards doctor --fix · claude-guards weather --reap · claude-guards swarm-teardown --dead-only\n" +
			"  SessionEnd → claude-guards swarm-teardown · claude-guards browser-teardown · claude-guards reap · claude-guards redact-session\n" +
			"`claude-guards hooks` prints this wiring as JSON; `doctor` checks the live file against it.\n" +
			"A block prints its reason and the sanctioned alternative on stderr and exits 2.",
	}
	hook := func(name, short string, decide func(*claudeguards.HookInput) *claudeguards.Denial) *cobra.Command {
		return &cobra.Command{
			Use:   name,
			Short: short,
			Args:  cobra.NoArgs,
			RunE: func(_ *cobra.Command, _ []string) error {
				in, err := claudeguards.ReadInput(rt.stdin)
				if err != nil {
					return nil // fail open: malformed payload must never brick the session
				}
				d := decide(in)
				if d == nil {
					if context := claudeguards.BudgetContext(in); context != "" {
						return json.NewEncoder(rt.stdout).Encode(map[string]map[string]string{
							"hookSpecificOutput": map[string]string{"hookEventName": "PreToolUse", "additionalContext": context},
						})
					}
					return nil
				}
				if _, err := fmt.Fprint(rt.stderr, d.Text()); err != nil {
					return err
				}
				return ErrHookBlocked
			},
		}
	}
	root.AddCommand(hook("bash", "PreToolUse:Bash — every command rule (stdin: hook JSON)", claudeguards.Bash))
	root.AddCommand(hook("read", "PreToolUse:Read — raw .e2e PNGs, transcripts, over-budget reads (stdin: hook JSON)", claudeguards.Read))
	root.AddCommand(hook("browser", "PreToolUse:mcp__playwright__*|mcp__plugin_chrome-devtools-mcp_chrome-devtools__* — this session's Onyx browser must be running (stdin: hook JSON)", claudeguards.Browser))

	var cwd string
	check := &cobra.Command{
		Use:   "check <bash|read> <command-or-path>",
		Short: "Evaluate the rules against a command or path without a hook payload",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := &runx.Session{Tool: "claude-guards", JSON: rt.json, Verb: cmd.Name(), Stdout: rt.stdout, Stderr: rt.stderr}
			in := &claudeguards.HookInput{CWD: cwd}
			var d *claudeguards.Denial
			switch args[0] {
			case "bash":
				in.ToolInput.Command = args[1]
				d = claudeguards.Bash(in)
			case "read":
				in.ToolInput.FilePath = args[1]
				d = claudeguards.Read(in)
			default:
				return finishGuardCheck(s, nil, &runx.DiagError{Diag: runx.Diagnostic{
					Code: guardDiagUsage, Severity: "error",
					Detail: fmt.Sprintf("unknown tool %q — the first argument names the hook", args[0]),
					Fix:    fmt.Sprintf("claude-guards check bash %q --json", args[1]),
				}})
			}
			return finishGuardCheck(s, d, nil)
		},
	}
	check.Flags().StringVar(&cwd, "cwd", "", "directory the command would run in (default: current)")
	root.AddCommand(check)
	root.AddCommand(&cobra.Command{
		Use: "ctx <session-id|latest>", Short: "Diagnose transcript context growth without dumping conversation content", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := &runx.Session{Tool: "claude-guards", JSON: rt.json, Verb: cmd.Name(), Stdout: rt.stdout, Stderr: rt.stderr}
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			path, err := claudeguards.ResolveTranscript(filepath.Join(home, ".claude", "projects"), args[0])
			if err != nil {
				return finishGuardCheck(s, nil, &runx.DiagError{Diag: runx.Diagnostic{Code: "TRANSCRIPT_UNAVAILABLE", Severity: "error", Detail: err.Error(), Fix: "claude-guards ctx <longer-session-id>"}})
			}
			report, err := claudeguards.DiagnoseContext(path)
			if err != nil {
				return finishGuardCheck(s, nil, &runx.DiagError{Diag: runx.Diagnostic{Code: "TRANSCRIPT_UNAVAILABLE", Severity: "error", Detail: err.Error()}})
			}
			if report.Kind == "session" {
				report.Leaks = sessionLeaks(strings.TrimSuffix(filepath.Base(path), ".jsonl"), rt.stderr)
			}
			if !rt.json {
				return report.Render(rt.stdout)
			}
			return s.Emit(runx.Envelope{OK: true, Verb: s.Verb, Data: report, Diagnostics: []runx.Diagnostic{}, Next: []string{}})
		},
	})

	var deadOnly bool
	teardown := &cobra.Command{
		Use:   "swarm-teardown",
		Short: "Kill this session's swarm tmux server and sweep dead ones (SessionEnd); --dead-only for SessionStart",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			claudeguards.SwarmTeardown(deadOnly, rt.stderr)
			return nil
		},
	}
	teardown.Flags().BoolVar(&deadOnly, "dead-only", false, "sweep dead leaders only; never touch the caller's own swarm")
	root.AddCommand(teardown)

	// Machine-health verbs. Each is a thin wrapper over one package entry
	// point; the package owns the behaviour and its tests.
	root.AddCommand(&cobra.Command{
		Use:   "list [family]",
		Short: "List every rule id with its event, one-line reason and escape hatch (from the rule registry)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			family := ""
			if len(args) == 1 {
				family = args[0]
			}
			return claudeguards.RenderRules(rt.stdout, family, rt.json)
		},
	})

	var doctorFix bool
	var settingsPath string
	doctor := &cobra.Command{
		Use:   "doctor",
		Short: "Verify ~/.claude/settings.json still carries every claude-guards hook (SessionStart); --fix re-inserts missing ones",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return claudeguards.Doctor(settingsPath, doctorFix, rt.stdout, rt.stderr)
		},
	}
	doctor.Flags().BoolVar(&doctorFix, "fix", false, "re-insert missing hook entries into settings.json (surgical merge, never a rewrite)")
	doctor.Flags().StringVar(&settingsPath, "settings", "", "settings file to check (default ~/.claude/settings.json)")
	root.AddCommand(doctor)

	root.AddCommand(&cobra.Command{
		Use:   "hooks",
		Short: "Print the settings.json hook wiring manifest as JSON (what doctor checks against)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			enc := json.NewEncoder(rt.stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(claudeguards.Hooks)
		},
	})

	var weatherText, weatherReap bool
	weather := &cobra.Command{
		Use:   "weather",
		Short: "One line of machine pressure (load, memory, sims, dev servers, sessions) as SessionStart additionalContext; --text for humans",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return claudeguards.Weather(weatherText, weatherReap, rt.stdout, rt.stderr)
		},
	}
	weather.Flags().BoolVar(&weatherText, "text", false, "print the human report (with the idle sessions it would name) instead of the hook JSON")
	weather.Flags().BoolVar(&weatherReap, "reap", false, "then run reap on the same process table (the SessionStart hook form)")
	root.AddCommand(weather)

	root.AddCommand(&cobra.Command{
		Use:   "reap",
		Short: "Kill orphaned xcodebuild/WebDriverAgent/Appium processes whose owning session is gone (SessionEnd; SessionStart runs it via weather --reap)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			claudeguards.Reap(rt.stderr)
			return nil
		},
	})

	var session string
	browserTeardown := &cobra.Command{
		Use:   "browser-teardown",
		Short: "Stop this session's Onyx browser at session end (SessionEnd; stdin: hook JSON)",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// --session is the hand-run form, typed at a terminal that never
			// sends EOF, so the flag is consulted BEFORE stdin: reading first
			// would make the documented invocation hang on a read nothing ever
			// ends. Without it, a payload comes from the hook — and a malformed
			// one fails open the same way the PreToolUse hooks do, but keeps
			// going, because the environment still names the session whose
			// browser must stop.
			in := &claudeguards.HookInput{}
			if s := strings.TrimSpace(session); s != "" {
				in.SessionID = s
			} else if payload, err := claudeguards.ReadInput(rt.stdin); err == nil {
				in = payload
			}
			claudeguards.BrowserTeardown(in, rt.stderr)
			return nil
		},
	}
	browserTeardown.Flags().StringVar(&session, "session", "", "session id to stop (default: the hook payload's, else CLAUDE_CODE_SESSION_ID)")
	root.AddCommand(browserTeardown)

	var redactTarget string
	redactSessionCmd := &cobra.Command{
		Use:   "redact-session",
		Short: "Redact secrets from the ending session's files in place (SessionEnd; stdin: hook JSON) — docs/redact.md",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			// The flag before stdin, as for browser-teardown: a hand-run at a
			// terminal never sends EOF. A hook never fails the exit — every
			// problem is a line on stderr.
			id := strings.TrimSpace(redactTarget)
			if id == "" {
				if in, err := claudeguards.ReadInput(rt.stdin); err == nil {
					id = in.SessionID
				}
			}
			if id == "" {
				id = os.Getenv("CLAUDE_CODE_SESSION_ID")
			}
			if id == "" {
				fmt.Fprintln(rt.stderr, "claude-guards redact-session: no session id (payload, --session or CLAUDE_CODE_SESSION_ID)")
				return nil
			}
			report, err := redactSession(id, true)
			switch {
			case err != nil:
				fmt.Fprintf(rt.stderr, "claude-guards redact-session: %v\n", err)
			case report.Redacted > 0 || report.Errors > 0 || report.Changed > 0:
				fmt.Fprintf(rt.stderr, "claude-guards redact-session: %d secret spans redacted in %d files of session %s (%d errors, %d changed underneath) — audit: ~/.config/vybava/redact-audit.jsonl\n",
					report.Redacted, len(report.Leaky), id, report.Errors, report.Changed)
			}
			return nil
		},
	}
	redactSessionCmd.Flags().StringVar(&redactTarget, "session", "", "session id to redact (default: the hook payload's, else CLAUDE_CODE_SESSION_ID)")
	root.AddCommand(redactSessionCmd)

	root.AddCommand(&cobra.Command{
		Use:    "refresh-visibility <repo-dir> <cache-file>",
		Short:  "internal: background repo-visibility refresh spawned by the commit-secrets rule",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			claudeguards.RefreshVisibility(args[0], args[1])
			return nil
		},
	})
	return root
}

// Closed diagnostic codes for `claude-guards check`.
const (
	// guardDiagBlocked fires when a rule matched; data.rule names it and the
	// detail carries the same reason the hook prints. Fix is the escape hatch.
	guardDiagBlocked = "BLOCKED"
	// guardDiagUsage fires on a wrong tool name; fix is the corrected invocation.
	guardDiagUsage = "USAGE"
)

type guardCheckData struct {
	Rule string `json:"rule,omitempty"`
}

func finishGuardCheck(s *runx.Session, d *claudeguards.Denial, usage *runx.DiagError) error {
	env := runx.Envelope{V: runx.EnvelopeVersion, OK: true, Verb: s.Verb, Data: guardCheckData{}, Diagnostics: []runx.Diagnostic{}, Next: []string{}}
	var err error
	switch {
	case usage != nil:
		env.OK = false
		env.Diagnostics = append(env.Diagnostics, usage.Diag)
		env.Next = append(env.Next, usage.Diag.Fix)
		err = usage
	case d != nil:
		env.OK = false
		env.Data = guardCheckData{Rule: d.Rule}
		env.Diagnostics = append(env.Diagnostics, runx.Diagnostic{Code: guardDiagBlocked, Severity: "error", Detail: d.Rule + ": " + d.Message, Fix: d.EscapeHatch})
		err = &runx.DiagError{Diag: env.Diagnostics[0]}
	}
	if emitErr := s.Emit(env); emitErr != nil {
		return emitErr
	}
	if code := s.Finish(err); code != 0 {
		return runx.ExitError{Code: code}
	}
	return nil
}
