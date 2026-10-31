// Package pool defines the static shape of a runner pool and the pure helpers
// that map a workflow's runs-on labels onto it.
package pool

import (
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Targets is the ordered list of ECI targets a pool may use. The first target
// that has capacity takes the instance and the rest are fallbacks, so a stock
// out in one zone does not fail the job.
type Targets []string

// UnmarshalYAML accepts both `eci: hangzhou` and `eci: [hangzhou, beijing]`.
func (t *Targets) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		var name string
		if err := value.Decode(&name); err != nil {
			return err
		}
		*t = Targets{name}
	case yaml.SequenceNode:
		var names []string
		if err := value.Decode(&names); err != nil {
			return err
		}
		*t = names
	default:
		return fmt.Errorf("line %d: eci must be a target name or a list of target names", value.Line)
	}
	return nil
}

// Primary returns the preferred target, or "" when none is set.
func (t Targets) Primary() string {
	if len(t) == 0 {
		return ""
	}
	return t[0]
}

func (t Targets) validate() error {
	if len(t) == 0 {
		return fmt.Errorf("at least one eci target is required")
	}
	seen := make(map[string]struct{}, len(t))
	for _, name := range t {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("eci targets must not be empty")
		}
		if _, dup := seen[name]; dup {
			return fmt.Errorf("eci target %q is listed twice", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

// Spec is one runner pool as declared in the configuration file. A pool maps a
// set of runner labels onto a single ECI shape (vCPU/memory/image).
type Spec struct {
	Name      string   `yaml:"name" json:"name"`
	VCPU      float32  `yaml:"vcpu" json:"vcpu"`
	MemoryGiB float32  `yaml:"memory_gib" json:"memory_gib"`
	Min       int      `yaml:"min" json:"min"`
	Warm      int      `yaml:"warm" json:"warm"`
	Max       int      `yaml:"max" json:"max"`
	Labels    []string `yaml:"labels" json:"labels"`
	// ECI lists the targets this pool may use, in preference order.
	ECI Targets `yaml:"eci" json:"eci"`
	// Image is the fallback image, used for any target without an override.
	Image string `yaml:"image" json:"image"`
	// Images overrides Image per target. A container registry endpoint is
	// regional, so pulling a cross-region image is slow and billable.
	Images     map[string]string `yaml:"images" json:"images"`
	TTLMinutes int               `yaml:"ttl_minutes" json:"ttl_minutes"`
	Spot       bool              `yaml:"spot" json:"spot"`
	// Privileged creates the container group privileged and starts a docker
	// daemon inside the runner container, so a job can run `docker build`. ECI
	// grants privileged containers only to whitelisted accounts. The daemon
	// shares the runner's lifecycle on purpose: a second container that never
	// exits would keep the group Running after the job and delay its deletion
	// until the TTL.
	Privileged bool `yaml:"privileged" json:"privileged"`
}

// ImageFor returns the image to pull in target, falling back to Image.
func (s Spec) ImageFor(target string) string {
	if image, ok := s.Images[target]; ok {
		return image
	}
	return s.Image
}

// Capacity is what a pool already has, split by whether an instance can still
// take a job. Counting container groups would not answer that: the Running state
// covers an idle runner, a busy one, and a group whose runner has already
// unregistered, and only the first can start a job right now.
type Capacity struct {
	// Idle instances are registered, online and not busy: a job assigned now
	// starts immediately.
	Idle int
	// Starting instances cannot take a job yet but will shortly: a container
	// group that has not reached Running, a runner that is registered but has not
	// connected, or a launch the platform has not listed.
	Starting int
	// Occupied instances are running a job and will not accept another.
	Occupied int
	// Exiting instances still exist but their runner is already gone, so the
	// group is only waiting to be collected.
	Exiting int
}

// Total is how many instances exist or are on their way. Max caps this, not the
// idle count: an occupied instance costs exactly as much as an idle one.
func (c Capacity) Total() int { return c.Idle + c.Starting + c.Occupied + c.Exiting }

// Plan returns the raw shortfall: how many container groups the queue needs that
// the pool cannot serve yet.
//
// The target is a number of instances that can take work, so an occupied or
// exiting group never counts towards it. Otherwise a pool that is full of running
// jobs would decide it has enough machines and leave the next job queued until an
// unrelated job happens to finish.
//
// The scheduler damps the result before acting on it; see settleShortfall.
func (s Spec) Plan(queued int, have Capacity) int {
	if queued < 0 {
		queued = 0
	}
	need := max(queued+s.Warm, s.Min)
	usable := have.Idle + have.Starting
	if need <= usable {
		return 0
	}
	return max(0, min(need-usable, s.Max-have.Total()))
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
	}
	if err := s.ECI.validate(); err != nil {
		return fmt.Errorf("pool %q: %w", s.Name, err)
	}
	switch {
	case strings.TrimSpace(s.Image) == "":
		return fmt.Errorf("pool %q: image is required", s.Name)
	case s.TTLMinutes <= 0:
		return fmt.Errorf("pool %q: ttl_minutes must be greater than 0", s.Name)
	}
	for target, image := range s.Images {
		if strings.TrimSpace(target) == "" || strings.TrimSpace(image) == "" {
			return fmt.Errorf("pool %q: every images entry needs a target and an image", s.Name)
		}
	}
	return nil
}
