package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alibabacloud-go/tea/tea"

	"github.com/x-botlab/eci-actions-runner/internal/config"
	"github.com/x-botlab/eci-actions-runner/internal/eci"
	"github.com/x-botlab/eci-actions-runner/internal/github"
	"github.com/x-botlab/eci-actions-runner/internal/metrics"
	"github.com/x-botlab/eci-actions-runner/internal/pool"
)

type fakeBackend struct {
	err      error
	launches int
	images   []string

	instances   []eci.Instance
	describeErr error
	terminated  []string
}

func (f *fakeBackend) Launch(_ context.Context, spec eci.Spec) (string, error) {
	f.launches++
	f.images = append(f.images, spec.Image)
	if f.err != nil {
		return "", f.err
	}
	return "eci-" + spec.Pool, nil
}

func (f *fakeBackend) Describe(_ context.Context, sel eci.Selector) ([]eci.Instance, error) {
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	var found []eci.Instance
	for _, instance := range f.instances {
		if sel.Pool != "" && instance.Pool != sel.Pool {
			continue
		}
		if sel.Repo != "" && instance.Repo != sel.Repo {
			continue
		}
		found = append(found, instance)
	}
	return found, nil
}

func (f *fakeBackend) Terminate(_ context.Context, id string) error {
	f.terminated = append(f.terminated, id)
	return nil
}

// fakeForge stands in for GitHub: the queue, the JIT configs and the registry.
type fakeForge struct {
	jobs    []github.Job
	runners []github.Runner
	jitErr  error

	jitNames  []string
	jitLabels [][]string
	removed   []int64
}

func (f *fakeForge) QueuedJobs(context.Context, github.Repo) ([]github.Job, error) {
	return f.jobs, nil
}

func (f *fakeForge) CreateJITConfig(_ context.Context, _ github.Repo, name string, labels []string) (github.JITConfig, error) {
	if f.jitErr != nil {
		return github.JITConfig{}, f.jitErr
	}
	f.jitNames = append(f.jitNames, name)
	f.jitLabels = append(f.jitLabels, labels)
	return github.JITConfig{Encoded: "jit-" + name, RunnerID: int64(len(f.jitNames)), RunnerName: name}, nil
}

func (f *fakeForge) Runners(context.Context, github.Repo, string) ([]github.Runner, error) {
	return f.runners, nil
}

func (f *fakeForge) RemoveRunner(_ context.Context, _ github.Repo, id int64) error {
	f.removed = append(f.removed, id)
	return nil
}

func sdkError(code string) error {
	return &tea.SDKError{Code: tea.String(code)}
}

// newTestScheduler builds a scheduler with the given backends and a single
// binding whose pool lists those targets in order.
func newTestScheduler(t *testing.T, backends map[string]*fakeBackend, targets []string) (*Scheduler, config.Binding) {
	t.Helper()

	clouds := make(map[string]eci.Backend, len(backends))
	for name, backend := range backends {
		clouds[name] = backend
	}

	spec := pool.Spec{
		Name:       "pool-short",
		ECI:        targets,
		Image:      "registry/default:v1",
		Labels:     []string{"eci-pool-short"},
		VCPU:       4,
		MemoryGiB:  8,
		Max:        4,
		TTLMinutes: 380,
	}
	binding := config.Binding{
		Repo: github.Repo{Owner: "o", Name: "r", InstallationID: 7},
		Pool: spec,
	}
	for _, name := range targets {
		binding.Targets = append(binding.Targets, config.Target{Name: name, RegionID: name})
	}

	s := New(Deps{
		Clouds:   clouds,
		Bindings: []config.Binding{binding},
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:  metrics.New(),
	})
	return s, binding
}

func TestLaunchFallsBackOnStockOut(t *testing.T) {
	first := &fakeBackend{err: sdkError("StockExhausted")}
	second := &fakeBackend{}
	s, binding := newTestScheduler(t, map[string]*fakeBackend{"a": first, "b": second}, []string{"a", "b"})

	id, target, err := s.launch(context.Background(), binding, eci.Spec{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}
	if target != "b" {
		t.Fatalf("landed in %q, want b", target)
	}
	if id != "eci-pool-short" {
		t.Fatalf("instance id %q, want eci-pool-short", id)
	}
	if first.launches != 1 || second.launches != 1 {
		t.Fatalf("launches = %d/%d, want 1/1", first.launches, second.launches)
	}
}

func TestLaunchStopsOnFatalError(t *testing.T) {
	first := &fakeBackend{err: sdkError("InvalidParameter.Image")}
	second := &fakeBackend{}
	s, binding := newTestScheduler(t, map[string]*fakeBackend{"a": first, "b": second}, []string{"a", "b"})

	if _, _, err := s.launch(context.Background(), binding, eci.Spec{}); err == nil {
		t.Fatal("expected a fatal error to be returned")
	}
	if second.launches != 0 {
		t.Fatalf("second target was tried %d times, want 0", second.launches)
	}
}

func TestLaunchReportsExhaustedPool(t *testing.T) {
	first := &fakeBackend{err: sdkError("StockExhausted")}
	second := &fakeBackend{err: sdkError("QuotaExceeded")}
	s, binding := newTestScheduler(t, map[string]*fakeBackend{"a": first, "b": second}, []string{"a", "b"})

	if _, _, err := s.launch(context.Background(), binding, eci.Spec{}); err == nil {
		t.Fatal("expected an error when every target is out of capacity")
	}
	if first.launches != 1 || second.launches != 1 {
		t.Fatalf("launches = %d/%d, want 1/1", first.launches, second.launches)
	}
}

func TestLaunchUsesPerTargetImage(t *testing.T) {
	first := &fakeBackend{err: sdkError("StockExhausted")}
	second := &fakeBackend{}
	s, binding := newTestScheduler(t, map[string]*fakeBackend{"a": first, "b": second}, []string{"a", "b"})
	binding.Pool.Images = map[string]string{"b": "registry/b:v2"}

	if _, target, err := s.launch(context.Background(), binding, eci.Spec{}); err != nil || target != "b" {
		t.Fatalf("launch: target %q, err %v", target, err)
	}
	if first.images[0] != "registry/default:v1" {
		t.Fatalf("first target pulled %q, want the fallback image", first.images[0])
	}
	if second.images[0] != "registry/b:v2" {
		t.Fatalf("second target pulled %q, want the override", second.images[0])
	}
}

func TestLaunchSkippableMissingBackend(t *testing.T) {
	second := &fakeBackend{}
	s, binding := newTestScheduler(t, map[string]*fakeBackend{"b": second}, []string{"missing", "b"})

	if _, target, err := s.launch(context.Background(), binding, eci.Spec{}); err != nil || target != "b" {
		t.Fatalf("launch: target %q, err %v", target, err)
	}
}

func TestLaunchWrapsFailureReasons(t *testing.T) {
	if got := eci.FailureReason(sdkError("StockExhausted")); got != eci.ReasonStock {
		t.Fatalf("reason = %q, want %q", got, eci.ReasonStock)
	}
	if got := eci.FailureReason(errors.New("boom")); got != eci.ReasonOther {
		t.Fatalf("reason = %q, want %q", got, eci.ReasonOther)
	}
}

// newReconcileScheduler builds a scheduler with one binding (repo o/r, pool
// pool-short) whose targets are the fake backends, keyed by name.
func newReconcileScheduler(t *testing.T, forge *fakeForge, backends map[string]*fakeBackend, spec pool.Spec) *Scheduler {
	t.Helper()

	if len(spec.ECI) == 0 {
		for name := range backends {
			spec.ECI = append(spec.ECI, name)
		}
		slices.Sort(spec.ECI)
	}
	if spec.Name == "" {
		spec.Name = "pool-short"
	}
	if len(spec.Labels) == 0 {
		spec.Labels = []string{"eci-pool-short"}
	}
	if spec.Image == "" {
		spec.Image = "registry/default:v1"
	}
	if spec.VCPU == 0 {
		spec.VCPU = 4
	}
	if spec.MemoryGiB == 0 {
		spec.MemoryGiB = 8
	}
	if spec.TTLMinutes == 0 {
		spec.TTLMinutes = 380
	}

	binding := config.Binding{
		Repo: github.Repo{Owner: "o", Name: "r", InstallationID: 7},
		Pool: spec,
	}
	for _, name := range spec.ECI {
		binding.Targets = append(binding.Targets, config.Target{Name: name, RegionID: name})
	}

	clouds := make(map[string]eci.Backend, len(backends))
	for name, backend := range backends {
		clouds[name] = backend
	}

	return New(Deps{
		Forge:          forge,
		Clouds:         clouds,
		Bindings:       []config.Binding{binding},
		MaxLaunches:    3,
		PendingTimeout: 3 * time.Minute,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:        metrics.New(),
	})
}

func queuedJob(id int64, labels ...string) github.Job {
	return github.Job{
		ID: id, RunID: 1, Status: "queued",
		Labels:    labels,
		CreatedAt: time.Now().Add(-30 * time.Second),
	}
}

func TestReconcileLaunchesForQueuedJobs(t *testing.T) {
	forge := &fakeForge{jobs: []github.Job{
		queuedJob(1, "self-hosted", "eci-pool-short"),
		queuedJob(2, "self-hosted", "eci-pool-short"),
		queuedJob(3, "self-hosted", "eci-pool-heavy"), // different pool
	}}
	cloud := &fakeBackend{}
	s := newReconcileScheduler(t, forge, map[string]*fakeBackend{"a": cloud}, pool.Spec{Max: 6})

	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cloud.launches != 2 {
		t.Fatalf("launches = %d, want 2", cloud.launches)
	}
	for _, name := range forge.jitNames {
		if !strings.HasPrefix(name, "eci-pool-short-") {
			t.Fatalf("runner name %q lacks the pool prefix", name)
		}
	}
	if got := forge.jitLabels[0]; !slices.Equal(got, []string{"eci-pool-short"}) {
		t.Fatalf("jit labels = %v, want the pool labels", got)
	}
}

func TestReconcileDoesNothingWhenSatisfied(t *testing.T) {
	forge := &fakeForge{jobs: []github.Job{queuedJob(1, "eci-pool-short")}}
	cloud := &fakeBackend{instances: []eci.Instance{
		{ID: "i-1", Name: "eci-pool-short-1", Status: "Running", Pool: "pool-short", Repo: "o/r"},
	}}
	s := newReconcileScheduler(t, forge, map[string]*fakeBackend{"a": cloud}, pool.Spec{Max: 6})

	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cloud.launches != 0 {
		t.Fatalf("launches = %d, want 0", cloud.launches)
	}
}

func TestReconcileLaunchesWarmInstance(t *testing.T) {
	forge := &fakeForge{}
	cloud := &fakeBackend{}
	s := newReconcileScheduler(t, forge, map[string]*fakeBackend{"a": cloud}, pool.Spec{Max: 6, Warm: 1})

	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cloud.launches != 1 {
		t.Fatalf("launches = %d, want 1 warm instance", cloud.launches)
	}
}

func TestReconcileDeletesTerminalInstance(t *testing.T) {
	forge := &fakeForge{}
	cloud := &fakeBackend{instances: []eci.Instance{
		{ID: "i-1", Name: "eci-pool-short-1", Status: "Succeeded", Pool: "pool-short", Repo: "o/r", Target: "a", CreatedAt: time.Now()},
	}}
	s := newReconcileScheduler(t, forge, map[string]*fakeBackend{"a": cloud}, pool.Spec{Max: 6})

	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !slices.Equal(cloud.terminated, []string{"i-1"}) {
		t.Fatalf("terminated = %v, want [i-1]", cloud.terminated)
	}
}

func TestReconcileDeletesStuckPending(t *testing.T) {
	forge := &fakeForge{}
	cloud := &fakeBackend{instances: []eci.Instance{
		{ID: "i-1", Name: "eci-pool-short-1", Status: "Pending", Pool: "pool-short", Repo: "o/r", Target: "a",
			CreatedAt: time.Now().Add(-10 * time.Minute)},
	}}
	s := newReconcileScheduler(t, forge, map[string]*fakeBackend{"a": cloud}, pool.Spec{Max: 6})

	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !slices.Equal(cloud.terminated, []string{"i-1"}) {
		t.Fatalf("terminated = %v, want [i-1]", cloud.terminated)
	}
}

// A long job must survive cleanup: only states that cannot run again are deleted.
func TestReconcileKeepsLongRunningInstance(t *testing.T) {
	forge := &fakeForge{}
	cloud := &fakeBackend{instances: []eci.Instance{
		{ID: "i-1", Name: "eci-pool-short-1", Status: "Running", Pool: "pool-short", Repo: "o/r", Target: "a",
			CreatedAt: time.Now().Add(-10 * time.Hour)},
	}}
	s := newReconcileScheduler(t, forge, map[string]*fakeBackend{"a": cloud}, pool.Spec{Max: 6, TTLMinutes: 60})

	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(cloud.terminated) != 0 {
		t.Fatalf("a running instance was terminated: %v", cloud.terminated)
	}
}

func TestReconcileRespectsLaunchBudget(t *testing.T) {
	forge := &fakeForge{jobs: []github.Job{
		queuedJob(1, "eci-pool-short"),
		queuedJob(2, "eci-pool-short"),
		queuedJob(3, "eci-pool-short"),
		queuedJob(4, "eci-pool-short"),
		queuedJob(5, "eci-pool-short"),
	}}
	cloud := &fakeBackend{}
	s := newReconcileScheduler(t, forge, map[string]*fakeBackend{"a": cloud}, pool.Spec{Max: 10})
	s.deps.MaxLaunches = 2

	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cloud.launches != 2 {
		t.Fatalf("launches = %d, want the 2 of the budget", cloud.launches)
	}
}

func TestReconcileSkipsLaunchWhenDescribeFails(t *testing.T) {
	forge := &fakeForge{jobs: []github.Job{queuedJob(1, "eci-pool-short")}}
	cloud := &fakeBackend{describeErr: errors.New("eci down")}
	s := newReconcileScheduler(t, forge, map[string]*fakeBackend{"a": cloud}, pool.Spec{Max: 6})

	if err := s.Reconcile(context.Background()); err == nil {
		t.Fatal("expected the describe failure to be reported")
	}
	if cloud.launches != 0 {
		t.Fatalf("launched %d instances on partial information", cloud.launches)
	}
}

func TestReconcileRemovesLeakedRunners(t *testing.T) {
	forge := &fakeForge{runners: []github.Runner{
		{ID: 1, Name: "eci-pool-short-gone", Status: "offline"},  // no container group
		{ID: 2, Name: "eci-pool-short-alive", Status: "offline"}, // still starting
		{ID: 3, Name: "eci-pool-short-busy", Status: "online", Busy: true},
	}}
	cloud := &fakeBackend{instances: []eci.Instance{
		{ID: "i-2", Name: "eci-pool-short-alive", Status: "Running", Pool: "pool-short", Repo: "o/r", Target: "a",
			CreatedAt: time.Now()},
	}}
	s := newReconcileScheduler(t, forge, map[string]*fakeBackend{"a": cloud}, pool.Spec{Max: 6})

	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !slices.Equal(forge.removed, []int64{1}) {
		t.Fatalf("removed = %v, want only the orphaned runner", forge.removed)
	}
}

func TestReconcileSkipsPruneWhileStarting(t *testing.T) {
	forge := &fakeForge{runners: []github.Runner{
		{ID: 1, Name: "eci-pool-short-gone", Status: "offline"},
	}}
	cloud := &fakeBackend{instances: []eci.Instance{
		{ID: "i-9", Name: "eci-pool-short-new", Status: "Pending", Pool: "pool-short", Repo: "o/r", Target: "a",
			CreatedAt: time.Now()},
	}}
	s := newReconcileScheduler(t, forge, map[string]*fakeBackend{"a": cloud}, pool.Spec{Max: 6})

	if err := s.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(forge.removed) != 0 {
		t.Fatalf("pruned runners while an instance was starting: %v", forge.removed)
	}
}

func TestRunnerName(t *testing.T) {
	prefix := RunnerNamePrefix("Pool_Short.1")
	if prefix != "eci-pool-short-1-" {
		t.Fatalf("prefix = %q", prefix)
	}
	name := runnerName("pool-short")
	if !strings.HasPrefix(name, "eci-pool-short-") {
		t.Fatalf("name = %q", name)
	}
	if name == runnerName("pool-short") {
		t.Fatal("expected names to vary between calls")
	}
}

func TestDeadReason(t *testing.T) {
	now := time.Now()
	const ttl = time.Hour
	const pendingTimeout = 3 * time.Minute

	tests := []struct {
		name     string
		instance eci.Instance
		want     string
	}{
		{
			name:     "healthy long job is left alone",
			instance: eci.Instance{Status: "Running", CreatedAt: now.Add(-10 * time.Hour)},
			want:     "",
		},
		{
			name:     "finished job is collected",
			instance: eci.Instance{Status: "Succeeded", CreatedAt: now},
			want:     "terminal",
		},
		{
			name:     "pending past the timeout is collected",
			instance: eci.Instance{Status: "Pending", CreatedAt: now.Add(-10 * time.Minute)},
			want:     "stuck-pending",
		},
		{
			name:     "pending within the timeout is left alone",
			instance: eci.Instance{Status: "Pending", CreatedAt: now.Add(-time.Minute)},
			want:     "",
		},
		{
			name:     "unknown state past ttl is collected",
			instance: eci.Instance{Status: "Stopping", CreatedAt: now.Add(-2 * time.Hour)},
			want:     "expired",
		},
		{
			name:     "unknown state within ttl is left alone",
			instance: eci.Instance{Status: "Stopping", CreatedAt: now.Add(-time.Minute)},
			want:     "",
		},
		{
			name:     "missing timestamp never triggers a delete",
			instance: eci.Instance{Status: "Stopping"},
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DeadReason(tt.instance, ttl, pendingTimeout, now); got != tt.want {
				t.Fatalf("DeadReason = %q, want %q", got, tt.want)
			}
		})
	}
}
