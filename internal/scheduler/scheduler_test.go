package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

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
}

func (f *fakeBackend) Launch(_ context.Context, spec eci.Spec) (string, error) {
	f.launches++
	f.images = append(f.images, spec.Image)
	if f.err != nil {
		return "", f.err
	}
	return "eci-" + spec.Pool, nil
}

func (f *fakeBackend) Describe(context.Context, eci.Selector) ([]eci.Instance, error) {
	return nil, nil
}

func (f *fakeBackend) Terminate(context.Context, string) error { return nil }

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
