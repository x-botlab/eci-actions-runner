// Package cli wires the subcommands. There is deliberately no web UI: pool
// configuration lives in Git and day-two operations are verbs.
package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

// Version is overridden at build time via -ldflags.
var Version = "dev"

// Execute runs the root command.
func Execute() error {
	return newRootCmd().Execute()
}

func newRootCmd() *cobra.Command {
	var configPath string

	root := &cobra.Command{
		Use:   "eci-runner",
		Short: "Schedule GitHub Actions runners on Alibaba Cloud ECI",
		Long: "eci-runner creates one ephemeral ECI container group per GitHub Actions job,\n" +
			"registers it with a one-shot JIT config, and deletes it when the job ends.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&configPath, "config", "pools.yaml", "path to the configuration file")
	root.AddCommand(
		newServerCmd(&configPath),
		newConfigCmd(&configPath),
		newGitHubCmd(&configPath),
		newPoolCmd(&configPath),
		newRepoCmd(&configPath),
		newPSCmd(&configPath),
		newGCCmd(&configPath),
		newLogsCmd(&configPath),
		newDrainCmd(&configPath),
		newVersionCmd(),
	)
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the build version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), Version)
			return err
		},
	}
}
