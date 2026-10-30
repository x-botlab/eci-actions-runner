package cli

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
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

func newServerCmd(configPath *string) *cobra.Command {
	var metricsAddr, logLevel string

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

			forge, err := github.NewAppClient(github.Config{
				AppID:          cfg.GitHub.AppID,
				PrivateKeyPath: cfg.GitHub.PrivateKeyPath,
			})
			if err != nil {
				return err
			}

			clouds := make(map[string]eci.Backend, len(cfg.ECI.Targets))
			for _, target := range cfg.ECI.Targets {
				client, err := eci.New(eci.Config{
					Name:            target.Name,
					RegionID:        target.RegionID,
					Endpoint:        target.Endpoint,
					VSwitchIDs:      target.VSwitchIDs,
					SecurityGroupID: target.SecurityGroupID,
					RAMRoleName:     target.RAMRoleName,
				})
				if err != nil {
					return err
				}
				clouds[target.Name] = client
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			m := metrics.New()
			go serveMetrics(ctx, metricsAddr, m.Handler(), logger)

			s := scheduler.New(scheduler.Deps{
				Forge:    forge,
				Clouds:   clouds,
				Bindings: cfg.Bindings(),
				Interval: time.Duration(cfg.Runner.PollIntervalSeconds) * time.Second,
				Logger:   logger,
				Metrics:  m,
			})
			return s.Run(ctx)
		},
	}

	cmd.Flags().StringVar(&metricsAddr, "metrics-addr", ":9090", "address to serve /metrics on")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level: debug, info, warn or error")
	return cmd
}

func serveMetrics(ctx context.Context, addr string, handler http.Handler, logger *slog.Logger) {
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

	logger.Info("metrics listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("metrics server stopped", "err", err)
	}
}

func parseLevel(name string) slog.Level {
	var level slog.Level
	if err := level.UnmarshalText([]byte(name)); err != nil {
		return slog.LevelInfo
	}
	return level
}
