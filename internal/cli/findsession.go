package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/findsession"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/spf13/cobra"
)

func (rt *runtime) findSessionApplet() *cobra.Command {
	command := rt.findSessionCommand("find-session")
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetOut(rt.stdout)
	command.SetErr(rt.stderr)
	command.PersistentFlags().BoolVar(&rt.json, "json", false, "emit the versioned envelope as JSON")
	return command
}

func (rt *runtime) findSessionCommand(use string) *cobra.Command {
	var (
		root           string
		days           int
		limit          int
		includeCurrent bool
		full           bool
	)
	command := &cobra.Command{
		Use:   use + " [text|session-id]",
		Short: "Find the Claude Code session a pasted conversation came from, and the switcheroo line that reopens it",
		Long: `Matches the text against every main-session transcript and prints the
sessions that hold it, the one that wrote it first, with the command that
resumes it from its launch directory under the preset it started with
(cc, cco, ccoo, ccol, cch, ccl).

The text comes from the arguments, else stdin, else the clipboard. A session
id or id prefix is looked up by name. Your own session (CLAUDE_CODE_SESSION_ID)
is left out unless --include-current.`,
		Example: `  find-session                         # the clipboard
  pbpaste | find-session --json
  find-session 'Trap for that deploy'
  find-session e3f88062`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			s := &runx.Session{Tool: "find-session", JSON: rt.json, Verb: "find", Stdout: rt.stdout, Stderr: rt.stderr}
			finish := func(err error) error {
				if code := s.Finish(err); code != 0 {
					return runx.ExitError{Code: code}
				}
				return nil
			}
			query, source, err := rt.findSessionQuery(args)
			if err != nil {
				return finish(err)
			}
			home, _ := os.UserHomeDir()
			if root == "" {
				root = claudeProjectsRoot(home)
			}
			opts := findsession.Options{Root: root, Home: home, Since: time.Duration(days) * 24 * time.Hour, Limit: limit, Full: full}
			if !includeCurrent {
				opts.Exclude = os.Getenv("CLAUDE_CODE_SESSION_ID")
			}
			res, err := findsession.Find(query, opts)
			if err != nil {
				return finish(err)
			}
			ok := len(res.Sessions) > 0
			env := runx.Envelope{OK: ok, Diagnostics: res.Diagnostics}
			if rt.json {
				env.Data = map[string]any{"source": source, "result": res}
				for _, d := range res.Diagnostics {
					if d.Fix != "" {
						env.Next = append(env.Next, d.Fix)
					}
				}
				if ok {
					env.Next = append(env.Next, res.Sessions[0].Reopen)
				}
			} else {
				rt.findSessionReport(source, res)
			}
			if err := s.Emit(env); err != nil {
				return finish(err)
			}
			if !ok {
				return runx.ExitError{Code: 2}
			}
			return nil
		},
	}
	// A flag the parser rejects still answers one envelope (exit 2), never a
	// bare cobra error.
	command.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		s := &runx.Session{Tool: "find-session", JSON: rt.json || hasJSONFlag(os.Args[1:]), Verb: "find", Stdout: rt.stdout, Stderr: rt.stderr}
		return runx.ExitError{Code: s.Finish(runx.DiagError{Diag: runx.Diagnostic{
			Code: findsession.DiagBadFlag, Severity: "error", Detail: err.Error(), Fix: "find-session --help"}})}
	})
	command.Flags().StringVar(&root, "root", "", "Claude projects directory (default $CLAUDE_CONFIG_DIR/projects or ~/.claude/projects)")
	command.Flags().IntVar(&days, "days", 0, "only sessions written in the last N days (default all)")
	command.Flags().IntVar(&limit, "limit", 3, "sessions to show")
	command.Flags().BoolVar(&full, "full", false, "scan every transcript at once instead of newest first (2 days, 14 days, all) stopping at the first that wrote it")
	command.Flags().BoolVar(&includeCurrent, "include-current", false, "also search this session ($CLAUDE_CODE_SESSION_ID)")
	return command
}

// findSessionQuery takes the text from the arguments, else piped stdin, else
// the clipboard — so a bare call searches whatever was just copied.
func (rt *runtime) findSessionQuery(args []string) (query, source string, err error) {
	if len(args) > 0 {
		return strings.Join(args, " "), "args", nil
	}
	if f, ok := rt.stdin.(*os.File); !ok || stdinPiped(f) {
		data, err := io.ReadAll(rt.stdin)
		if err != nil {
			return "", "", err
		}
		if strings.TrimSpace(string(data)) != "" {
			return string(data), "stdin", nil
		}
	}
	if goruntime.GOOS == "darwin" {
		if out, err := exec.Command("pbpaste").Output(); err == nil && strings.TrimSpace(string(out)) != "" {
			return string(out), "clipboard", nil
		}
	}
	return "", "", nil
}

func stdinPiped(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice == 0
}

func claudeProjectsRoot(home string) string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return filepath.Join(dir, "projects")
	}
	return filepath.Join(home, ".claude", "projects")
}

// findSessionReport prints each session as three lines — id and title, where
// and how it ran, the reopen command alone on its own line so it copies
// clean.
func (rt *runtime) findSessionReport(source string, res findsession.Result) {
	fmt.Fprintf(rt.stdout, "find-session · %s · %d/%d sessions in %s\n", source, res.Scanned, res.Total, res.Elapsed)
	for _, s := range res.Sessions {
		title := s.Title
		if title == "" {
			title = s.Prompt
		}
		fmt.Fprintf(rt.stdout, "\n%s  %s\n", s.ID, title)
		where := s.Cwd
		if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(where, home+"/") {
			where = "~" + strings.TrimPrefix(where, home)
		}
		if s.Branch != "" {
			where += " (" + s.Branch + ")"
		}
		how := strings.TrimSpace(strings.TrimPrefix(s.Model, "claude-") + " " + s.Effort)
		if s.Ultracode {
			how += " ultracode"
		}
		fmt.Fprintf(rt.stdout, "  %s · %s · %s", where, sessionSpan(s.Started, s.Ended), how)
		if res.Mode == "text" {
			fmt.Fprintf(rt.stdout, " · %s", matchNote(s))
		}
		fmt.Fprintf(rt.stdout, "\n  %s\n", s.Reopen)
	}
	if len(res.Sessions) > 0 {
		fmt.Fprintln(rt.stdout)
	}
}

func sessionSpan(start, end time.Time) string {
	start, end = start.Local(), end.Local()
	if start.Format(time.DateOnly) == end.Format(time.DateOnly) {
		return start.Format("2006-01-02 15:04") + "–" + end.Format("15:04")
	}
	return start.Format("2006-01-02 15:04") + " – " + end.Format("2006-01-02 15:04")
}

func matchNote(s findsession.Session) string {
	note := fmt.Sprintf("%d/%d matched", s.Found, s.Fragments)
	switch {
	case s.Authored == 0 && s.Titled:
		return note + ", in its title"
	case s.Authored == 0:
		return note + ", quoted only"
	case s.Lines-s.LastHit <= 20:
		return note + ", its ending"
	default:
		return note + fmt.Sprintf(", written at line %d of %d", s.LastHit, s.Lines)
	}
}
