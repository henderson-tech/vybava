package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/posta"
	"github.com/spf13/cobra"
)

func (rt *runtime) postaApplet() *cobra.Command {
	cmd := rt.postaCommand("posta")
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(rt.stdout)
	cmd.SetErr(rt.stderr)
	cmd.PersistentFlags().BoolVar(&rt.json, "json", false, "emit machine-readable output")
	return cmd
}

func (rt *runtime) postaCommand(use string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: "Drive the shared AI test mailbox — mint a per-run address, wait for the mail, take its links and files",
		Long: strings.TrimSpace(`
Drive the shared AI test mailbox so an agent can complete a real email journey:
sign-up verification, password reset, an attachment round trip.

The mailbox address and app password arrive in POSTA_ADDRESS and
POSTA_APP_PASSWORD. Inject them with the vault at the point of use — never a
literal, never a file:

  onyx run_command --env-refs POSTA_APP_PASSWORD=onyx://<group>/<item>/app_password -- posta wait --to ...

Mint one address per run with ` + "`posta address`" + `. A freshly minted address has
never received mail, so a run cannot be satisfied by a previous run's message,
and two agents running the same journey never read each other's links.

The vault suppresses the whole of a child's output whenever it injects a secret,
so pass --out to send the result — or {"error": ...} on failure — to a file and
read that file afterwards:

  onyx run_command ... -- posta wait --to <addr> --json --out /tmp/mail.json`),
	}
	cmd.AddCommand(
		rt.postaAddressCommand(),
		rt.postaWaitCommand(),
		rt.postaListCommand(),
		rt.postaLinksCommand(),
		rt.postaAttachCommand(),
		rt.postaSendCommand(),
		rt.postaPurgeCommand(),
		rt.postaDoctorCommand(),
	)
	// --out is not a convenience: the vault redacts the whole output once it
	// injects a credential, so the file is the only way back.
	rt.bindOutFlag(cmd)
	return cmd
}

// query is the selector every reading subcommand shares.
type postaQuery struct {
	to      string
	subject string
	since   string
	mailbox string
}

func (q *postaQuery) bind(cmd *cobra.Command) {
	cmd.Flags().StringVar(&q.to, "to", "", "exact recipient address, including its +tag (required)")
	cmd.Flags().StringVar(&q.subject, "subject", "", "regular expression the subject must match")
	cmd.Flags().StringVar(&q.since, "since", "10m", "only mail delivered after this: a duration ago (10m) or an RFC3339 time")
	cmd.Flags().StringVar(&q.mailbox, "mailbox", posta.DefaultMailbox, "IMAP folder to read")
	_ = cmd.MarkFlagRequired("to")
}

func (q *postaQuery) build() (posta.Query, error) {
	since, err := parseSince(q.since)
	if err != nil {
		return posta.Query{}, err
	}
	query := posta.Query{Recipient: strings.TrimSpace(q.to), Since: since, Mailbox: q.mailbox}
	if q.subject != "" {
		pattern, err := regexp.Compile(q.subject)
		if err != nil {
			return posta.Query{}, fmt.Errorf("--subject is not a valid regular expression: %w", err)
		}
		query.Subject = pattern
	}
	return query, nil
}

// parseSince accepts a duration ago or an absolute time. Empty means no cutoff,
// which is deliberate but rarely what a journey wants.
func parseSince(value string) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "any" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(value); err == nil {
		return time.Now().Add(-d), nil
	}
	at, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("--since %q is neither a duration (10m) nor an RFC3339 time", value)
	}
	return at, nil
}

func (rt *runtime) postaOpen() (*posta.Mailbox, posta.Credentials, error) {
	creds, err := posta.CredentialsFromEnv()
	if err != nil {
		return nil, posta.Credentials{}, err
	}
	box, err := posta.Dial(creds)
	if err != nil {
		return nil, posta.Credentials{}, err
	}
	return box, creds, nil
}

func (rt *runtime) postaAddressCommand() *cobra.Command {
	var project, role, run string
	cmd := &cobra.Command{
		Use:   "address",
		Short: "Mint the delivery address one test run owns",
		Args:  cobra.NoArgs,
	}
	cmd.Flags().StringVar(&project, "project", "", "the application under test (required)")
	cmd.Flags().StringVar(&role, "role", "", "the persona, e.g. customer or worker")
	cmd.Flags().StringVar(&run, "run", "", "run id; omitted means mint a fresh random one")
	_ = cmd.MarkFlagRequired("project")
	cmd.RunE = func(command *cobra.Command, _ []string) error {
		creds, err := posta.CredentialsFromEnv()
		if err != nil {
			return err
		}
		if strings.TrimSpace(run) == "" {
			run = posta.RunID()
		}
		address, err := posta.Address(creds.Address, project, role, run)
		if err != nil {
			return err
		}
		if rt.json {
			return json.NewEncoder(rt.stdout).Encode(struct {
				Address string `json:"address"`
				Project string `json:"project"`
				Role    string `json:"role,omitempty"`
				Run     string `json:"run"`
			}{address, project, role, run})
		}
		_, err = fmt.Fprintln(rt.stdout, address)
		return err
	}
	return cmd
}

func (rt *runtime) postaWaitCommand() *cobra.Command {
	var q postaQuery
	var timeout, poll time.Duration
	var save string
	cmd := &cobra.Command{
		Use:   "wait",
		Short: "Block until the awaited message arrives, then print it",
		Args:  cobra.NoArgs,
	}
	q.bind(cmd)
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Minute, "give up after this long")
	cmd.Flags().DurationVar(&poll, "poll", 3*time.Second, "how often to re-check the mailbox")
	cmd.Flags().StringVar(&save, "save", "", "directory to write attachments into")
	cmd.RunE = func(command *cobra.Command, _ []string) error {
		query, err := q.build()
		if err != nil {
			return err
		}
		box, _, err := rt.postaOpen()
		if err != nil {
			return err
		}
		defer func() { _ = box.Close() }()
		ctx, cancel := context.WithTimeout(command.Context(), timeout)
		defer cancel()
		message, err := box.Wait(ctx, query, poll)
		if err != nil {
			return err
		}
		if save != "" {
			if err := message.Save(save); err != nil {
				return err
			}
		}
		return rt.postaPrintMessages([]*posta.Message{message})
	}
	return cmd
}

func (rt *runtime) postaListCommand() *cobra.Command {
	var q postaQuery
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List every message already delivered to an address, newest first",
		Args:  cobra.NoArgs,
	}
	q.bind(cmd)
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		query, err := q.build()
		if err != nil {
			return err
		}
		box, _, err := rt.postaOpen()
		if err != nil {
			return err
		}
		defer func() { _ = box.Close() }()
		messages, err := box.Search(query)
		if err != nil {
			return err
		}
		return rt.postaPrintMessages(messages)
	}
	return cmd
}

func (rt *runtime) postaLinksCommand() *cobra.Command {
	var q postaQuery
	var match string
	var timeout, poll time.Duration
	cmd := &cobra.Command{
		Use:   "links",
		Short: "Wait for the message and print only its URLs — the verification or reset link",
		Args:  cobra.NoArgs,
	}
	q.bind(cmd)
	cmd.Flags().StringVar(&match, "match", "", "keep only links matching this regular expression")
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Minute, "give up after this long")
	cmd.Flags().DurationVar(&poll, "poll", 3*time.Second, "how often to re-check the mailbox")
	cmd.RunE = func(command *cobra.Command, _ []string) error {
		query, err := q.build()
		if err != nil {
			return err
		}
		var keep *regexp.Regexp
		if match != "" {
			if keep, err = regexp.Compile(match); err != nil {
				return fmt.Errorf("--match is not a valid regular expression: %w", err)
			}
		}
		box, _, err := rt.postaOpen()
		if err != nil {
			return err
		}
		defer func() { _ = box.Close() }()
		ctx, cancel := context.WithTimeout(command.Context(), timeout)
		defer cancel()
		message, err := box.Wait(ctx, query, poll)
		if err != nil {
			return err
		}
		links := message.Links
		if keep != nil {
			filtered := []string{}
			for _, link := range links {
				if keep.MatchString(link) {
					filtered = append(filtered, link)
				}
			}
			links = filtered
		}
		if len(links) == 0 {
			return fmt.Errorf("message %q carried no matching link", message.Subject)
		}
		if rt.json {
			return json.NewEncoder(rt.stdout).Encode(links)
		}
		for _, link := range links {
			if _, err := fmt.Fprintln(rt.stdout, link); err != nil {
				return err
			}
		}
		return nil
	}
	return cmd
}

func (rt *runtime) postaAttachCommand() *cobra.Command {
	var q postaQuery
	var save string
	var timeout, poll time.Duration
	cmd := &cobra.Command{
		Use:   "attach",
		Short: "Wait for the message and write its attachments to disk",
		Args:  cobra.NoArgs,
	}
	q.bind(cmd)
	cmd.Flags().StringVar(&save, "save", "", "directory to write attachments into (required)")
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Minute, "give up after this long")
	cmd.Flags().DurationVar(&poll, "poll", 3*time.Second, "how often to re-check the mailbox")
	_ = cmd.MarkFlagRequired("save")
	cmd.RunE = func(command *cobra.Command, _ []string) error {
		query, err := q.build()
		if err != nil {
			return err
		}
		box, _, err := rt.postaOpen()
		if err != nil {
			return err
		}
		defer func() { _ = box.Close() }()
		ctx, cancel := context.WithTimeout(command.Context(), timeout)
		defer cancel()
		message, err := box.Wait(ctx, query, poll)
		if err != nil {
			return err
		}
		if len(message.Attachments) == 0 {
			return fmt.Errorf("message %q carried no attachment", message.Subject)
		}
		if err := message.Save(save); err != nil {
			return err
		}
		if rt.json {
			return json.NewEncoder(rt.stdout).Encode(message.Attachments)
		}
		for _, attachment := range message.Attachments {
			if _, err := fmt.Fprintf(rt.stdout, "%s\t%d bytes\t%s\n", attachment.Path, attachment.Size, attachment.ContentType); err != nil {
				return err
			}
		}
		return nil
	}
	return cmd
}

func (rt *runtime) postaSendCommand() *cobra.Command {
	var to []string
	var subject, body string
	var attach []string
	cmd := &cobra.Command{
		Use:   "send",
		Short: "Send mail from the test mailbox",
		Args:  cobra.NoArgs,
	}
	cmd.Flags().StringSliceVar(&to, "to", nil, "recipient (repeatable, required)")
	cmd.Flags().StringVar(&subject, "subject", "", "subject line")
	cmd.Flags().StringVar(&body, "body", "", "plain-text body")
	cmd.Flags().StringSliceVar(&attach, "attach", nil, "file to attach (repeatable)")
	_ = cmd.MarkFlagRequired("to")
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		creds, err := posta.CredentialsFromEnv()
		if err != nil {
			return err
		}
		if err := posta.Send(creds, posta.Outgoing{To: to, Subject: subject, Body: body, Attach: attach}); err != nil {
			return err
		}
		if rt.json {
			return json.NewEncoder(rt.stdout).Encode(struct {
				Sent bool     `json:"sent"`
				To   []string `json:"to"`
			}{true, to})
		}
		_, err = fmt.Fprintf(rt.stdout, "Sent to %s\n", strings.Join(to, ", "))
		return err
	}
	return cmd
}

func (rt *runtime) postaPurgeCommand() *cobra.Command {
	var q postaQuery
	cmd := &cobra.Command{
		Use:   "purge",
		Short: "Delete a run's mail so the next run starts from an empty address",
		Long:  "Refuses any recipient without a +tag, so it cannot delete mail a human sent or received.",
		Args:  cobra.NoArgs,
	}
	q.bind(cmd)
	// A purge is scoped by its address, not by time: the point is to empty a run's
	// inbox completely, including the mail that predates the default window.
	q.since = "any"
	cmd.Flags().Lookup("since").DefValue = "any"
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		query, err := q.build()
		if err != nil {
			return err
		}
		box, _, err := rt.postaOpen()
		if err != nil {
			return err
		}
		defer func() { _ = box.Close() }()
		deleted, err := box.Purge(query)
		if err != nil {
			if errors.Is(err, posta.ErrUntaggedPurge) {
				return fmt.Errorf("%w — purge only ever touches an address posta minted", err)
			}
			return err
		}
		if rt.json {
			return json.NewEncoder(rt.stdout).Encode(struct {
				Deleted int    `json:"deleted"`
				To      string `json:"to"`
			}{deleted, query.Recipient})
		}
		_, err = fmt.Fprintf(rt.stdout, "Deleted %d message(s) for %s\n", deleted, query.Recipient)
		return err
	}
	return cmd
}

func (rt *runtime) postaDoctorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Prove IMAP and SMTP both authenticate, without sending anything",
		Args:  cobra.NoArgs,
	}
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		creds, err := posta.CredentialsFromEnv()
		if err != nil {
			return err
		}
		report := struct {
			Address string `json:"address"`
			IMAP    string `json:"imap"`
			SMTP    string `json:"smtp"`
			IMAPOK  bool   `json:"imapOk"`
			SMTPOK  bool   `json:"smtpOk"`
			Error   string `json:"error,omitempty"`
		}{Address: creds.Address, IMAP: creds.IMAPAddr, SMTP: creds.SMTPAddr}

		box, err := posta.Dial(creds)
		if err != nil {
			report.Error = err.Error()
		} else {
			report.IMAPOK = true
			_ = box.Close()
		}
		if err := posta.CheckSMTP(creds); err != nil {
			report.SMTPOK = false
			if report.Error == "" {
				report.Error = err.Error()
			}
		} else {
			report.SMTPOK = true
		}
		if rt.json {
			if err := json.NewEncoder(rt.stdout).Encode(report); err != nil {
				return err
			}
		} else {
			fmt.Fprintf(rt.stdout, "mailbox %s\n  IMAP %s %s\n  SMTP %s %s\n",
				report.Address, report.IMAP, verdict(report.IMAPOK), report.SMTP, verdict(report.SMTPOK))
			if report.Error != "" {
				fmt.Fprintf(rt.stdout, "  %s\n", report.Error)
			}
		}
		if !report.IMAPOK || !report.SMTPOK {
			return errors.New("mailbox is not usable")
		}
		return nil
	}
	return cmd
}

func verdict(ok bool) string {
	if ok {
		return "OK"
	}
	return "FAILED"
}

// postaPrintMessages is the one place a message becomes output, so the JSON and
// the human rendering can never drift apart.
func (rt *runtime) postaPrintMessages(messages []*posta.Message) error {
	if rt.json {
		if messages == nil {
			messages = []*posta.Message{}
		}
		return json.NewEncoder(rt.stdout).Encode(messages)
	}
	if len(messages) == 0 {
		_, err := fmt.Fprintln(rt.stdout, "No matching message.")
		return err
	}
	for _, message := range messages {
		fmt.Fprintf(rt.stdout, "From:    %s\nTo:      %s\nSubject: %s\nDate:    %s\n",
			message.From, message.DeliveredTo, message.Subject, message.Date.Format(time.RFC3339))
		for _, link := range message.Links {
			fmt.Fprintf(rt.stdout, "Link:    %s\n", link)
		}
		for _, attachment := range message.Attachments {
			fmt.Fprintf(rt.stdout, "File:    %s (%d bytes)\n", attachment.Filename, attachment.Size)
		}
		if body := strings.TrimSpace(message.Text); body != "" {
			fmt.Fprintf(rt.stdout, "\n%s\n", body)
		}
		fmt.Fprintln(rt.stdout, "---")
	}
	return nil
}
