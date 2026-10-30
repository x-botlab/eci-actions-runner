// Package eci wraps the Alibaba Cloud ECI OpenAPI behind a small interface so
// the scheduler can be exercised against a fake during tests. One Client is one
// ECI target: a region, a set of switches and one security group.
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

// Config pins the region and network one target creates container groups in.
// More than one switch lets a launch fall back to another zone when the first
// one runs out of stock.
type Config struct {
	Name            string
	RegionID        string
	Endpoint        string
	VSwitchIDs      []string
	SecurityGroupID string
	RAMRoleName     string
}

// Spec is one container group to create: exactly one runner, exactly one job.
type Spec struct {
	Name      string
	Pool      string
	Repo      string
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
	ID     string
	Name   string
	Status string
	Pool   string
	Repo   string
	JobID  string
	// Target is the ECI target the instance lives in, needed because a delete
	// has to go to the same regional endpoint that created it.
	Target    string
	CreatedAt time.Time
}

// Selector filters instances by their tags. Empty fields match everything.
type Selector struct {
	Pool  string
	Repo  string
	JobID string
}

// Backend is the only surface the scheduler uses to reach ECI.
type Backend interface {
	Launch(ctx context.Context, spec Spec) (string, error)
	Describe(ctx context.Context, sel Selector) ([]Instance, error)
	Terminate(ctx context.Context, id string) error
}

// Failure reasons, also used as metric labels.
const (
	ReasonStock = "stock"
	ReasonQuota = "quota"
	ReasonOther = "other"
)

// capacityCodes maps ECI error codes onto a failure reason. Anything not listed
// is fatal for the whole pool: another region cannot fix a bad credential, a bad
// parameter or a missing image.
var capacityCodes = map[string]string{
	"StockExhausted":           ReasonStock,
	"InvalidOperation.NoStock": ReasonStock,
	"QuotaExceeded":            ReasonQuota,
	"Throttling":               ReasonQuota,
	"Throttling.User":          ReasonQuota,
	"Throttling.Api":           ReasonQuota,
	"ServiceUnavailable":       ReasonQuota,
}

// FailureReason buckets err for metrics.
func FailureReason(err error) string {
	var sdkErr *tea.SDKError
	if errors.As(err, &sdkErr) && sdkErr.Code != nil {
		if reason, ok := capacityCodes[*sdkErr.Code]; ok {
			return reason
		}
	}
	return ReasonOther
}

// IsCapacityError reports whether another target could still accept the
// instance: the zone is out of stock, or the call was throttled.
func IsCapacityError(err error) bool {
	return FailureReason(err) != ReasonOther
}

// Client talks to the ECI OpenAPI.
type Client struct {
	api *eciclient.Client
	cfg Config
}

// New builds an ECI client for one target. On ECI or ECS the instance RAM role
// is preferred so that no static access key has to be stored anywhere; an
// env-provided key pair is accepted for local development only.
func New(cfg Config) (*Client, error) {
	if cfg.RegionID == "" {
		return nil, fmt.Errorf("eci %q: region is required", cfg.Name)
	}
	if len(cfg.VSwitchIDs) == 0 {
		return nil, fmt.Errorf("eci %q: at least one vswitch is required", cfg.Name)
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
		return nil, fmt.Errorf("eci %q: build credential: %w", cfg.Name, err)
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
		return nil, fmt.Errorf("eci %q: new client: %w", cfg.Name, err)
	}

	return &Client{api: api, cfg: cfg}, nil
}

// Target returns the name of the ECI target this client talks to.
func (c *Client) Target() string { return c.cfg.Name }

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
