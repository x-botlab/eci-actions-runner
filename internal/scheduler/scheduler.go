// Package scheduler converges the set of running ECI container groups towards
// what the GitHub queue demands. GitHub owns the queue and ECI owns the tags, so
// a restart loses nothing: the next reconcile reads both sides again.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/x-botlab/eci-actions-runner/internal/config"
	"github.com/x-botlab/eci-actions-runner/internal/eci"
	"github.com/x-botlab/eci-actions-runner/internal/github"
	"github.com/x-botlab/eci-actions-runner/internal/metrics"
)

var errNotImplemented = errors.New("scheduler: reconcile not implemented")

// Deps are the collaborators the scheduler needs.
type Deps struct {
	Forge github.Forge
	// Clouds is keyed by ECI target name, so a binding can land in any region.
	Clouds   map[string]eci.Backend
	Bindings []config.Binding
	Interval time.Duration
	Logger   *slog.Logger
	Metrics  *metrics.Metrics
}

// Scheduler runs the reconcile loop.
type Scheduler struct {
	deps Deps
	kick chan struct{}
}

// New builds a scheduler.
func New(deps Deps) *Scheduler {
	return &Scheduler{deps: deps, kick: make(chan struct{}, 1)}
}

// Kick requests a reconcile soon, for example when a webhook arrives. The
// channel is buffered so a burst of events collapses into a single pass.
func (s *Scheduler) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run reconciles on a timer until ctx is cancelled. Passes are serial on
// purpose: concurrent reconcilers would race on the same capacity budget.
func (s *Scheduler) Run(ctx context.Context) error {
	ticker := time.NewTicker(s.deps.Interval)
	defer ticker.Stop()

	s.deps.Logger.Info("scheduler started",
		"interval", s.deps.Interval.String(),
		"bindings", len(s.deps.Bindings),
		"targets", len(s.deps.Clouds),
	)

	for {
		select {
		case <-ctx.Done():
			s.deps.Logger.Info("scheduler stopping")
			return nil
		case <-s.kick:
		case <-ticker.C:
		}

		if err := s.Reconcile(ctx); err != nil {
			s.deps.Logger.Error("reconcile failed", "err", err)
		}
	}
}

// Reconcile performs one convergence pass.
func (s *Scheduler) Reconcile(ctx context.Context) error {
	return errNotImplemented
}

// launch walks a binding's targets in preference order and returns the instance
// id together with the target that accepted it. A stock out or a throttle in one
// zone moves on to the next target; anything else stops the walk, because a bad
// credential or a missing image fails in every region.
func (s *Scheduler) launch(ctx context.Context, binding config.Binding, spec eci.Spec) (instanceID, target string, err error) {
	if spec.Pool == "" {
		spec.Pool = binding.Pool.Name
	}
	if spec.Repo == "" {
		spec.Repo = binding.Repo.String()
	}

	var lastErr error
	for _, candidate := range binding.Targets {
		backend, ok := s.deps.Clouds[candidate.Name]
		if !ok {
			lastErr = fmt.Errorf("eci target %q has no backend", candidate.Name)
			continue
		}

		spec.Image = binding.Pool.ImageFor(candidate.Name)
		id, launchErr := backend.Launch(ctx, spec)
		if launchErr == nil {
			return id, candidate.Name, nil
		}

		s.deps.Metrics.CreateFailures.
			WithLabelValues(spec.Repo, spec.Pool, eci.FailureReason(launchErr)).Inc()
		if !eci.IsCapacityError(launchErr) {
			return "", "", fmt.Errorf("target %s: %w", candidate.Name, launchErr)
		}

		s.deps.Logger.Warn("no capacity in target, trying the next one",
			"repo", spec.Repo, "pool", spec.Pool, "target", candidate.Name, "err", launchErr)
		lastErr = launchErr
	}

	if lastErr == nil {
		lastErr = errors.New("pool lists no eci targets")
	}
	return "", "", fmt.Errorf("pool %s: no target accepted the instance: %w", spec.Pool, lastErr)
}
