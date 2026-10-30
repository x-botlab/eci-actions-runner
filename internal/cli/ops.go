package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/x-botlab/eci-actions-runner/internal/config"
)

var errNotYetImplemented = errors.New("not implemented yet")

func newPSCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "ps",
		Short: "Show container groups with their job and runtime",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := config.Load(*configPath); err != nil {
				return err
			}
			return errNotYetImplemented
		},
	}
}

func newGCCmd(configPath *string) *cobra.Command {
	var apply bool

	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Delete container groups that outlived their job",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := config.Load(*configPath); err != nil {
				return err
			}
			if !apply {
				fmt.Fprintln(cmd.OutOrStdout(), "dry run: nothing will be deleted (pass --apply to delete)")
			}
			return errNotYetImplemented
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "actually delete the instances listed by the dry run")
	return cmd
}

func newLogsCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "logs <job-id>",
		Short: "Print the scheduler trace for one job",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if _, err := config.Load(*configPath); err != nil {
				return err
			}
			return fmt.Errorf("logs for job %s: %w", args[0], errNotYetImplemented)
		},
	}
}

func newDrainCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "drain <pool>",
		Short: "Stop expanding a pool without touching running jobs",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}
			if _, ok := cfg.Pool(args[0]); !ok {
				return fmt.Errorf("unknown pool %q", args[0])
			}
			return errNotYetImplemented
		},
	}
}
