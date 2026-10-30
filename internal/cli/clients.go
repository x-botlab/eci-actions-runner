package cli

import (
	"fmt"

	"github.com/x-botlab/eci-actions-runner/internal/config"
	"github.com/x-botlab/eci-actions-runner/internal/eci"
	"github.com/x-botlab/eci-actions-runner/internal/github"
)

// cloudClients loads the configuration and creates one backend per ECI target.
func cloudClients(configPath string) (*config.Config, map[string]eci.Backend, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, nil, err
	}
	clouds, err := buildClouds(cfg)
	if err != nil {
		return nil, nil, err
	}
	return cfg, clouds, nil
}

// buildForge creates the GitHub App client from the configuration.
func buildForge(cfg *config.Config) (*github.AppClient, error) {
	return github.NewAppClient(github.Config{
		ClientID:       cfg.GitHub.ClientID,
		PrivateKeyPath: cfg.GitHub.PrivateKeyPath,
	})
}

// buildClouds creates one backend per configured ECI target.
func buildClouds(cfg *config.Config) (map[string]eci.Backend, error) {
	registries := make([]eci.Registry, 0, len(cfg.ECI.Registries))
	for _, registry := range cfg.ECI.Registries {
		registries = append(registries, eci.Registry{
			Server:      registry.Server,
			UsernameEnv: registry.UsernameEnv,
			PasswordEnv: registry.PasswordEnv,
		})
	}

	clouds := make(map[string]eci.Backend, len(cfg.ECI.Targets))
	for _, target := range cfg.ECI.Targets {
		client, err := eci.New(eci.Config{
			Name:            target.Name,
			RegionID:        target.RegionID,
			Endpoint:        target.Endpoint,
			VSwitchIDs:      target.VSwitchIDs,
			SecurityGroupID: target.SecurityGroupID,
			RAMRoleName:     target.RAMRoleName,
			AutoCreateEIP:   target.AutoCreateEIP,
			EIPBandwidth:    target.EIPBandwidth,
			Registries:      registries,
		})
		if err != nil {
			return nil, err
		}
		clouds[target.Name] = client
	}
	return clouds, nil
}

// validateClouds fails when a backend is missing something only discoverable at
// call time. The server uses it so a bad environment stops the process at
// start-up; diagnostic verbs skip it so they still work during an incident.
func validateClouds(clouds map[string]eci.Backend) error {
	for name, backend := range clouds {
		if err := backend.Validate(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}
