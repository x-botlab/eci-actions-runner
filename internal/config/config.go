// Package config loads and validates the single configuration file that drives
// the scheduler. Pool definitions live in Git, so a change is a pull request
// rather than a click in a control panel.
package config

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/x-botlab/eci-actions-runner/internal/pool"
)

// Config is the on-disk configuration.
type Config struct {
	GitHub GitHub      `yaml:"github"`
	ECI    ECI         `yaml:"eci"`
	Runner Runner      `yaml:"runner"`
	Pools  []pool.Spec `yaml:"pools"`
}

// GitHub identifies the App installation used to mint just-in-time runner configs.
type GitHub struct {
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	PrivateKeyPath string `yaml:"private_key_path"`
	Owner          string `yaml:"owner"`
	Repo           string `yaml:"repo"`
}

// ECI describes the Alibaba Cloud region and network the container groups land in.
type ECI struct {
	RegionID        string   `yaml:"region_id"`
	Endpoint        string   `yaml:"endpoint"`
	VSwitchIDs      []string `yaml:"vswitch_ids"`
	SecurityGroupID string   `yaml:"security_group_id"`
	RAMRoleName     string   `yaml:"ram_role_name"`
}

// Runner holds scheduler behaviour that is shared by every pool.
type Runner struct {
	WorkFolder            string `yaml:"work_folder"`
	PollIntervalSeconds   int    `yaml:"poll_interval_seconds"`
	MaxConcurrentLaunches int    `yaml:"max_concurrent_launches"`
	PendingTimeoutSeconds int    `yaml:"pending_timeout_seconds"`
}

// Load reads the file at path on top of the built-in defaults and validates it.
// Unknown keys are rejected so that a typo in a label or field name fails loudly
// instead of silently queueing jobs for 24 hours.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := Defaults()
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Defaults returns the configuration used when a field is absent.
func Defaults() *Config {
	return &Config{
		Runner: Runner{
			WorkFolder:            "_work",
			PollIntervalSeconds:   30,
			MaxConcurrentLaunches: 5,
			PendingTimeoutSeconds: 180,
		},
	}
}

// Validate reports the first structural problem with the configuration.
func (c *Config) Validate() error {
	if c.GitHub.AppID <= 0 {
		return fmt.Errorf("github.app_id is required")
	}
	if c.GitHub.InstallationID <= 0 {
		return fmt.Errorf("github.installation_id is required")
	}
	if strings.TrimSpace(c.GitHub.PrivateKeyPath) == "" {
		return fmt.Errorf("github.private_key_path is required")
	}
	if strings.TrimSpace(c.GitHub.Owner) == "" || strings.TrimSpace(c.GitHub.Repo) == "" {
		return fmt.Errorf("github.owner and github.repo are required")
	}
	if strings.TrimSpace(c.ECI.RegionID) == "" {
		return fmt.Errorf("eci.region_id is required")
	}
	if len(c.ECI.VSwitchIDs) == 0 {
		return fmt.Errorf("eci.vswitch_ids must contain at least one switch")
	}
	if strings.TrimSpace(c.ECI.SecurityGroupID) == "" {
		return fmt.Errorf("eci.security_group_id is required")
	}
	if c.Runner.PollIntervalSeconds <= 0 {
		return fmt.Errorf("runner.poll_interval_seconds must be greater than 0")
	}
	if c.Runner.MaxConcurrentLaunches <= 0 {
		return fmt.Errorf("runner.max_concurrent_launches must be greater than 0")
	}
	if len(c.Pools) == 0 {
		return fmt.Errorf("at least one pool is required")
	}

	// A label claimed by two pools makes routing ambiguous: two different ECI
	// shapes would both look like valid targets for the same runs-on value.
	seenNames := make(map[string]struct{}, len(c.Pools))
	seenLabels := make(map[string]string, len(c.Pools))
	for _, p := range c.Pools {
		if err := p.Validate(); err != nil {
			return err
		}
		if _, dup := seenNames[p.Name]; dup {
			return fmt.Errorf("pool name %q is declared twice", p.Name)
		}
		seenNames[p.Name] = struct{}{}
		for _, label := range p.Labels {
			if prev, dup := seenLabels[label]; dup {
				return fmt.Errorf("label %q is claimed by both pool %q and pool %q", label, prev, p.Name)
			}
			seenLabels[label] = p.Name
		}
	}
	return nil
}

// Pool returns the pool with the given name.
func (c *Config) Pool(name string) (pool.Spec, bool) {
	for _, p := range c.Pools {
		if p.Name == name {
			return p, true
		}
	}
	return pool.Spec{}, false
}
