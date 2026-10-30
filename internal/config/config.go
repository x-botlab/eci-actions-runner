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

	"github.com/x-botlab/eci-actions-runner/internal/github"
	"github.com/x-botlab/eci-actions-runner/internal/pool"
)

// Config is the on-disk configuration.
type Config struct {
	GitHub GitHub      `yaml:"github"`
	ECI    ECI         `yaml:"eci"`
	Runner Runner      `yaml:"runner"`
	Pools  []pool.Spec `yaml:"pools"`
}

// GitHub identifies the App used to mint just-in-time runner configs. The App
// is installed once per account, and one installation can cover many repos, so
// repositories are grouped by the installation that authorises them.
type GitHub struct {
	// ClientID is the OAuth client ID, the App's public identifier and the value
	// GitHub recommends as the JWT issuer.
	ClientID       string         `yaml:"client_id"`
	PrivateKeyPath string         `yaml:"private_key_path"`
	Installations  []Installation `yaml:"installations"`
}

// Installation is one App installation and the repositories it can mint tokens for.
type Installation struct {
	ID    int64  `yaml:"id"`
	Repos []Repo `yaml:"repos"`
}

// Repo is one repository served by an installation. An empty pools list means
// every declared pool, so a new repo does not silently miss a pool that already
// exists.
type Repo struct {
	Owner string   `yaml:"owner"`
	Name  string   `yaml:"name"`
	Pools []string `yaml:"pools"`
}

// FullName returns owner/name.
func (r Repo) FullName() string { return r.Owner + "/" + r.Name }

// ECI holds the container group targets. One target is one region with its own
// switches, security group and instance role; pools choose a target by name.
type ECI struct {
	Targets []Target `yaml:"targets"`
}

// Target is one place container groups can be created.
type Target struct {
	Name            string   `yaml:"name"`
	RegionID        string   `yaml:"region_id"`
	Endpoint        string   `yaml:"endpoint"`
	VSwitchIDs      []string `yaml:"vswitch_ids"`
	SecurityGroupID string   `yaml:"security_group_id"`
	RAMRoleName     string   `yaml:"ram_role_name"`
}

// Binding is one schedulable unit: a repository, the pool serving it and the
// ECI targets that pool may use, in preference order.
type Binding struct {
	Repo    github.Repo
	Pool    pool.Spec
	Targets []Target
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
	if err := c.validateGitHub(); err != nil {
		return err
	}
	if err := c.validateECI(); err != nil {
		return err
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

	targets := make(map[string]struct{}, len(c.ECI.Targets))
	for _, t := range c.ECI.Targets {
		targets[t.Name] = struct{}{}
	}

	// Pool names are identifiers: they must be unique and they name a target.
	seenNames := make(map[string]struct{}, len(c.Pools))
	for _, p := range c.Pools {
		if err := p.Validate(); err != nil {
			return err
		}
		if _, dup := seenNames[p.Name]; dup {
			return fmt.Errorf("pool name %q is declared twice", p.Name)
		}
		seenNames[p.Name] = struct{}{}
		for _, name := range p.ECI {
			if _, ok := targets[name]; !ok {
				return fmt.Errorf("pool %q: unknown eci target %q", p.Name, name)
			}
		}
		for name := range p.Images {
			if _, ok := targets[name]; !ok {
				return fmt.Errorf("pool %q: images refers to unknown eci target %q", p.Name, name)
			}
		}
	}

	for _, inst := range c.GitHub.Installations {
		for _, repo := range inst.Repos {
			for _, name := range repo.Pools {
				if _, ok := seenNames[name]; !ok {
					return fmt.Errorf("%s: unknown pool %q", repo.FullName(), name)
				}
			}
			// Labels only have to be unique among the pools one repo binds:
			// runners are per-repository, so the same label may name a
			// different shape in a different repo.
			seenLabels := make(map[string]string, len(c.Pools))
			for _, spec := range c.poolsFor(repo) {
				for _, label := range spec.Labels {
					if prev, dup := seenLabels[label]; dup {
						return fmt.Errorf("%s: label %q is claimed by both pool %q and pool %q",
							repo.FullName(), label, prev, spec.Name)
					}
					seenLabels[label] = spec.Name
				}
			}
		}
	}
	return nil
}

func (c *Config) validateGitHub() error {
	if strings.TrimSpace(c.GitHub.ClientID) == "" {
		return fmt.Errorf("github.client_id is required")
	}
	if strings.TrimSpace(c.GitHub.PrivateKeyPath) == "" {
		return fmt.Errorf("github.private_key_path is required")
	}
	if len(c.GitHub.Installations) == 0 {
		return fmt.Errorf("github.installations must contain at least one installation")
	}

	seenInstallations := make(map[int64]struct{}, len(c.GitHub.Installations))
	// A repository is served by exactly one installation: the token used to
	// create its runner has to come from the installation that owns it.
	seenRepos := make(map[string]string, len(c.GitHub.Installations))
	for _, inst := range c.GitHub.Installations {
		if inst.ID <= 0 {
			return fmt.Errorf("github.installations: id is required")
		}
		if _, dup := seenInstallations[inst.ID]; dup {
			return fmt.Errorf("installation %d is declared twice", inst.ID)
		}
		seenInstallations[inst.ID] = struct{}{}
		if len(inst.Repos) == 0 {
			return fmt.Errorf("installation %d: at least one repo is required", inst.ID)
		}
		for _, repo := range inst.Repos {
			if strings.TrimSpace(repo.Owner) == "" || strings.TrimSpace(repo.Name) == "" {
				return fmt.Errorf("installation %d: every repo needs an owner and a name", inst.ID)
			}
			if prev, dup := seenRepos[repo.FullName()]; dup {
				return fmt.Errorf("%s is listed by installation %s and installation %d", repo.FullName(), prev, inst.ID)
			}
			seenRepos[repo.FullName()] = fmt.Sprint(inst.ID)
		}
	}
	return nil
}

func (c *Config) validateECI() error {
	if len(c.ECI.Targets) == 0 {
		return fmt.Errorf("eci.targets must contain at least one target")
	}
	seen := make(map[string]struct{}, len(c.ECI.Targets))
	for _, t := range c.ECI.Targets {
		if strings.TrimSpace(t.Name) == "" {
			return fmt.Errorf("eci.targets: name is required")
		}
		if _, dup := seen[t.Name]; dup {
			return fmt.Errorf("eci target %q is declared twice", t.Name)
		}
		seen[t.Name] = struct{}{}
		if strings.TrimSpace(t.RegionID) == "" {
			return fmt.Errorf("eci target %q: region_id is required", t.Name)
		}
		if len(t.VSwitchIDs) == 0 {
			return fmt.Errorf("eci target %q: vswitch_ids must contain at least one switch", t.Name)
		}
		if strings.TrimSpace(t.SecurityGroupID) == "" {
			return fmt.Errorf("eci target %q: security_group_id is required", t.Name)
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

// Target returns the ECI target with the given name.
func (c *Config) Target(name string) (Target, bool) {
	for _, t := range c.ECI.Targets {
		if t.Name == name {
			return t, true
		}
	}
	return Target{}, false
}

// Bindings expands the configuration into one schedulable unit per repo and
// pool. A repo without an explicit pool list receives every pool.
func (c *Config) Bindings() []Binding {
	var bindings []Binding
	for _, inst := range c.GitHub.Installations {
		for _, repo := range inst.Repos {
			ref := github.Repo{
				Owner:          repo.Owner,
				Name:           repo.Name,
				InstallationID: inst.ID,
			}
			for _, spec := range c.poolsFor(repo) {
				targets := make([]Target, 0, len(spec.ECI))
				for _, name := range spec.ECI {
					if target, ok := c.Target(name); ok {
						targets = append(targets, target)
					}
				}
				bindings = append(bindings, Binding{Repo: ref, Pool: spec, Targets: targets})
			}
		}
	}
	return bindings
}

func (c *Config) poolsFor(repo Repo) []pool.Spec {
	if len(repo.Pools) == 0 {
		return c.Pools
	}
	pools := make([]pool.Spec, 0, len(repo.Pools))
	for _, name := range repo.Pools {
		if spec, ok := c.Pool(name); ok {
			pools = append(pools, spec)
		}
	}
	return pools
}
