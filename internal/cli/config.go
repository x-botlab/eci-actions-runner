package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/x-botlab/eci-actions-runner/internal/config"
)

func newConfigCmd(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect and validate the configuration file",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "check",
		Short: "Validate the configuration file",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "ok: %s, %d pool(s), poll every %ds\n",
				*configPath, len(cfg.Pools), cfg.Runner.PollIntervalSeconds)
			return err
		},
	})
	return cmd
}

func newPoolCmd(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pool",
		Short: "Inspect runner pools",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "ls",
		Short: "List the declared pools",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "POOL\tVCPU\tMEM(GiB)\tMIN\tWARM\tMAX\tTTL(m)\tSPOT\tLABELS")
			for _, p := range cfg.Pools {
				fmt.Fprintf(w, "%s\t%g\t%g\t%d\t%d\t%d\t%d\t%t\t%s\n",
					p.Name, p.VCPU, p.MemoryGiB, p.Min, p.Warm, p.Max, p.TTLMinutes, p.Spot,
					strings.Join(p.Labels, ","))
			}
			return w.Flush()
		},
	})
	return cmd
}
