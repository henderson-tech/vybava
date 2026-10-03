package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

// A subcommand that reports before failing (posta doctor names the broken
// IMAP/SMTP side, then returns a generic error) must not lose that report:
// under vault redaction the file is the only diagnosis the caller gets.
func TestOutFlagKeepsWhatWasWrittenBeforeAFailure(t *testing.T) {
	var buf bytes.Buffer
	rt := &runtime{stdout: &buf, stderr: &buf}
	root := &cobra.Command{Use: "applet", SilenceErrors: true, SilenceUsage: true}
	root.AddCommand(&cobra.Command{
		Use: "doctor",
		RunE: func(_ *cobra.Command, _ []string) error {
			fmt.Fprintln(rt.stdout, "IMAP fail: auth rejected")
			return errors.New("mailbox is not usable")
		},
	})
	rt.bindOutFlag(root)
	path := filepath.Join(t.TempDir(), "doctor.json")

	root.SetArgs([]string{"doctor", "--out", path})
	if err := root.Execute(); err == nil {
		t.Fatal("doctor must fail")
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Error  string `json:"error"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(written, &envelope); err != nil {
		t.Fatalf("file = %q: %v", written, err)
	}
	if envelope.Error != "mailbox is not usable" || envelope.Output != "IMAP fail: auth rejected\n" {
		t.Fatalf("envelope = %+v", envelope)
	}
}
