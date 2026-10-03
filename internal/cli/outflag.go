package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// bindOutFlag gives an applet that runs under `onyx run_command` a persistent
// --out <file>. Once the vault injects a credential it redacts the child's
// whole stdout and stderr, so the file is the only way the caller learns
// anything: the result on success; on failure {"error": "...", "output": "..."}
// with whatever the subcommand had already written (a diagnostic report, a
// half-written result). The file is owner-only — it carries transcripts and
// reset links — and its parent directory is created when missing, since a
// failure to open it would be as silent as the redaction it works around.
// Call it after the subcommands are added — it wraps their RunE.
func (rt *runtime) bindOutFlag(root *cobra.Command) {
	var outPath string
	var outFile *os.File
	root.PersistentFlags().StringVar(&outPath, "out", "", `write the result to this file (0600) instead of stdout ({"error": ...} on failure)`)
	root.PersistentPreRunE = func(_ *cobra.Command, _ []string) error {
		if outPath == "" {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(outPath), 0o700); err != nil {
			return err
		}
		file, err := os.OpenFile(outPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
		if err != nil {
			return err
		}
		// OpenFile keeps an existing file's mode; a reused path must not stay readable.
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			return err
		}
		outFile = file
		rt.stdout = file
		return nil
	}
	root.PersistentPostRunE = func(_ *cobra.Command, _ []string) error {
		if outFile == nil {
			return nil
		}
		return outFile.Close()
	}
	recordFailure := func(failure error) {
		if outFile == nil {
			return
		}
		defer outFile.Close()
		// What the subcommand wrote moves under "output": it may be the only
		// diagnosis (posta doctor's report), yet must not read as success.
		if _, err := outFile.Seek(0, io.SeekStart); err != nil {
			return
		}
		written, err := io.ReadAll(outFile)
		if err != nil {
			return
		}
		if err := outFile.Truncate(0); err != nil {
			return
		}
		if _, err := outFile.Seek(0, io.SeekStart); err != nil {
			return
		}
		_ = json.NewEncoder(outFile).Encode(struct {
			Error  string `json:"error"`
			Output string `json:"output,omitempty"`
		}{failure.Error(), string(written)})
	}
	var wrap func(*cobra.Command)
	wrap = func(cmd *cobra.Command) {
		for _, sub := range cmd.Commands() {
			wrap(sub)
		}
		if run := cmd.RunE; run != nil {
			cmd.RunE = func(c *cobra.Command, args []string) error {
				err := run(c, args)
				if err != nil {
					recordFailure(err)
				}
				return err
			}
		}
	}
	wrap(root)
}
