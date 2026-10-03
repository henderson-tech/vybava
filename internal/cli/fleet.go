package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/cmux"
	"github.com/henderson-tech/vybava/internal/fleet"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/spf13/cobra"
)

// cmuxBundleID is what `fleet focus` activates after cmux focused the surface.
const cmuxBundleID = "com.cmuxterm.app"

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
		env := fleet.Env{Home: home, Now: time.Now(), Cmux: cmux.Client{Socket: cmux.SocketPath(os.Getenv, home)},
			Activate: func(ctx context.Context) error {
				return exec.CommandContext(ctx, "open", "-b", cmuxBundleID).Run()
			}}
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

	var ts, swift, example bool
	schema := &cobra.Command{
		Use:   "schema --ts|--swift|--example",
		Short: "Print fleet's JSON contracts: TypeScript for the mods, Swift for Fleet.app, or a sample of every Fleet.app contract",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			switch {
			case swift:
				_, err := io.WriteString(rt.stdout, fleet.Swift())
				return err
			case example:
				body, err := json.MarshalIndent(fleet.Example(), "", "  ")
				if err == nil {
					_, err = rt.stdout.Write(append(body, '\n'))
				}
				return err
			case ts:
				_, err := io.WriteString(rt.stdout, fleet.TypeScript())
				return err
			}
			return finish(session("schema"), nil, nil, nil, nil, runx.DiagError{Diag: runx.Diagnostic{
				Code: fleet.DiagSchemaFormat, Severity: "error", Detail: "name a format", Fix: "fleet schema --ts"}})
		},
	}
	schema.Flags().BoolVar(&ts, "ts", true, "emit TypeScript declarations (the mods' types/fleet.gen.d.ts)")
	schema.Flags().BoolVar(&swift, "swift", false, "emit the Swift contracts (Fleet.app's FleetSnapshot.swift)")
	schema.Flags().BoolVar(&example, "example", false, "emit a sample of every contract Fleet.app decodes")

	var target fleet.Target
	var lines int
	screen := &cobra.Command{
		Use:   "screen --session <id>",
		Short: "A session's terminal screen through cmux, secrets redacted, with the dialog open on it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session("screen")
			env, err := machine(false)
			if err != nil {
				return finish(s, nil, nil, nil, nil, err)
			}
			out, err := fleet.Screen(cmd.Context(), env, target.SessionID, lines)
			return finish(s, out, []string{out.Text}, nil, nil, err)
		},
	}
	screen.Flags().IntVar(&lines, "lines", 0, "read this many trailing lines including scrollback (default: the visible screen)")
	dialog := &cobra.Command{
		Use:   "dialog --session <id>",
		Short: "The permission or question dialog open on a session's screen, its options and fingerprint",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session("dialog")
			env, err := machine(false)
			if err != nil {
				return finish(s, nil, nil, nil, nil, err)
			}
			out, err := fleet.OpenDialog(cmd.Context(), env, target.SessionID)
			return finish(s, out, dialogLines(out), nil, nil, err)
		},
	}
	var reply fleet.Reply
	replyCmd := &cobra.Command{
		Use:   "reply --session <id> [--option <key> --expect <fingerprint>]",
		Short: "Answer a waiting session in place: text from stdin for a plain wait, or one dialog option",
		Long: `reply answers a Claude session through its cmux surface, resolved by live pid
now. Without --option it reads the text from stdin, pastes it once and
submits it — refused while any dialog is open, so typed text can never
approve a tool call. With --option it presses that one key, only while the
screen still shows the dialog --expect names (fleet dialog prints it). Codex
sessions are read-only.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session("reply")
			env, err := machine(false)
			if err != nil {
				return finish(s, nil, nil, nil, nil, err)
			}
			if reply.Option == "" {
				raw, err := io.ReadAll(io.LimitReader(rt.stdin, fleet.MaxReplyBytes+1))
				if err != nil {
					return finish(s, nil, nil, nil, nil, err)
				}
				reply.Text = string(raw)
			}
			out, err := fleet.SendReply(cmd.Context(), env, target, reply)
			line := fmt.Sprintf("sent %d bytes to %s", out.Bytes, out.SessionID)
			if out.Mode == "option" {
				line = fmt.Sprintf("pressed %s in %s", out.Key, out.SessionID)
			}
			return finish(s, out, []string{line}, nil, nil, err)
		},
	}
	replyCmd.Flags().StringVar(&reply.Option, "option", "", "the dialog option key to press")
	replyCmd.Flags().StringVar(&reply.Expect, "expect", "", "fingerprint of the dialog the option was chosen from")
	focus := &cobra.Command{
		Use:   "focus --session <id> | --pid <n>",
		Short: "Bring cmux to a session's window, workspace and surface (Codex rows by --pid)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session("focus")
			env, err := machine(false)
			if err != nil {
				return finish(s, nil, nil, nil, nil, err)
			}
			out, err := fleet.Focus(cmd.Context(), env, target)
			return finish(s, out, []string{"focused " + out.SurfaceID}, nil, nil, err)
		},
	}
	for _, verb := range []*cobra.Command{screen, dialog, replyCmd, focus} {
		verb.Flags().StringVar(&target.SessionID, "session", "", "the Claude Code session id")
	}
	for _, verb := range []*cobra.Command{replyCmd, focus} {
		verb.Flags().IntVar(&target.PID, "pid", 0, "an agent process id (Codex rows)")
	}

	var publishCodex bool
	publish := &cobra.Command{
		Use:   "publish",
		Short: "Write snapshot.json once, as watch serve does every 15 s (for Fleet.app without the daemon)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session("publish")
			env, err := machine(publishCodex)
			if err != nil {
				return finish(s, nil, nil, nil, nil, err)
			}
			p := &fleet.Publisher{Env: env, Path: fleet.PublishedPath(env.Home), Clock: time.Now, ReadCodex: env.Codex}
			p.RefreshCodex(cmd.Context())
			view, err := p.Build(cmd.Context())
			if err == nil {
				if werr := fleet.WritePublished(p.Path, view); werr != nil {
					err = runx.DiagError{Diag: runx.Diagnostic{Code: fleet.DiagSnapshotUnwritable, Severity: "error", Detail: werr.Error()}}
				}
			}
			line := fmt.Sprintf("%s · %d sessions, %d waiting, %d codex, %d hidden · cmux %s", p.Path, len(view.Sessions), view.Counts.Waiting, len(view.Codex), view.Hidden, view.Cmux.State)
			return finish(s, view, []string{line}, view.Diagnostics, nil, err)
		},
	}
	publish.Flags().BoolVar(&publishCodex, "codex", false, "include the Codex rows (a ~10 s ps/lsof read)")

	command.AddCommand(revive, ledger, schema, screen, dialog, replyCmd, focus, publish)
	return command
}

func dialogLines(d *fleet.Dialog) []string {
	if d == nil {
		return []string{"no dialog open"}
	}
	lines := []string{fmt.Sprintf("%s · %s · fingerprint %s", d.Kind, d.Question, d.Fingerprint)}
	if d.Title != "" {
		lines = append(lines, "  "+d.Title)
	}
	for _, o := range d.Options {
		mark := " "
		if o.Selected {
			mark = "❯"
		}
		lines = append(lines, fmt.Sprintf("  %s %s. %s (%s)", mark, o.Key, o.Label, o.Kind))
	}
	return lines
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
