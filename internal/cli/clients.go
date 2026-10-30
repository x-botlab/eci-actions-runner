package cli

import (
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
		AppID:          cfg.GitHub.AppID,
		ClientID:       cfg.GitHub.ClientID,
		PrivateKeyPath: cfg.GitHub.PrivateKeyPath,
	})
}

// buildClouds creates one backend per configured ECI target.
func buildClouds(cfg *config.Config) (map[string]eci.Backend, error) {
	clouds := make(map[string]eci.Backend, len(cfg.ECI.Targets))
	for _, target := range cfg.ECI.Targets {
		client, err := eci.New(eci.Config{
			Name:            target.Name,
			RegionID:        target.RegionID,
			Endpoint:        target.Endpoint,
			VSwitchIDs:      target.VSwitchIDs,
			SecurityGroupID: target.SecurityGroupID,
			RAMRoleName:     target.RAMRoleName,
		})
		if err != nil {
			return nil, err
		}
		clouds[target.Name] = client
	}
	return clouds, nil
}
