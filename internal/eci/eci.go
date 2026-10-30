// Package eci wraps the Alibaba Cloud ECI OpenAPI behind a small interface so
// the scheduler can be exercised against a fake during tests.
package eci

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/client"
	eciclient "github.com/alibabacloud-go/eci-20180808/v3/client"
	"github.com/alibabacloud-go/tea/tea"
	"github.com/aliyun/credentials-go/credentials"
)

// ErrNotImplemented marks behaviour the scaffold deliberately leaves out.
var ErrNotImplemented = errors.New("eci: not implemented")

// Config pins the region and network every container group is created in.
type Config struct {
	RegionID        string
	Endpoint        string
	VSwitchID       string
	SecurityGroupID string
	RAMRoleName     string
}

// Spec is one container group to create: exactly one runner, exactly one job.
type Spec struct {
	Name      string
	Pool      string
	JobID     string
	VCPU      float32
	MemoryGiB float32
	Image     string
	// Env carries the one-shot JIT config so that no long-lived credential
	// has to be baked into the runner image.
	Env  map[string]string
	TTL  time.Duration
	Spot bool
}

// Instance is a container group as reported by DescribeContainerGroups.
type Instance struct {
	ID        string
	Name      string
	Status    string
	Pool      string
	JobID     string
	CreatedAt time.Time
}

// Selector filters instances by their tags.
type Selector struct {
	Pool  string
	JobID string
}

// Backend is the only surface the scheduler uses to reach ECI.
type Backend interface {
	Launch(ctx context.Context, spec Spec) (string, error)
	Describe(ctx context.Context, sel Selector) ([]Instance, error)
	Terminate(ctx context.Context, id string) error
}

// Client talks to the ECI OpenAPI.
type Client struct {
	api *eciclient.Client
	cfg Config
}

// New builds an ECI client. On ECI or ECS the instance RAM role is preferred so
// that no static access key has to be stored anywhere; an env-provided key pair
// is accepted for local development only.
func New(cfg Config) (*Client, error) {
	if cfg.RegionID == "" {
		return nil, errors.New("eci: region is required")
	}

	credCfg := &credentials.Config{Type: tea.String("ecs_ram_role")}
	if id, secret := os.Getenv("ALIBABA_CLOUD_ACCESS_KEY_ID"), os.Getenv("ALIBABA_CLOUD_ACCESS_KEY_SECRET"); id != "" && secret != "" {
		credCfg = &credentials.Config{
			Type:            tea.String("access_key"),
			AccessKeyId:     tea.String(id),
			AccessKeySecret: tea.String(secret),
			SecurityToken:   tea.String(os.Getenv("ALIBABA_CLOUD_SECURITY_TOKEN")),
		}
	}
	cred, err := credentials.NewCredential(credCfg)
	if err != nil {
		return nil, fmt.Errorf("eci: build credential: %w", err)
	}

	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = fmt.Sprintf("eci.%s.aliyuncs.com", cfg.RegionID)
	}
	api, err := eciclient.NewClient(&openapi.Config{
		Credential: cred,
		Endpoint:   tea.String(endpoint),
		RegionId:   tea.String(cfg.RegionID),
	})
	if err != nil {
		return nil, fmt.Errorf("eci: new client: %w", err)
	}

	return &Client{api: api, cfg: cfg}, nil
}

// Launch creates one container group and returns its id.
func (c *Client) Launch(ctx context.Context, spec Spec) (string, error) {
	return "", ErrNotImplemented
}

// Describe lists the container groups matching sel, following pagination.
func (c *Client) Describe(ctx context.Context, sel Selector) ([]Instance, error) {
	return nil, ErrNotImplemented
}

// Terminate deletes one container group.
func (c *Client) Terminate(ctx context.Context, id string) error {
	return ErrNotImplemented
}

var _ Backend = (*Client)(nil)
