package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/plaud"
	"github.com/spf13/cobra"
)

func (rt *runtime) plaudApplet() *cobra.Command {
	command := rt.plaudCommand("plaud")
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetOut(rt.stdout)
	command.SetErr(rt.stderr)
	command.PersistentFlags().BoolVar(&rt.json, "json", false, "emit stable JSON output")
	return command
}

func (rt *runtime) plaudCommand(use string) *cobra.Command {
	command := &cobra.Command{
		Use:   use,
		Short: "Read Plaud recordings, notes and transcripts straight from the Plaud API",
		Long: `Talks to the Plaud developer API directly (no MCP server process). The
refresh token is never stored: it arrives as $` + plaud.RefreshTokenEnv + `, injected from
the onyx vault, and only the short-lived access token is cached at
~/.plaud/access-token.json (0600).

The vault suppresses the whole of a child's output whenever it injects the
token, so data commands pass --out and the caller reads the file: the result,
or {"error": ...} on failure. Never pass --out to login or refresh under an
onyx capture — the capture reads stdout.`,
	}
	command.AddCommand(
		rt.plaudLoginCommand(),
		rt.plaudRefreshCommand(),
		rt.plaudWhoamiCommand(),
		rt.plaudFilesCommand(),
		rt.plaudFileCommand(),
		rt.plaudNoteCommand(),
		rt.plaudTranscriptCommand(),
	)
	rt.bindOutFlag(command)
	return command
}

// plaudSession wires the vault-injected refresh token and the loud rotation
// notice every data command shares.
func (rt *runtime) plaudSession() (plaud.Session, error) {
	cfg, err := plaud.DefaultConfig()
	if err != nil {
		return plaud.Session{}, err
	}
	return plaud.Session{
		Config:       cfg,
		RefreshToken: os.Getenv(plaud.RefreshTokenEnv),
		Rotated: func(plaud.Token) {
			fmt.Fprintln(rt.stderr, "⚠️  plaud: Plaud rotated the refresh token during this call; the vault copy is now stale.")
			fmt.Fprintln(rt.stderr, "    Re-run `plaud refresh --json` through the onyx capture flow (see the plaud skill) to store the new one.")
		},
		Warn: func(err error) {
			fmt.Fprintf(rt.stderr, "⚠️  plaud: %v (the token still works; the next call refreshes again)\n", err)
		},
	}, nil
}

func (rt *runtime) plaudClient() (plaud.Client, error) {
	session, err := rt.plaudSession()
	if err != nil {
		return plaud.Client{}, err
	}
	return plaud.Client{Session: session}, nil
}

func (rt *runtime) plaudLoginCommand() *cobra.Command {
	var timeout time.Duration
	var noOpen bool
	command := &cobra.Command{
		Use:   "login",
		Short: "Run the browser consent flow and print the token response (stdout only)",
		Long: `Starts the PKCE authorization-code flow: prints the consent URL to stderr,
opens it, waits on the registered localhost callback, exchanges the code and
writes the raw token JSON to stdout. Designed for mcp__onyx__run_command with
capture {json_path: "refresh_token"} so the token never enters an agent's
context. Nothing is persisted.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := plaud.DefaultConfig()
			if err != nil {
				return err
			}
			opts := plaud.LoginOptions{Progress: rt.stderr, Timeout: timeout}
			if !noOpen {
				opts.Open = openBrowser
			}
			token, err := cfg.Login(cmd.Context(), opts)
			if err != nil {
				return err
			}
			return writeJSON(rt.stdout, token)
		},
	}
	command.Flags().DurationVar(&timeout, "timeout", 2*time.Minute, "how long to wait for the browser callback")
	command.Flags().BoolVar(&noOpen, "no-open", false, "only print the consent URL; do not launch a browser")
	return command
}

func (rt *runtime) plaudRefreshCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "refresh",
		Short: "Exchange $" + plaud.RefreshTokenEnv + " for an access token, cache it, print the token response",
		Long: `Prints the raw token JSON (including a rotated refresh_token when Plaud
issues one) so the onyx capture flow can update the vault. Only the access
token is cached on disk.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			session, err := rt.plaudSession()
			if err != nil {
				return err
			}
			if session.RefreshToken == "" {
				return plaud.ErrNoRefreshToken
			}
			token, err := session.Config.Refresh(cmd.Context(), session.RefreshToken)
			if err != nil {
				return err
			}
			// stdout first: the (possibly rotated) token must reach the capture
			// flow even when the cache write fails.
			if err := writeJSON(rt.stdout, token); err != nil {
				return err
			}
			if err := session.CacheAccessToken(token); err != nil {
				session.Warn(err)
			}
			return nil
		},
	}
}

func (rt *runtime) plaudWhoamiCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "whoami",
		Short: "Show the authenticated Plaud account",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := rt.plaudClient()
			if err != nil {
				return err
			}
			user, err := client.CurrentUser(cmd.Context())
			if err != nil {
				return err
			}
			if rt.json {
				return writeJSON(rt.stdout, user)
			}
			for _, key := range []string{"email", "name", "nickname", "id", "user_id"} {
				if v, ok := user[key]; ok && v != nil && v != "" {
					fmt.Fprintf(rt.stdout, "%-8s %v\n", key, v)
				}
			}
			return nil
		},
	}
}

func (rt *runtime) plaudFilesCommand() *cobra.Command {
	var opts plaud.ListOptions
	command := &cobra.Command{
		Use:   "files",
		Short: "List recordings (newest first); --query scans names case-insensitively",
		Example: `  plaud files
  plaud files --query "weekly" --json | jq '.data[].id'
  plaud files --page 2 --page-size 50`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := rt.plaudClient()
			if err != nil {
				return err
			}
			list, err := client.ListFiles(cmd.Context(), opts)
			if err != nil {
				return err
			}
			if rt.json {
				if opts.Query == "" {
					return writeJSON(rt.stdout, list.Raw)
				}
				return writeJSON(rt.stdout, list)
			}
			fmt.Fprintf(rt.stdout, "%-26s %-20s %-8s %s\n", "ID", "CREATED", "LENGTH", "NAME")
			for _, item := range list.Data {
				fmt.Fprintf(rt.stdout, "%-26s %-20s %-8s %s\n",
					str(item["id"]), trunc(str(item["created_at"]), 20), plaudDuration(item["duration"]), str(item["name"]))
			}
			if opts.Query != "" {
				fmt.Fprintf(rt.stdout, "%d of %d scanned match %q", list.Matched, list.Scanned, opts.Query)
				if list.Truncated {
					fmt.Fprint(rt.stdout, "   (scan capped — narrow the query)")
				}
				fmt.Fprintln(rt.stdout)
			}
			return nil
		},
	}
	command.Flags().StringVar(&opts.Query, "query", "", "case-insensitive substring of the recording name")
	command.Flags().IntVar(&opts.Page, "page", 1, "page number (ignored with --query)")
	command.Flags().IntVar(&opts.PageSize, "page-size", 20, "items per page (ignored with --query)")
	return command
}

func (rt *runtime) plaudFileCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "file <id>",
		Short: "Show one recording's metadata and available blocks",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.plaudClient()
			if err != nil {
				return err
			}
			file, err := client.GetFile(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			if rt.json {
				return writeJSON(rt.stdout, file.Raw)
			}
			fmt.Fprintf(rt.stdout, "id       %s\nname     %s\ncreated  %s\nlength   %s\n", file.ID, file.Name, file.CreatedAt, plaudDuration(file.Duration))
			blocks := make([]string, 0, len(file.SourceList))
			for _, b := range file.SourceList {
				blocks = append(blocks, b.DataType)
			}
			fmt.Fprintf(rt.stdout, "blocks   %s\n", strings.Join(blocks, ", "))
			return nil
		},
	}
}

func (rt *runtime) plaudNoteCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "note <id>",
		Short: "Print the AI notes (summary, action items, topics) of a recording",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.plaudClient()
			if err != nil {
				return err
			}
			file, err := client.GetFile(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			notes := file.NoteList
			if len(notes) == 0 {
				notes = json.RawMessage("[]")
			}
			if rt.json {
				var v any
				if err := json.Unmarshal(notes, &v); err != nil {
					return err
				}
				return writeJSON(rt.stdout, v)
			}
			var items []map[string]any
			if err := json.Unmarshal(notes, &items); err != nil {
				fmt.Fprintln(rt.stdout, string(notes))
				return nil
			}
			for _, note := range items {
				if title := str(note["title"]); title != "" {
					fmt.Fprintf(rt.stdout, "## %s\n\n", title)
				}
				for _, key := range []string{"content", "data_content", "text", "summary"} {
					if body := str(note[key]); body != "" {
						fmt.Fprintln(rt.stdout, body)
						fmt.Fprintln(rt.stdout)
						break
					}
				}
			}
			return nil
		},
	}
}

func (rt *runtime) plaudTranscriptCommand() *cobra.Command {
	var block string
	command := &cobra.Command{
		Use:   "transcript <id>",
		Short: "Print the full transcript of a recording (speaker + timestamp per line)",
		Long: `Loads the whole block in one go — Plaud stores it as a single document, so
there is nothing to paginate. Blocks: ` + strings.Join(plaud.TranscriptBlocks, ", ") + `.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := rt.plaudClient()
			if err != nil {
				return err
			}
			transcript, err := client.Transcript(cmd.Context(), args[0], block)
			if err != nil {
				return err
			}
			if rt.json {
				return writeJSON(rt.stdout, transcript)
			}
			if transcript.Segments == nil {
				fmt.Fprintln(rt.stdout, transcript.Text)
				return nil
			}
			for _, seg := range transcript.Segments {
				fmt.Fprintf(rt.stdout, "[%s] %s: %s\n", plaudStamp(seg), firstStr(seg, "speaker", "speaker_name", "speaker_id"), firstStr(seg, "content", "text"))
			}
			return nil
		},
	}
	command.Flags().StringVar(&block, "block", plaud.TranscriptBlocks[0], "source block to print")
	return command
}

func openBrowser(target string) error {
	var cmd *exec.Cmd
	switch goruntime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}

func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func firstStr(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if s := str(m[key]); s != "" {
			return s
		}
	}
	return "?"
}

// plaudDuration renders seconds (or milliseconds when clearly too large for
// seconds) as h:mm:ss.
func plaudDuration(v any) string {
	n, ok := v.(float64)
	if !ok || n <= 0 {
		return "-"
	}
	if n > 100_000 { // > ~27h: this is milliseconds
		n /= 1000
	}
	d := time.Duration(n * float64(time.Second))
	return fmt.Sprintf("%d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
}

// plaudStamp renders a segment's start time; Plaud utterances carry
// milliseconds in start_time.
func plaudStamp(seg map[string]any) string {
	for _, key := range []string{"start_time", "start", "begin_time", "timestamp"} {
		if n, ok := seg[key].(float64); ok {
			d := time.Duration(n) * time.Millisecond
			return fmt.Sprintf("%02d:%02d:%02d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60)
		}
	}
	return "--:--:--"
}
