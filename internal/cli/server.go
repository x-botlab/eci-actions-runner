package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/x-botlab/eci-actions-runner/internal/config"
	"github.com/x-botlab/eci-actions-runner/internal/metrics"
	"github.com/x-botlab/eci-actions-runner/internal/scheduler"
	"github.com/x-botlab/eci-actions-runner/internal/webhook"
)

func newServerCmd(configPath *string) *cobra.Command {
	var metricsAddr, webhookAddr, logLevel string

	cmd := &cobra.Command{
		Use:   "server",
		Short: "Run the reconcile loop",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(*configPath)
			if err != nil {
				return err
			}

			logger := slog.New(slog.NewJSONHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{
				Level: parseLevel(logLevel),
			}))

			forge, err := buildForge(cfg)
			if err != nil {
				return err
			}

			clouds, err := buildClouds(cfg)
			if err != nil {
				return err
			}
			// Fail now rather than on the first launch: a missing registry
			// credential is a deployment mistake, not a runtime condition.
			if err := validateClouds(clouds); err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			m := metrics.New()
			go serveHTTP(ctx, "metrics", metricsAddr, m.Handler(), logger)

			s := scheduler.New(scheduler.Deps{
				Forge:          forge,
				Clouds:         clouds,
				Bindings:       cfg.Bindings(),
				Interval:       time.Duration(cfg.Runner.PollIntervalSeconds) * time.Second,
				Logger:         logger,
				Metrics:        m,
				MaxLaunches:    cfg.Runner.MaxConcurrentLaunches,
				PendingTimeout: time.Duration(cfg.Runner.PendingTimeoutSeconds) * time.Second,
			})

			if webhookAddr != "" {
				handler, err := buildWebhook(cfg, s.Kick, logger)
				if err != nil {
					return err
				}
				go serveHTTP(ctx, "webhook", webhookAddr, handler, logger)
			}

			return s.Run(ctx)
		},
	}

	cmd.Flags().StringVar(&metricsAddr, "metrics-addr", ":9090", "address to serve /metrics on")
	cmd.Flags().StringVar(&webhookAddr, "webhook-addr", "", "address to serve GitHub webhook deliveries on; polling still runs")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level: debug, info, warn or error")
	return cmd
}

// buildWebhook prepares the delivery endpoint. The secret is read from the
// environment, and its absence is fatal: an unauthenticated endpoint would let
// anyone trigger reconciles.
func buildWebhook(cfg *config.Config, kick func(), logger *slog.Logger) (http.Handler, error) {
	envName := cfg.GitHub.Webhook.SecretEnv
	secret := strings.TrimSpace(os.Getenv(envName))
	if secret == "" {
		return nil, fmt.Errorf("webhook: %s is required to verify deliveries", envName)
	}

	seen := make(map[string]struct{})
	var repos []string
	for _, binding := range cfg.Bindings() {
		key := binding.Repo.String()
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		repos = append(repos, key)
	}

	handler, err := webhook.New([]byte(secret), repos, kick, logger)
	if err != nil {
		return nil, err
	}
	logger.Info("webhook endpoint enabled", "repos", len(repos), "secret_env", envName)
	return handler, nil
}

func serveHTTP(ctx context.Context, name, addr string, handler http.Handler, logger *slog.Logger) {
	if addr == "" {
		return
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logger.Info("listening", "service", name, "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("server stopped", "service", name, "err", err)
	}
}

func parseLevel(name string) slog.Level {
	var level slog.Level
	if err := level.UnmarshalText([]byte(name)); err != nil {
		return slog.LevelInfo
	}
	return level
}
