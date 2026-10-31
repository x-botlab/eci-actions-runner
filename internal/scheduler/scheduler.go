// Package scheduler converges the set of running ECI container groups towards
// what the GitHub queue demands. GitHub owns the queue and ECI owns the tags, so
// a restart loses nothing: the next reconcile reads both sides again.
package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/x-botlab/eci-actions-runner/internal/config"
	"github.com/x-botlab/eci-actions-runner/internal/eci"
	"github.com/x-botlab/eci-actions-runner/internal/github"
	"github.com/x-botlab/eci-actions-runner/internal/metrics"
	"github.com/x-botlab/eci-actions-runner/internal/pool"
)

// Defaults applied when Deps leaves a knob at zero.
const (
	defaultMaxLaunches    = 3
	defaultPendingTimeout = 3 * time.Minute
)

// Deps are the collaborators the scheduler needs.
type Deps struct {
	Forge github.Forge
	// Clouds is keyed by ECI target name, so a binding can land in any region.
	Clouds   map[string]eci.Backend
	Bindings []config.Binding
	Interval time.Duration
	Logger   *slog.Logger
	Metrics  *metrics.Metrics
	// MaxLaunches caps how many container groups one pass creates, so a burst of
	// queued jobs cannot spend the whole ECI quota in a single tick.
	MaxLaunches int
	// PendingTimeout is how long a container group may sit in Pending before it
	// is treated as a failed start and deleted.
	PendingTimeout time.Duration
}

// Scheduler runs the reconcile loop.
type Scheduler struct {
	deps Deps
	kick chan struct{}

	// ready flips once a pass has completed cleanly, which is what a readiness
	// probe should mean: the loop has talked to both GitHub and ECI at least once.
	ready atomic.Bool

	// inFlight remembers launches that the platform does not list yet. Tag based
	// discovery is eventually consistent, so without this two passes a second
	// apart would both decide to create a runner for the same job.
	mu       sync.Mutex
	inFlight map[string]time.Time
}

// inFlightTTL bounds how long a remembered launch counts as pending. Past it, the
// platform either lists the instance or the launch never happened.
const inFlightTTL = 3 * time.Minute

// New builds a scheduler.
func New(deps Deps) *Scheduler {
	if deps.MaxLaunches <= 0 {
		deps.MaxLaunches = defaultMaxLaunches
	}
	if deps.PendingTimeout <= 0 {
		deps.PendingTimeout = defaultPendingTimeout
	}
	return &Scheduler{
		deps:     deps,
		kick:     make(chan struct{}, 1),
		inFlight: make(map[string]time.Time),
	}
}

func launchKey(repo, pool, name string) string { return repo + "|" + pool + "|" + name }

// rememberLaunch records a container group this pass created.
func (s *Scheduler) rememberLaunch(repo, pool, name string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inFlight[launchKey(repo, pool, name)] = now
}

// pendingLaunches returns how many launches for this binding the platform has not
// shown yet, and forgets the ones that are now visible or too old to trust.
func (s *Scheduler) pendingLaunches(repo, pool string, observed []eci.Instance, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	visible := make(map[string]struct{}, len(observed))
	for _, instance := range observed {
		visible[instance.Name] = struct{}{}
	}

	prefix := repo + "|" + pool + "|"
	pending := 0
	for key, at := range s.inFlight {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		if _, ok := visible[strings.TrimPrefix(key, prefix)]; ok {
			delete(s.inFlight, key)
			continue
		}
		if now.Sub(at) > inFlightTTL {
			delete(s.inFlight, key)
			continue
		}
		pending++
	}
	return pending
}

// Kick requests a reconcile soon, for example when a webhook arrives. The
// channel is buffered so a burst of events collapses into a single pass.
func (s *Scheduler) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// followUpDelays are the extra passes a webhook schedules. The first covers a
// delivery that arrives before GitHub lists the job; the second covers a run whose
// jobs appear a little later.
var followUpDelays = []time.Duration{1500 * time.Millisecond, 5 * time.Second}

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

	var (
		pending []time.Duration
		settle  <-chan time.Time
	)
	for {
		select {
		case <-ctx.Done():
			s.deps.Logger.Info("scheduler stopping")
			return nil
		case <-s.kick:
			s.reconcileOnce(ctx)
			pending = followUpDelays
			settle = time.After(pending[0])
			pending = pending[1:]
		case <-settle:
			s.reconcileOnce(ctx)
			if len(pending) > 0 {
				settle = time.After(pending[0])
				pending = pending[1:]
			} else {
				// A nil channel blocks forever, which is what a disabled
				// follow-up should do.
				settle = nil
			}
		case <-ticker.C:
			s.reconcileOnce(ctx)
		}
	}
}

func (s *Scheduler) reconcileOnce(ctx context.Context) {
	if err := s.Reconcile(ctx); err != nil {
		s.deps.Logger.Error("reconcile failed", "err", err)
		return
	}
	s.ready.Store(true)
}

// Ready reports whether a reconcile pass has ever completed. A readiness probe
// uses it so traffic (and a rollout) waits until both APIs have answered once.
func (s *Scheduler) Ready() bool { return s.ready.Load() }

// Reconcile performs one convergence pass: it reads the queue and the observed
// container groups, deletes what can never run again, and creates the difference.
func (s *Scheduler) Reconcile(ctx context.Context) error {
	pass := &reconcilePass{}

	var errs []error
	for _, group := range groupByRepo(s.deps.Bindings) {
		if err := s.reconcileRepo(ctx, group, pass); err != nil {
			s.deps.Logger.Error("reconcile failed", "repo", group.repo.String(), "err", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// repoGroup is one repository with the pools bound to it.
type repoGroup struct {
	repo     github.Repo
	bindings []config.Binding
}

// groupByRepo keeps bindings in configuration order while making sure a repo's
// queue is fetched once per pass instead of once per pool.
func groupByRepo(bindings []config.Binding) []repoGroup {
	index := make(map[string]int, len(bindings))
	var groups []repoGroup
	for _, binding := range bindings {
		key := binding.Repo.String()
		at, ok := index[key]
		if !ok {
			index[key] = len(groups)
			groups = append(groups, repoGroup{repo: binding.Repo})
			at = len(groups) - 1
		}
		groups[at].bindings = append(groups[at].bindings, binding)
	}
	return groups
}

// reconcilePass carries state across the bindings of one pass.
type reconcilePass struct {
	launched int
}

func (s *Scheduler) reconcileRepo(ctx context.Context, group repoGroup, pass *reconcilePass) error {
	queued, err := s.deps.Forge.QueuedJobs(ctx, group.repo)
	if err != nil {
		return fmt.Errorf("queued jobs for %s: %w", group.repo, err)
	}

	var errs []error
	for _, binding := range group.bindings {
		if err := s.reconcileBinding(ctx, binding, queued, pass); err != nil {
			// One broken pool must not stop the others in the same repo.
			s.deps.Logger.Error("binding failed",
				"repo", binding.Repo.String(), "pool", binding.Pool.Name, "err", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Scheduler) reconcileBinding(ctx context.Context, binding config.Binding, queued []github.Job, pass *reconcilePass) error {
	repo := binding.Repo.String()
	spec := binding.Pool

	waiting, oldest := countMatching(spec, queued)
	s.deps.Metrics.QueuedJobs.WithLabelValues(repo, spec.Name).Set(float64(waiting))

	observed, err := s.observe(ctx, binding)
	if err != nil {
		// Never launch on partial information: a failed describe could double the
		// pool, and a doubled pool costs money silently.
		return err
	}
	s.observeMetrics(repo, spec.Name, observed)

	// Clean up first, and only while the observed state is trustworthy. The
	// runner registry is needed for both the cleanup and the capacity below, so it
	// is fetched once: two snapshots could disagree.
	runners, err := s.deps.Forge.Runners(ctx, binding.Repo, RunnerNamePrefix(binding.Pool.Name))
	if err != nil {
		// Same reasoning as the describe above: without knowing which runners are
		// idle, a launch decision is a guess, and a wrong guess costs money.
		return fmt.Errorf("runners for %s: %w", repo, err)
	}
	if err := s.terminateDead(ctx, binding, observed); err != nil {
		return err
	}
	if err := s.pruneRunners(ctx, binding, observed, runners); err != nil {
		return err
	}

	have := capacityOf(observed, runners, s.pendingLaunches(repo, spec.Name, observed, time.Now()))
	s.deps.Logger.Debug("observed",
		"repo", repo, "pool", spec.Name,
		"waiting", waiting, "idle", have.Idle, "starting", have.Starting,
		"occupied", have.Occupied, "exiting", have.Exiting, "plan", spec.Plan(waiting, have))

	for launch := spec.Plan(waiting, have); launch > 0; launch-- {
		if pass.launched >= s.deps.MaxLaunches {
			s.deps.Logger.Warn("launch budget exhausted for this pass",
				"repo", repo, "pool", spec.Name, "budget", s.deps.MaxLaunches)
			return nil
		}
		if err := s.launchOne(ctx, binding, oldest); err != nil {
			return err
		}
		pass.launched++
	}
	return nil
}

// countMatching returns how many queued jobs this pool can serve and when the
// oldest of them started waiting.
func countMatching(spec pool.Spec, queued []github.Job) (int, time.Time) {
	waiting := 0
	var oldest time.Time
	for _, job := range queued {
		if !spec.Matches(job.Labels) {
			continue
		}
		waiting++
		if oldest.IsZero() || job.CreatedAt.Before(oldest) {
			oldest = job.CreatedAt
		}
	}
	return waiting, oldest
}

// observe reads the container groups across every target the pool may use.
func (s *Scheduler) observe(ctx context.Context, binding config.Binding) ([]eci.Instance, error) {
	selector := eci.Selector{Pool: binding.Pool.Name, Repo: binding.Repo.String()}

	var instances []eci.Instance
	for _, target := range binding.Targets {
		backend, ok := s.deps.Clouds[target.Name]
		if !ok {
			return nil, fmt.Errorf("eci target %q has no backend", target.Name)
		}
		found, err := backend.Describe(ctx, selector)
		if err != nil {
			return nil, fmt.Errorf("describe %s: %w", target.Name, err)
		}
		instances = append(instances, found...)
	}
	return instances, nil
}

func (s *Scheduler) observeMetrics(repo, poolName string, instances []eci.Instance) {
	counts := map[string]int{"active": 0, "terminal": 0, "other": 0}
	for _, instance := range instances {
		switch {
		case instance.Active():
			counts["active"]++
		case instance.Terminal():
			counts["terminal"]++
		default:
			counts["other"]++
		}
	}
	for state, count := range counts {
		s.deps.Metrics.Instances.WithLabelValues(repo, poolName, state).Set(float64(count))
	}
}

// DeadReason reports why a container group may be deleted, or "" when it must be
// left alone. Only states that cannot run another job qualify: a healthy long job
// is never killed for being old, because the TTL is enforced by the platform
// through ActiveDeadlineSeconds and not by us.
func DeadReason(instance eci.Instance, ttl, pendingTimeout time.Duration, now time.Time) string {
	age := now.Sub(instance.CreatedAt)
	switch {
	case instance.Terminal():
		return "terminal"
	case instance.Status == "Pending" && !instance.CreatedAt.IsZero() && age > pendingTimeout:
		return "stuck-pending"
	case !instance.Active() && !instance.CreatedAt.IsZero() && age > ttl:
		return "expired"
	}
	return ""
}

// terminateDead deletes container groups that can never take another job.
func (s *Scheduler) terminateDead(ctx context.Context, binding config.Binding, instances []eci.Instance) error {
	ttl := time.Duration(binding.Pool.TTLMinutes) * time.Minute
	now := time.Now()

	for _, instance := range instances {
		reason := DeadReason(instance, ttl, s.deps.PendingTimeout, now)
		if reason == "" {
			continue
		}

		backend, ok := s.deps.Clouds[instance.Target]
		if !ok {
			return fmt.Errorf("eci target %q has no backend", instance.Target)
		}
		if err := backend.Terminate(ctx, instance.ID); err != nil {
			return fmt.Errorf("terminate %s: %w", instance.Name, err)
		}
		s.deps.Logger.Info("deleted container group",
			"repo", binding.Repo.String(), "pool", binding.Pool.Name,
			"instance", instance.Name, "target", instance.Target,
			"status", instance.Status, "reason", reason)
	}
	return nil
}

// capacityOf classifies what the pool has. The container groups and the runner
// registry are joined by name, which is the same value by construction: one
// launch names both its JIT runner and its container group (see runnerName).
//
// The two sides do not share a lifetime. A runner is registered before its
// container exists and unregisters itself before its container is deleted, so a
// registry entry and a group can each exist alone:
//
//   - a registered runner with no group is a launch the platform has not listed
//     yet (already counted by pending) or a leak that pruneRunners just removed;
//   - a group with no registered runner cannot take a job any more.
func capacityOf(observed []eci.Instance, runners []github.Runner, pending int) pool.Capacity {
	byName := make(map[string]github.Runner, len(runners))
	for _, runner := range runners {
		byName[runner.Name] = runner
	}

	// A launch the platform has not listed is on its way, so it counts as
	// starting rather than as nothing.
	have := pool.Capacity{Starting: pending}
	for _, instance := range observed {
		if !instance.Active() {
			continue
		}
		if instance.Status != "Running" {
			// Pending, Scheduling, Restarting: the container is not up yet.
			have.Starting++
			continue
		}
		runner, registered := byName[instance.Name]
		switch {
		case !registered:
			have.Exiting++
		case runner.Busy:
			have.Occupied++
		case runner.Status == "online":
			have.Idle++
		default:
			// Registered but not connected yet: the container is running, the
			// listener is still starting up, and GitHub will not assign a job.
			have.Starting++
		}
	}
	return have
}

// pruneRunners removes runners that registered from a JIT config but never came
// online, which is what a container group that failed to start leaves behind.
// It is skipped while anything is still starting, because a starting runner is
// briefly offline and must not be unregistered.
func (s *Scheduler) pruneRunners(ctx context.Context, binding config.Binding, instances []eci.Instance, runners []github.Runner) error {
	for _, instance := range instances {
		if instance.Status == "Pending" || instance.Status == "Scheduling" {
			return nil
		}
	}

	live := make(map[string]struct{}, len(instances))
	for _, instance := range instances {
		if instance.Name != "" {
			live[instance.Name] = struct{}{}
		}
	}

	for _, runner := range runners {
		if runner.Status != "offline" || runner.Busy {
			continue
		}
		if _, ok := live[runner.Name]; ok {
			continue
		}
		if err := s.deps.Forge.RemoveRunner(ctx, binding.Repo, runner.ID); err != nil {
			return err
		}
		s.deps.Logger.Info("unregistered leaked runner",
			"repo", binding.Repo.String(), "pool", binding.Pool.Name, "runner", runner.Name)
	}
	return nil
}

// SpecFor builds the launch spec for one runner of binding. The reconcile loop
// and `eci probe` both use it, so a pool property cannot reach one path and miss
// the other — which is exactly what happened to Privileged when the probe built
// its own spec.
func SpecFor(binding config.Binding, name, jitConfig string) eci.Spec {
	return eci.Spec{
		Name:      name,
		Pool:      binding.Pool.Name,
		Repo:      binding.Repo.String(),
		VCPU:      binding.Pool.VCPU,
		MemoryGiB: binding.Pool.MemoryGiB,
		Env:       map[string]string{eci.EnvJITConfig: jitConfig},
		TTL:       time.Duration(binding.Pool.TTLMinutes) * time.Minute,
		Spot:      binding.Pool.Spot,
		// Privileged is a property of the pool, not of the target: the daemon it
		// starts lives in the same container group in every region.
		Privileged: binding.Pool.Privileged,
	}
}

// launchOne mints a JIT config and hands it to a new container group. The runner
// stays unbound to a specific job: it registers as ephemeral and takes whichever
// matching job arrives first, which is also what makes a warm instance possible.
func (s *Scheduler) launchOne(ctx context.Context, binding config.Binding, oldest time.Time) error {
	name := runnerName(binding.Pool.Name)

	jit, err := s.deps.Forge.CreateJITConfig(ctx, binding.Repo, name, binding.Pool.Labels)
	if err != nil {
		return err
	}

	spec := SpecFor(binding, name, jit.Encoded)

	instanceID, target, err := s.launch(ctx, binding, spec)
	if err != nil {
		return err
	}
	s.rememberLaunch(spec.Repo, spec.Pool, name, time.Now())
	if !oldest.IsZero() {
		s.deps.Metrics.QueueWait.WithLabelValues(spec.Repo, spec.Pool).Observe(time.Since(oldest).Seconds())
	}
	s.deps.Logger.Info("launched runner",
		"repo", spec.Repo, "pool", spec.Pool, "instance", instanceID, "target", target, "name", name)
	return nil
}

// RunnerNamePrefix is the name prefix that identifies this pool's runners. ECI
// container group names and runner names share the same value, so cleanup can
// match one against the other.
func RunnerNamePrefix(poolName string) string {
	cleaned := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, poolName)
	if cleaned = strings.Trim(cleaned, "-"); cleaned == "" {
		cleaned = "pool"
	}
	return "eci-" + cleaned + "-"
}

func runnerName(poolName string) string {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		// Names only have to be unique, not unpredictable.
		return RunnerNamePrefix(poolName) + strconv.FormatInt(time.Now().UnixNano()%1_000_000, 36)
	}
	return RunnerNamePrefix(poolName) + hex.EncodeToString(buf)
}

// LaunchOne walks a binding's targets in preference order and returns the
// instance id together with the target that accepted it. A stock out or a
// throttle in one zone moves on to the next target; anything else stops the walk,
// because a bad credential or a missing image fails in every region.
//
// It is exported so a smoke test can reuse the exact production path.
func LaunchOne(ctx context.Context, clouds map[string]eci.Backend, binding config.Binding, spec eci.Spec, logger *slog.Logger, m *metrics.Metrics) (instanceID, target string, err error) {
	if spec.Pool == "" {
		spec.Pool = binding.Pool.Name
	}
	if spec.Repo == "" {
		spec.Repo = binding.Repo.String()
	}

	var lastErr error
	for _, candidate := range binding.Targets {
		backend, ok := clouds[candidate.Name]
		if !ok {
			lastErr = fmt.Errorf("eci target %q has no backend", candidate.Name)
			continue
		}

		spec.Image = binding.Pool.ImageFor(candidate.Name)
		id, launchErr := backend.Launch(ctx, spec)
		if launchErr == nil {
			return id, candidate.Name, nil
		}

		if m != nil {
			m.CreateFailures.WithLabelValues(spec.Repo, spec.Pool, eci.FailureReason(launchErr)).Inc()
		}
		if !eci.IsCapacityError(launchErr) {
			return "", "", fmt.Errorf("target %s: %w", candidate.Name, launchErr)
		}

		logger.Warn("no capacity in target, trying the next one",
			"repo", spec.Repo, "pool", spec.Pool, "target", candidate.Name, "err", launchErr)
		lastErr = launchErr
	}

	if lastErr == nil {
		lastErr = errors.New("pool lists no eci targets")
	}
	return "", "", fmt.Errorf("pool %s: no target accepted the instance: %w", spec.Pool, lastErr)
}

// launch is the scheduler's view of LaunchOne.
func (s *Scheduler) launch(ctx context.Context, binding config.Binding, spec eci.Spec) (string, string, error) {
	return LaunchOne(ctx, s.deps.Clouds, binding, spec, s.deps.Logger, s.deps.Metrics)
}
