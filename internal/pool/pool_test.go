package pool

import (
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDesired(t *testing.T) {
	spec := Spec{Name: "p", Min: 0, Warm: 1, Max: 6}

	tests := []struct {
		name   string
		queued int
		want   int
	}{
		{name: "warm covers an empty queue", queued: 0, want: 1},
		{name: "queued jobs add to warm", queued: 2, want: 3},
		{name: "capped by max", queued: 50, want: 6},
		{name: "negative queue is treated as empty", queued: -3, want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := spec.Desired(tt.queued); got != tt.want {
				t.Fatalf("Desired(%d) = %d, want %d", tt.queued, got, tt.want)
			}
		})
	}
}

func TestDesiredRespectsMin(t *testing.T) {
	spec := Spec{Name: "p", Min: 2, Warm: 0, Max: 6}
	if got := spec.Desired(0); got != 2 {
		t.Fatalf("Desired(0) = %d, want 2", got)
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
