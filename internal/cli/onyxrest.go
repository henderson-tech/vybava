package cli

import (
	"fmt"
	"net/http"
	"os"

	"github.com/henderson-tech/vybava/internal/onyxrest"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/spf13/cobra"
)

func (rt *runtime) onyxRestApplet() *cobra.Command {
	command := rt.onyxRestCommand("onyx-rest")
	command.SilenceUsage, command.SilenceErrors = true, true
	command.SetOut(rt.stdout)
	command.SetErr(rt.stderr)
	return command
}

// onyxRestClient is swapped by tests for an httptest TLS client.
var onyxRestClient = &http.Client{}

// onyxRestCommand parses its own argv: the Onyx binding is a string prefix of
// it, so cobra's reordering flag parser must never see it.
func (rt *runtime) onyxRestCommand(use string) *cobra.Command {
	return &cobra.Command{
		Use:                use + " --base https://<host>/ [--basic-user <user>] <METHOD> <PATH> [--data <json>|@<file>] --out <file>",
		Short:              "One REST call with a secret Onyx injects as " + onyxrest.TokenEnv + "; the response goes to a 0600 --out file",
		DisableFlagParsing: true,
		RunE: func(command *cobra.Command, args []string) error {
			if len(args) == 1 && (args[0] == "-h" || args[0] == "--help") {
				fmt.Fprintln(rt.stdout, onyxrest.Usage)
				return nil
			}
			req, err := onyxrest.Parse(args)
			if err != nil {
				return err
			}
			status, err := onyxrest.Do(command.Context(), onyxRestClient, req, os.Getenv(onyxrest.TokenEnv))
			if err != nil {
				return err
			}
			if status < 200 || status > 299 {
				fmt.Fprintf(rt.stderr, "onyx-rest: HTTP %d, response in %s\n", status, req.Out)
				return runx.ExitError{Code: 1}
			}
			return nil
		},
	}
}
