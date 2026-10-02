package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/fleet"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/spf13/cobra"
)

func (rt *runtime) fleetApplet() *cobra.Command {
	c := rt.fleetCommand("fleet")
	c.SetOut(rt.stdout)
	c.SetErr(rt.stderr)
	c.SilenceErrors, c.SilenceUsage = true, true
	c.PersistentFlags().BoolVar(&rt.json, "json", false, "emit the versioned envelope as JSON")
	return c
}

func (rt *runtime) fleetCommand(use string) *cobra.Command {
	return rt.fleetCommandWithEnv(use, func(withCodex bool) (fleet.Env, error) {
		home, err := os.UserHomeDir()
		if err != nil {
			return fleet.Env{}, err
		}
		env := fleet.Env{Home: home, Now: time.Now()}
		if withCodex {
			env.Codex = fleet.CodexUsage(home, func(ctx context.Context, name string, args ...string) ([]byte, error) {
				return exec.CommandContext(ctx, name, args...).Output()
			})
		}
		return env, nil
	})
}

// fleetCommandWithEnv takes the machine as a factory so tests drive fixture
// registries and process tables. Domain logic lives in internal/fleet.
func (rt *runtime) fleetCommandWithEnv(use string, machine func(withCodex bool) (fleet.Env, error)) *cobra.Command {
	session := func(verb string) *runx.Session {
		return &runx.Session{Tool: "fleet", JSON: rt.json, Verb: verb, Stdout: rt.stdout, Stderr: rt.stderr}
	}
	// finish prints the human lines (text) or the data (JSON) with the
	// diagnostics gathered on the way, then hands back the real exit code.
	finish := func(s *runx.Session, data any, lines []string, diags []runx.Diagnostic, next []string, err error) error {
		if err == nil {
			envelope := runx.Envelope{OK: true, Diagnostics: diags, Next: next}
			if rt.json {
				envelope.Data = data
			} else {
				for _, line := range lines {
					fmt.Fprintln(rt.stdout, line)
				}
			}
			err = s.Emit(envelope)
		} else if derr := (runx.DiagError{}); errors.As(err, &derr) && len(diags) > 0 {
			// The warnings gathered before the failure still belong in it.
			fix := []string{}
			if derr.Diag.Fix != "" {
				fix = append(fix, derr.Diag.Fix)
			}
			_ = s.Emit(runx.Envelope{OK: false, Diagnostics: append(diags, derr.Diag), Next: fix})
		}
		if code := s.Finish(err); code != 0 {
			return runx.ExitError{Code: code}
		}
		return nil
	}

	var noCodex bool
	command := &cobra.Command{
		Use:   use,
		Short: "Every Claude Code session on this Mac by project, waiting-on-you first, dead ones proven",
		Long: `fleet reads Claude Code's session registry (~/.claude/sessions/*.json, never
written) and shows every session grouped by repository: waiting on you first,
then sessions whose process is proven gone while busy, then the busy, idle and
shell ones. A session is dead only when proven: its PID is gone, held by
something that cannot be Claude Code, or held by a process that started after
it did. Live Codex CLIs are listed read-only. The lazarus mod records the
background jobs a session starts through 'fleet ledger'; 'fleet revive' lists
the sessions that died with work in flight and the line that resumes each.
Contract: docs/fleet.md.`,
		Example: `  fleet                       # the fleet, waiting on you first
  fleet --json                # the snapshot envelope the /fleet pane reads
  fleet revive                # sessions that died with work in flight
  echo '{"kind":"workflow","id":"wf_1","status":"started"}' | fleet ledger record --session <id>
  fleet ledger show --session <id>
  fleet schema --ts           # the .d.ts of these contracts, for the mods`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session("snapshot")
			env, err := machine(!noCodex)
			if err != nil {
				return finish(s, nil, nil, nil, nil, err)
			}
			snap, diags, err := fleet.Read(cmd.Context(), env)
			next := []string{}
			if snap.Counts.Dead > 0 {
				next = append(next, "fleet revive")
			}
			return finish(s, snap, fleetLines(snap, env.Home), diags, next, err)
		},
	}
	command.Flags().BoolVar(&noCodex, "no-codex", false, "leave the Codex rows out (skips the ps/lsof/rollout read)")

	revive := &cobra.Command{
		Use:   "revive",
		Short: "Sessions that died while busy or waiting, or with background jobs open, and how to resume each",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session("revive")
			env, err := machine(false)
			if err != nil {
				return finish(s, nil, nil, nil, nil, err)
			}
			out, diags, err := fleet.ReadRevive(cmd.Context(), env)
			next := []string{}
			for _, dead := range out.Sessions {
				next = append(next, dead.Resume)
			}
			return finish(s, out, reviveLines(out, env.Home), diags, next, err)
		},
	}

	var sessionID string
	ledger := &cobra.Command{Use: "ledger", Short: "The per-session job ledger the lazarus mod writes", Args: cobra.NoArgs}
	record := &cobra.Command{
		Use:   "record --session <id>",
		Short: "Apply one job event read from stdin to the session's ledger",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session("ledger record")
			env, err := machine(false)
			if err != nil {
				return finish(s, nil, nil, nil, nil, err)
			}
			raw, err := io.ReadAll(io.LimitReader(rt.stdin, 1<<20))
			if err != nil {
				return finish(s, nil, nil, nil, nil, err)
			}
			ev, err := fleet.ReadEvent(raw)
			if err != nil {
				return finish(s, nil, nil, nil, nil, err)
			}
			out, err := fleet.Record(env, sessionID, ev)
			line := fmt.Sprintf("%s %s %s · %d open", ev.Kind, ev.ID, ev.Status, len(out.OpenJobs()))
			return finish(s, out, []string{line}, nil, nil, err)
		},
	}
	show := &cobra.Command{
		Use:   "show --session <id>",
		Short: "Print a session's ledger",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session("ledger show")
			env, err := machine(false)
			if err != nil {
				return finish(s, nil, nil, nil, nil, err)
			}
			out, err := fleet.Show(env, sessionID)
			return finish(s, out, ledgerLines(out), nil, nil, err)
		},
	}
	closeJobs := &cobra.Command{
		Use:   "close --session <id>",
		Short: "Mark every open job of a session stopped (dismissed, or resumed elsewhere)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session("ledger close")
			env, err := machine(false)
			if err != nil {
				return finish(s, nil, nil, nil, nil, err)
			}
			out, err := fleet.Close(env, sessionID)
			return finish(s, out, ledgerLines(out), nil, nil, err)
		},
	}
	for _, verb := range []*cobra.Command{record, show, closeJobs} {
		verb.Flags().StringVar(&sessionID, "session", "", "the Claude Code session id")
		ledger.AddCommand(verb)
	}

	var ts bool
	schema := &cobra.Command{
		Use:   "schema --ts",
		Short: "Print the TypeScript declarations of fleet's JSON contracts (mods keep a copy at types/fleet.gen.d.ts)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !ts {
				return finish(session("schema"), nil, nil, nil, nil, runx.DiagError{Diag: runx.Diagnostic{
					Code: fleet.DiagSchemaFormat, Severity: "error", Detail: "TypeScript is the one schema format", Fix: "fleet schema --ts"}})
			}
			_, err := io.WriteString(rt.stdout, fleet.TypeScript())
			return err
		},
	}
	schema.Flags().BoolVar(&ts, "ts", true, "emit TypeScript declarations")

	command.AddCommand(revive, ledger, schema)
	return command
}

// fleetLines renders the snapshot for a human: one section per state, the
// sessions that need the human first.
func fleetLines(snap fleet.Snapshot, home string) []string {
	c := snap.Counts
	lines := []string{fmt.Sprintf("fleet · %d sessions · %d waiting on you · %d busy · %d idle · %d dead", c.Total, c.Waiting, c.Busy, c.Idle, c.Dead)}
	sections := []struct {
		state fleet.State
		title string
	}{
		{fleet.StateWaiting, "waiting on you"},
		{fleet.StateDead, "dead while working — fleet revive"},
		{fleet.StateBusy, "busy"},
		{fleet.StateUnknown, "unknown status"},
		{fleet.StateIdle, "idle"},
		{fleet.StateShell, "at the shell"},
		{fleet.StateEnded, "ended"},
	}
	for _, section := range sections {
		var rows []string
		for _, s := range snap.Sessions {
			if s.State != section.state {
				continue
			}
			where := s.Name
			if s.Worktree != "" {
				where = s.Worktree
			}
			detail := s.WaitingFor
			if s.OpenJobs > 0 {
				detail = strings.TrimSpace(fmt.Sprintf("%s %d open job(s)", detail, s.OpenJobs))
			}
			rows = append(rows, fmt.Sprintf("  %-14s %-34s %-16s %6s", trunc(s.Project, 14), trunc(where, 34), trunc(detail, 16), fleetAge(time.Duration(s.AgeSeconds)*time.Second)))
		}
		if len(rows) == 0 {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s · %d", section.title, len(rows)))
		lines = append(lines, rows...)
	}
	if len(snap.Codex) > 0 {
		lines = append(lines, fmt.Sprintf("codex (read-only) · %d", len(snap.Codex)))
		for _, row := range snap.Codex {
			label := row.Name
			if label == "" {
				label = shortenHome(row.CWD, home)
			}
			lines = append(lines, fmt.Sprintf("  %-14s pid %-7d %s", trunc(row.Project, 14), row.PID, trunc(label, 40)))
		}
	}
	return lines
}

func reviveLines(out fleet.Revive, home string) []string {
	if len(out.Sessions) == 0 {
		return []string{"no session died with work in flight"}
	}
	lines := []string{fmt.Sprintf("%d session(s) died with work in flight", len(out.Sessions))}
	for _, s := range out.Sessions {
		lines = append(lines, fmt.Sprintf("  %s · %s · %s (%s, %s)", s.Project, shortenHome(s.CWD, home), s.SessionID, s.Liveness, s.Source))
		for _, job := range s.OpenJobs {
			lines = append(lines, fmt.Sprintf("      %s %s %s", job.Kind, job.ID, job.Description))
		}
		lines = append(lines, "    "+s.Resume)
	}
	return lines
}

func ledgerLines(ledger fleet.Ledger) []string {
	lines := []string{fmt.Sprintf("ledger %s · %d job(s), %d open", ledger.SessionID, len(ledger.Jobs), len(ledger.OpenJobs()))}
	for _, job := range ledger.Jobs {
		lines = append(lines, fmt.Sprintf("  %-10s %-9s %-24s %s", job.Kind, job.Status, trunc(job.ID, 24), job.Description))
	}
	return lines
}

// fleetAge is a compact age: 45s, 12m, 3h05m, 2d.
func fleetAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
