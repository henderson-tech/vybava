package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/henderson-tech/vybava/internal/cmuxgrid"
	"github.com/spf13/cobra"
)

func (rt *runtime) cmuxGridApplet() *cobra.Command {
	cmd := rt.cmuxGridCommand()
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(rt.stdout)
	cmd.SetErr(rt.stderr)
	cmd.PersistentFlags().BoolVar(&rt.json, "json", false, "emit machine-readable output")
	return cmd
}

func (rt *runtime) cmuxGridCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "cmux-grid", Short: "Open a fresh monitor-sized cmux terminal grid"}
	var width, height int
	var screen, socket, cli string
	cmd.PersistentFlags().IntVar(&width, "width", 0, "monitor width in macOS points")
	cmd.PersistentFlags().IntVar(&height, "height", 0, "monitor height in macOS points")
	cmd.PersistentFlags().StringVar(&screen, "screen", "", "monitor name (Pro Display XDR always gets five columns in landscape)")
	cmd.PersistentFlags().StringVar(&socket, "socket", "/tmp/cmux.sock", "cmux control socket")
	cmd.PersistentFlags().StringVar(&cli, "cmux", "/Applications/cmux.app/Contents/Resources/bin/cmux", "installed cmux CLI")
	for _, verb := range []struct{ name, description string }{
		{"plan", "Preview the monitor's grid without changing cmux"},
		{"new", "Create and focus a new workspace with the monitor's grid"},
	} {
		cmd.AddCommand(&cobra.Command{Use: verb.name, Short: verb.description, Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
			shape, err := cmuxgrid.ForMonitor(width, height, screen)
			if err != nil {
				return err
			}
			if command.Name() == "plan" {
				if !rt.json {
					_, err := fmt.Fprintf(rt.stdout, "%d columns × %d rows (%d terminals); no workspace created\n", shape.Columns, shape.Rows, shape.Columns*shape.Rows)
					return err
				}
				layout, err := cmuxgrid.Layout(shape)
				if err != nil {
					return err
				}
				return json.NewEncoder(rt.stdout).Encode(struct {
					Shape  cmuxgrid.Shape `json:"shape"`
					Layout cmuxgrid.Node  `json:"layout"`
				}{shape, layout})
			}
			ctx, cancel := context.WithTimeout(command.Context(), 40*time.Second)
			defer cancel()
			if err := cmuxgrid.CheckVersion(ctx, cli); err != nil {
				return err
			}
			result, err := (cmuxgrid.Client{Socket: socket}).Create(ctx, shape)
			if err != nil {
				return err
			}
			if rt.json {
				return json.NewEncoder(rt.stdout).Encode(result)
			}
			_, err = fmt.Fprintf(rt.stdout, "Opened %d×%d grid: %s\n", shape.Columns, shape.Rows, result.WorkspaceID)
			return err
		}})
	}
	return cmd
}
