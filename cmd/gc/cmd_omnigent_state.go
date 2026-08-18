package main

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/spf13/cobra"
)

var (
	omnigentStateReadClientForCommand  = omnigentStateReadClient
	omnigentStateWriteClientForCommand = omnigentStateWriteClient
)

func newOmnigentStateCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:          "state",
		Short:        "Inspect and explicitly purge remote Omnigent capsule state",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
	}
	cmd.AddCommand(
		newOmnigentStateInspectCmd(stdout, stderr),
		newOmnigentStatePurgeCmd(stdout, stderr),
	)
	return cmd
}

func newOmnigentStateInspectCmd(stdout, _ io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:          "inspect",
		Short:        "Compare durable sessions with provider-owned capsule state",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			client, err := omnigentStateReadClientForCommand()
			if err != nil {
				return fmt.Errorf("gc omnigent state inspect: %w", err)
			}
			report, err := client.InspectOmnigentCapsuleState()
			if err != nil {
				return fmt.Errorf("gc omnigent state inspect: %w", err)
			}
			return renderOmnigentCapsuleStateReport(stdout, report, jsonOutput)
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output JSON")
	return cmd
}

func newOmnigentStatePurgeCmd(stdout, _ io.Writer) *cobra.Command {
	var dryRun, jsonOutput bool
	cmd := &cobra.Command{
		Use:   "purge <session-id>",
		Short: "Preview or explicitly delete one closed session's capsule state",
		Long: "Record durable purge authorization and delete the exact provider-owned allocation for one closed, non-live session. " +
			"Use --dry-run to perform every safety read without recording authorization or mutating provider state.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, args []string) error {
			client, err := omnigentStateWriteClientForCommand()
			if err != nil {
				return fmt.Errorf("gc omnigent state purge: %w", err)
			}
			report, err := client.PurgeOmnigentCapsuleState(args[0], dryRun)
			if err != nil {
				return fmt.Errorf("gc omnigent state purge: %w", err)
			}
			return renderOmnigentCapsuleStateReport(stdout, report, jsonOutput)
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Validate purge without recording authorization or deleting state")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Output JSON")
	return cmd
}

func omnigentStateReadClient() (*api.Client, error) {
	remote, isRemote, cityPath, err := resolveReadTarget()
	if err != nil {
		return nil, err
	}
	if isRemote {
		return remote, nil
	}
	client, reason := maintenanceAPIClient(cityPath)
	if client == nil {
		return nil, fmt.Errorf("controller unavailable (%s)", reason)
	}
	return client, nil
}

func omnigentStateWriteClient() (*api.Client, error) {
	remote, isRemote, _, err := resolveWriteTarget()
	if err != nil {
		return nil, err
	}
	if isRemote {
		return remote, nil
	}
	resolved, err := resolveContext()
	if err != nil {
		return nil, err
	}
	client, reason := maintenanceAPIClient(resolved.CityPath)
	if client == nil {
		return nil, fmt.Errorf("controller unavailable (%s)", reason)
	}
	return client, nil
}

func renderOmnigentCapsuleStateReport(w io.Writer, report api.OmnigentCapsuleStateReportBody, jsonOutput bool) error {
	if jsonOutput {
		return writeCLIJSONLine(w, report)
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "SESSION\tACTION\tREASON"); err != nil {
		return err
	}
	for _, item := range report.Items {
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\n", item.SessionID, item.Action, item.Reason); err != nil {
			return err
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if report.IgnoredForeign > 0 {
		_, err := fmt.Fprintf(w, "foreign allocations ignored: %d\n", report.IgnoredForeign)
		return err
	}
	return nil
}
