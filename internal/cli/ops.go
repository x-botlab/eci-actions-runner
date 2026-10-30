package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/x-botlab/eci-actions-runner/internal/config"
	"github.com/x-botlab/eci-actions-runner/internal/eci"
	"github.com/x-botlab/eci-actions-runner/internal/scheduler"
)

var errNotYetImplemented = errors.New("not implemented yet")

func newTable(cmd *cobra.Command) *tabwriter.Writer {
	return tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
}

// observed is one container group together with the binding it belongs to.
type observed struct {
	binding  config.Binding
	instance eci.Instance
}

// observeBindings lists the container groups of every binding.
func observeBindings(ctx context.Context, cfg *config.Config, clouds map[string]eci.Backend) ([]observed, error) {
	var out []observed
	for _, binding := range cfg.Bindings() {
		selector := eci.Selector{Pool: binding.Pool.Name, Repo: binding.Repo.String()}
		for _, target := range binding.Targets {
			backend, ok := clouds[target.Name]
			if !ok {
				return nil, fmt.Errorf("eci target %q has no backend", target.Name)
			}
			instances, err := backend.Describe(ctx, selector)
			if err != nil {
				return nil, err
			}
			for _, instance := range instances {
				out = append(out, observed{binding: binding, instance: instance})
			}
		}
	}
	return out, nil
}

func newPSCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "ps",
		Short: "Show container groups with their job and runtime",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, clouds, err := cloudClients(*configPath)
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			found, err := observeBindings(ctx, cfg, clouds)
			if err != nil {
				return err
			}

			now := time.Now()
			w := newTable(cmd)
			fmt.Fprintln(w, "REPO\tPOOL\tTARGET\tINSTANCE\tSTATUS\tAGE\tJOB")
			for _, entry := range found {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
					entry.binding.Repo, entry.binding.Pool.Name, entry.instance.Target,
					entry.instance.Name, entry.instance.Status,
					now.Sub(entry.instance.CreatedAt).Truncate(time.Second), entry.instance.JobID)
			}
			return w.Flush()
		},
	}
}

func newGCCmd(configPath *string) *cobra.Command {
	var apply bool

	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Delete container groups that outlived their job",
		Long: "Lists the container groups that can never run another job. Without --apply it\n" +
			"only reports them. A running job is never touched, however old it looks.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, clouds, err := cloudClients(*configPath)
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			found, err := observeBindings(ctx, cfg, clouds)
			if err != nil {
				return err
			}

			now := time.Now()
			pendingTimeout := time.Duration(cfg.Runner.PendingTimeoutSeconds) * time.Second
			w := newTable(cmd)
			fmt.Fprintln(w, "REPO\tPOOL\tTARGET\tINSTANCE\tSTATUS\tREASON")

			deleted := 0
			for _, entry := range found {
				ttl := time.Duration(entry.binding.Pool.TTLMinutes) * time.Minute
				reason := scheduler.DeadReason(entry.instance, ttl, pendingTimeout, now)
				if reason == "" {
					continue
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					entry.binding.Repo, entry.binding.Pool.Name, entry.instance.Target,
					entry.instance.Name, entry.instance.Status, reason)

				if !apply {
					continue
				}
				if err := clouds[entry.instance.Target].Terminate(ctx, entry.instance.ID); err != nil {
					return err
				}
				deleted++
			}
			if err := w.Flush(); err != nil {
				return err
			}

			if apply {
				fmt.Fprintf(cmd.OutOrStdout(), "deleted %d container group(s)\n", deleted)
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "dry run: nothing deleted (pass --apply to delete)")
			}
			return nil
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
