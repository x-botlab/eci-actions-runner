// Package scheduler converges the set of running ECI container groups towards
// what the GitHub queue demands. GitHub owns the queue and ECI owns the tags, so
// a restart loses nothing: the next reconcile reads both sides again.
package scheduler

import (
	"context"
	"errors"
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
