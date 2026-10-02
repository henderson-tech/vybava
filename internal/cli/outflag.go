package cli

import (
	"encoding/json"
	"io"
	"os"

	"github.com/spf13/cobra"
)

// bindOutFlag gives an applet that runs under `onyx run_command` a persistent
// --out <file>. Once the vault injects a credential it redacts the child's
// whole stdout and stderr, so the file is the only way the caller learns
// anything: the result on success, {"error": "..."} when a subcommand fails.
// Call it after the subcommands are added — it wraps their RunE.
func (rt *runtime) bindOutFlag(root *cobra.Command) {
	var outPath string
	var outFile *os.File
	root.PersistentFlags().StringVar(&outPath, "out", "", `write the result to this file instead of stdout ({"error": ...} on failure)`)
	root.PersistentPreRunE = func(_ *cobra.Command, _ []string) error {
		if outPath == "" {
			return nil
		}
		file, err := os.Create(outPath)
		if err != nil {
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
		// A half-written result must not read as success.
		if err := outFile.Truncate(0); err != nil {
			return
		}
		if _, err := outFile.Seek(0, io.SeekStart); err != nil {
			return
		}
		_ = json.NewEncoder(outFile).Encode(struct {
			Error string `json:"error"`
		}{failure.Error()})
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
