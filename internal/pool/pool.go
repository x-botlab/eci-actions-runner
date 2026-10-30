// Package pool defines the static shape of a runner pool and the pure helpers
// that map a workflow's runs-on labels onto it.
package pool

import (
	"fmt"
	"slices"
	"strings"
)

// Spec is one runner pool as declared in the configuration file. A pool maps a
// set of runner labels onto a single ECI shape (vCPU/memory/image).
type Spec struct {
	Name       string   `yaml:"name" json:"name"`
	VCPU       float32  `yaml:"vcpu" json:"vcpu"`
	MemoryGiB  float32  `yaml:"memory_gib" json:"memory_gib"`
	Min        int      `yaml:"min" json:"min"`
	Warm       int      `yaml:"warm" json:"warm"`
	Max        int      `yaml:"max" json:"max"`
	Labels     []string `yaml:"labels" json:"labels"`
	Image      string   `yaml:"image" json:"image"`
	TTLMinutes int      `yaml:"ttl_minutes" json:"ttl_minutes"`
	Spot       bool     `yaml:"spot" json:"spot"`
}

// Desired returns how many instances the pool should hold for a queue depth.
func (s Spec) Desired(queued int) int {
	if queued < 0 {
		queued = 0
	}
	return min(max(queued+s.Warm, s.Min), s.Max)
}

// Matches reports whether a job asking for jobLabels can run in this pool.
// GitHub treats runs-on as an AND over labels, so every pool label must be present.
func (s Spec) Matches(jobLabels []string) bool {
	if len(s.Labels) == 0 {
		return false
	}
	for _, want := range s.Labels {
		if !slices.Contains(jobLabels, want) {
			return false
		}
	}
	return true
}

// Validate reports the first structural problem with the pool definition.
func (s Spec) Validate() error {
	switch {
	case strings.TrimSpace(s.Name) == "":
		return fmt.Errorf("pool: name is required")
	case s.VCPU <= 0:
		return fmt.Errorf("pool %q: vcpu must be greater than 0", s.Name)
	case s.MemoryGiB <= 0:
		return fmt.Errorf("pool %q: memory_gib must be greater than 0", s.Name)
	case s.Min < 0 || s.Warm < 0 || s.Max < 0:
		return fmt.Errorf("pool %q: min, warm and max must not be negative", s.Name)
	case s.Min > s.Max:
		return fmt.Errorf("pool %q: min (%d) exceeds max (%d)", s.Name, s.Min, s.Max)
	case s.Warm > s.Max:
		return fmt.Errorf("pool %q: warm (%d) exceeds max (%d)", s.Name, s.Warm, s.Max)
	case len(s.Labels) == 0:
		return fmt.Errorf("pool %q: at least one label is required", s.Name)
	case strings.TrimSpace(s.Image) == "":
		return fmt.Errorf("pool %q: image is required", s.Name)
	case s.TTLMinutes <= 0:
		return fmt.Errorf("pool %q: ttl_minutes must be greater than 0", s.Name)
	}
	return nil
}
