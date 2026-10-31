package pool

import (
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestPlanFillsIdleCapacity(t *testing.T) {
	// No warm and no min, so the plan is exactly the shortfall in usable capacity.
	spec := Spec{Name: "p", Max: 6}

	tests := []struct {
		name   string
		queued int
		have   Capacity
		want   int
	}{
		{
			name: "one queued job, nothing yet", queued: 1,
			want: 1,
		},
		{
			name: "an idle instance is already enough", queued: 1,
			have: Capacity{Idle: 1},
			want: 0,
		},
		{
			name: "a starting instance is enough", queued: 1,
			have: Capacity{Starting: 1},
			want: 0,
		},
		{
			// The bug this guards: an occupied instance cannot take the job, so
			// the queue has to be met by a new one.
			name: "an occupied instance is not capacity", queued: 1,
			have: Capacity{Occupied: 1},
			want: 1,
		},
		{
			// Same for a group whose runner already unregistered: it is on its way
			// out and will never run another job.
			name: "an exiting instance is not capacity", queued: 1,
			have: Capacity{Exiting: 1},
			want: 1,
		},
		{
			name: "only the missing part is launched", queued: 4,
			have: Capacity{Idle: 1, Starting: 1, Occupied: 2},
			want: 2,
		},
		{
			name: "capped by max", queued: 50,
			want: 6,
		},
		{
			name: "max counts occupied instances too", queued: 4,
			have: Capacity{Occupied: 5},
			want: 1,
		},
		{
			name: "nothing to launch at max", queued: 4,
			have: Capacity{Occupied: 6},
			want: 0,
		},
		{
			name: "negative queue is treated as empty", queued: -3,
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := spec.Plan(tt.queued, tt.have); got != tt.want {
				t.Fatalf("Plan(%d, %+v) = %d, want %d", tt.queued, tt.have, got, tt.want)
			}
		})
	}
}

// warm and min both ask for instances that can take work, so they are added to the
// queue rather than compared with the total.
func TestPlanKeepsWarmInstances(t *testing.T) {
	spec := Spec{Name: "p", Warm: 1, Max: 6}

	if got := spec.Plan(0, Capacity{}); got != 1 {
		t.Fatalf("Plan(0, empty) = %d, want the warm instance", got)
	}
	if got := spec.Plan(2, Capacity{}); got != 3 {
		t.Fatalf("Plan(2, empty) = %d, want 3", got)
	}
	if got := spec.Plan(0, Capacity{Idle: 1}); got != 0 {
		t.Fatalf("Plan(0, idle) = %d, want 0: warm is already satisfied", got)
	}
	if got := spec.Plan(0, Capacity{Occupied: 1}); got != 1 {
		t.Fatalf("Plan(0, occupied) = %d, want 1: warm is not satisfied by a busy instance", got)
	}
}

// min is a floor on instances that can take work, not on instances that exist:
// two occupied instances do not satisfy a min of two.
func TestPlanRespectsMin(t *testing.T) {
	spec := Spec{Name: "p", Min: 2, Warm: 0, Max: 6}
	if got := spec.Plan(0, Capacity{}); got != 2 {
		t.Fatalf("Plan(0, empty) = %d, want 2", got)
	}
	if got := spec.Plan(0, Capacity{Occupied: 2}); got != 2 {
		t.Fatalf("Plan(0, occupied) = %d, want 2", got)
	}
	if got := spec.Plan(0, Capacity{Idle: 2}); got != 0 {
		t.Fatalf("Plan(0, idle) = %d, want 0", got)
	}
}

func TestCapacityTotal(t *testing.T) {
	have := Capacity{Idle: 1, Starting: 2, Occupied: 3, Exiting: 4}
	if got := have.Total(); got != 10 {
		t.Fatalf("Total() = %d, want 10", got)
	}
}

func TestMatches(t *testing.T) {
	spec := Spec{Name: "p", Labels: []string{"eci-pool-short"}}
	if !spec.Matches([]string{"self-hosted", "eci-pool-short", "linux"}) {
		t.Fatal("expected the pool to match a job carrying its label")
	}
	if spec.Matches([]string{"self-hosted", "eci-pool-heavy"}) {
		t.Fatal("expected the pool to reject a job with a different pool label")
	}

	empty := Spec{Name: "p"}
	if empty.Matches([]string{"self-hosted"}) {
		t.Fatal("a pool without labels must not match anything")
	}
}

func TestValidate(t *testing.T) {
	valid := Spec{
		Name:       "pool-short",
		ECI:        Targets{"hangzhou"},
		VCPU:       4,
		MemoryGiB:  8,
		Max:        6,
		Labels:     []string{"eci-pool-short"},
		Image:      "registry.example.com/runner:v1",
		TTLMinutes: 380,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*Spec)
	}{
		{name: "missing name", mutate: func(s *Spec) { s.Name = "" }},
		{name: "zero vcpu", mutate: func(s *Spec) { s.VCPU = 0 }},
		{name: "zero memory", mutate: func(s *Spec) { s.MemoryGiB = 0 }},
		{name: "min above max", mutate: func(s *Spec) { s.Min = 7 }},
		{name: "warm above max", mutate: func(s *Spec) { s.Warm = 7 }},
		{name: "no labels", mutate: func(s *Spec) { s.Labels = nil }},
		{name: "no eci target", mutate: func(s *Spec) { s.ECI = nil }},
		{name: "duplicate eci target", mutate: func(s *Spec) { s.ECI = Targets{"a", "a"} }},
		{name: "no image", mutate: func(s *Spec) { s.Image = "" }},
		{name: "no ttl", mutate: func(s *Spec) { s.TTLMinutes = 0 }},
		{name: "empty image override", mutate: func(s *Spec) { s.Images = map[string]string{"b": ""} }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := valid
			tt.mutate(&spec)
			if err := spec.Validate(); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestTargetsUnmarshal(t *testing.T) {
	tests := []struct {
		name string
		body string
		want Targets
	}{
		{name: "scalar", body: "eci: hangzhou", want: Targets{"hangzhou"}},
		{name: "sequence", body: "eci: [hangzhou, beijing]", want: Targets{"hangzhou", "beijing"}},
		{
			name: "block sequence",
			body: "eci:\n  - hangzhou\n  - beijing",
			want: Targets{"hangzhou", "beijing"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got struct {
				ECI Targets `yaml:"eci"`
			}
			if err := yaml.Unmarshal([]byte(tt.body), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !slices.Equal(got.ECI, tt.want) {
				t.Fatalf("got %v, want %v", got.ECI, tt.want)
			}
		})
	}
}

func TestTargetsUnmarshalRejectsMapping(t *testing.T) {
	var got struct {
		ECI Targets `yaml:"eci"`
	}
	if err := yaml.Unmarshal([]byte("eci:\n  name: hangzhou"), &got); err == nil {
		t.Fatal("expected a mapping to be rejected")
	}
}

func TestImageFor(t *testing.T) {
	spec := Spec{Image: "registry/base:v1", Images: map[string]string{"beijing": "registry/bj:v2"}}
	if got := spec.ImageFor("beijing"); got != "registry/bj:v2" {
		t.Fatalf("ImageFor(beijing) = %q, want the override", got)
	}
	if got := spec.ImageFor("hangzhou"); got != "registry/base:v1" {
		t.Fatalf("ImageFor(hangzhou) = %q, want the fallback", got)
	}
}
