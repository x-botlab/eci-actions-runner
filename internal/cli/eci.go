package cli

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/x-botlab/eci-actions-runner/internal/config"
	"github.com/x-botlab/eci-actions-runner/internal/eci"
	"github.com/x-botlab/eci-actions-runner/internal/github"
	"github.com/x-botlab/eci-actions-runner/internal/metrics"
	"github.com/x-botlab/eci-actions-runner/internal/scheduler"
)

func newECICmd(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "eci",
		Short: "Check the ECI side without GitHub jobs",
	}
	cmd.AddCommand(newECIProbeCmd(configPath))
	return cmd
}

// newECIProbeCmd runs the whole launch path once: mint a JIT config, create one
// container group through the same failover the scheduler uses, wait for it to
// reach Running, and delete it again.
//
// It is the cheapest way to answer the two questions a first deployment has: can
// ECI pull this image, and how long does the cold start take.
func newECIProbeCmd(configPath *string) *cobra.Command {
	var (
		poolName string
		timeout  time.Duration
		keep     bool
	)

	cmd := &cobra.Command{
		Use:   "probe",
		Short: "Create one container group, time it, delete it",
		Long: "Creates a single container group for a pool, waits until it reaches Running,\n" +
			"reports how long each phase took, then deletes it. The container group is\n" +
			"removed even when the probe fails, unless --keep is given.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, clouds, err := cloudClients(*configPath)
			if err != nil {
				return err
			}
			if err := validateClouds(clouds); err != nil {
				return err
			}

			binding, err := bindingForPool(cfg, poolName)
			if err != nil {
				return err
			}
			if len(binding.Targets) == 0 {
				return fmt.Errorf("pool %q binds no eci target", poolName)
			}

			forge, err := buildForge(cfg)
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()

			out := cmd.OutOrStdout()
			logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: slog.LevelInfo}))

			name := fmt.Sprintf("%sprobe-%s", scheduler.RunnerNamePrefix(binding.Pool.Name), randomSuffix())
			started := time.Now()

			jit, err := forge.CreateJITConfig(ctx, binding.Repo, name, binding.Pool.Labels)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "jit config      %s for %s (%s in %s)\n",
				jit.RunnerName, binding.Repo, jitEncodedSummary(jit.Encoded), time.Since(started).Round(time.Millisecond))

			spec := eci.Spec{
				Name:      name,
				Pool:      binding.Pool.Name,
				Repo:      binding.Repo.String(),
				VCPU:      binding.Pool.VCPU,
				MemoryGiB: binding.Pool.MemoryGiB,
				Env:       map[string]string{eci.EnvJITConfig: jit.Encoded},
				TTL:       time.Duration(binding.Pool.TTLMinutes) * time.Minute,
				Spot:      binding.Pool.Spot,
			}

			instanceID, target, err := scheduler.LaunchOne(ctx, clouds, binding, spec, logger, metrics.New())
			if err != nil {
				// The JIT registration exists either way; drop it so the repo does
				// not accumulate offline runners.
				cleanupRunner(ctx, forge, binding.Repo, jit, out)
				return err
			}
			launched := time.Since(started)
			fmt.Fprintf(out, "created         %s in target %s after %s\n",
				instanceID, target, launched.Round(time.Millisecond))

			if !keep {
				defer func() {
					cleanupRunner(ctx, forge, binding.Repo, jit, out)
					deleteInstance(ctx, clouds[target], instanceID, out)
				}()
			}

			ready, err := waitForRunning(ctx, clouds[target], binding, name, out)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "\ncold start      %s (create %s, image pull and boot %s)\n",
				time.Since(started).Round(time.Second), launched.Round(time.Second),
				time.Since(started).Round(time.Second)-launched.Round(time.Second))
			fmt.Fprintf(out, "final status    %s\n", ready.Status)
			if ready.CreatedAt.IsZero() {
				fmt.Fprintf(out, "note            the platform did not report a creation time\n")
			}

			// Reaching Running only proves the container booted. The runner coming
			// online also proves egress and the JIT config, which is what a job
			// actually needs.
			if err := waitForRunnerOnline(ctx, forge, binding.Repo, name, out); err != nil {
				return err
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&poolName, "pool", "", "pool to probe (required)")
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "give up after this long")
	cmd.Flags().BoolVar(&keep, "keep", false, "leave the container group and registration in place")
	_ = cmd.MarkFlagRequired("pool")
	return cmd
}

// bindingForPool picks the first binding of a pool.
func bindingForPool(cfg *config.Config, name string) (config.Binding, error) {
	for _, binding := range cfg.Bindings() {
		if binding.Pool.Name == name {
			return binding, nil
		}
	}
	return config.Binding{}, fmt.Errorf("no repo binds pool %q", name)
}

// waitForRunning polls until the instance reports Running, or the context ends.
func waitForRunning(ctx context.Context, backend eci.Backend, binding config.Binding, name string, out io.Writer) (eci.Instance, error) {
	selector := eci.Selector{Pool: binding.Pool.Name, Repo: binding.Repo.String()}
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	lastStatus := ""
	internetIP := ""
	for {
		instances, err := backend.Describe(ctx, selector)
		if err != nil {
			return eci.Instance{}, err
		}
		for _, instance := range instances {
			if instance.Name != name {
				continue
			}
			if instance.Status != lastStatus {
				fmt.Fprintf(out, "status          %s\n", instance.Status)
				lastStatus = instance.Status
			}
			if instance.InternetIP != "" && internetIP == "" {
				internetIP = instance.InternetIP
				fmt.Fprintf(out, "public ip       %s\n", internetIP)
			}
			if instance.Status == "Running" {
				if internetIP == "" {
					fmt.Fprintf(out, "public ip       none (target has no auto_create_eip)\n")
				}
				return instance, nil
			}
			if instance.Terminal() {
				for _, event := range instance.Events {
					fmt.Fprintf(out, "event           %s: %s\n", event.Reason, event.Message)
				}
				return instance, fmt.Errorf("container group reached %s before Running", instance.Status)
			}
		}

		select {
		case <-ctx.Done():
			if lastStatus == "" {
				return eci.Instance{}, errors.New("the container group never appeared in ECI")
			}
			return eci.Instance{}, fmt.Errorf("still %s when the probe timed out", lastStatus)
		case <-ticker.C:
		}
	}
}

// waitForRunnerOnline waits until GitHub reports the probe runner as online. A
// container that boots but never registers means the container group has no
// egress, which no amount of ECI-side inspection shows.
func waitForRunnerOnline(ctx context.Context, forge *github.AppClient, repo github.Repo, name string, out io.Writer) error {
	const poll = 5 * time.Second

	deadline := time.Now().Add(2 * time.Minute)
	lastStatus := ""
	for {
		runners, err := forge.Runners(ctx, repo, name)
		if err != nil {
			return err
		}
		for _, runner := range runners {
			if runner.Name != name {
				continue
			}
			if runner.Status != lastStatus {
				fmt.Fprintf(out, "runner %-9s %s\n", runner.Status, runner.Name)
				lastStatus = runner.Status
			}
			if runner.Status == "online" {
				return nil
			}
		}

		if time.Now().After(deadline) {
			if lastStatus == "" {
				return errors.New("the runner never registered: the container group likely has no egress to github.com")
			}
			return fmt.Errorf("the runner stayed %s: it cannot reach github.com", lastStatus)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

func deleteInstance(ctx context.Context, backend eci.Backend, id string, out io.Writer) {
	if err := backend.Terminate(ctx, id); err != nil {
		fmt.Fprintf(out, "delete          FAILED for %s: %v\n", id, err)
		return
	}
	fmt.Fprintf(out, "deleted         %s\n", id)
}

func cleanupRunner(ctx context.Context, forge *github.AppClient, repo github.Repo, jit github.JITConfig, out io.Writer) {
	if jit.RunnerID == 0 {
		return
	}
	if err := forge.RemoveRunner(ctx, repo, jit.RunnerID); err != nil {
		fmt.Fprintf(out, "unregister      FAILED for runner %d: %v\n", jit.RunnerID, err)
		return
	}
	fmt.Fprintf(out, "unregistered    runner %d\n", jit.RunnerID)
}

// jitEncodedSummary describes the credential without revealing it.
func jitEncodedSummary(encoded string) string {
	sum := sha256.Sum256([]byte(encoded))
	return fmt.Sprintf("%d bytes, sha256:%x", len(encoded), sum[:4])
}
