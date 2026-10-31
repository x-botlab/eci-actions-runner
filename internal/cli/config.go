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
			bindings := cfg.Bindings()
			repos := 0
			for _, inst := range cfg.GitHub.Installations {
				repos += len(inst.Repos)
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"ok: %s, %d repo(s) over %d installation(s), %d pool(s) over %d target(s), %d binding(s), poll every %ds\n",
				*configPath, repos, len(cfg.GitHub.Installations), len(cfg.Pools), len(cfg.ECI.Targets),
				len(bindings), cfg.Runner.PollIntervalSeconds)
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
			fmt.Fprintln(w, "POOL\tECI\tVCPU\tMEM(GiB)\tMIN\tWARM\tMAX\tTTL(m)\tSPOT\tPRIV\tLABELS")
			for _, p := range cfg.Pools {
				fmt.Fprintf(w, "%s\t%s\t%g\t%g\t%d\t%d\t%d\t%d\t%t\t%t\t%s\n",
					p.Name, strings.Join(p.ECI, ">"), p.VCPU, p.MemoryGiB, p.Min, p.Warm, p.Max, p.TTLMinutes, p.Spot,
					p.Privileged, strings.Join(p.Labels, ","))
			}
			return w.Flush()
		},
	})
	return cmd
}

func newRepoCmd(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "repo",
		Short: "Inspect the repositories that get runners",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "ls",
		Short: "List repositories with the pools and targets bound to them",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}

			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "REPO\tINSTALLATION\tBINDINGS")

			byRepo := make(map[string][]config.Binding)
			for _, b := range cfg.Bindings() {
				key := b.Repo.String()
				byRepo[key] = append(byRepo[key], b)
			}
			for _, inst := range cfg.GitHub.Installations {
				for _, repo := range inst.Repos {
					var bindings []string
					for _, b := range byRepo[repo.FullName()] {
						names := make([]string, 0, len(b.Targets))
						for _, t := range b.Targets {
							names = append(names, t.Name)
						}
						bindings = append(bindings, fmt.Sprintf("%s(%s)", b.Pool.Name, strings.Join(names, ">")))
					}
					fmt.Fprintf(w, "%s\t%d\t%s\n", repo.FullName(), inst.ID, strings.Join(bindings, ","))
				}
			}
			return w.Flush()
		},
	})
	return cmd
}
