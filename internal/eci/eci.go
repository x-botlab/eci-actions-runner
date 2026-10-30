// Package eci wraps the Alibaba Cloud ECI OpenAPI behind a small interface so
// the scheduler can be exercised against a fake during tests. One Client is one
// ECI target: a region, a set of switches and one security group.
package eci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"time"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/client"
	eciclient "github.com/alibabacloud-go/eci-20180808/v3/client"
	"github.com/alibabacloud-go/tea/tea"
	"github.com/aliyun/credentials-go/credentials"
)

// ErrNotImplemented marks behaviour that is still missing.
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

// Container group layout. The official actions-runner image reads the one-shot
// JIT config from the environment, so no registration token is ever baked in.
const (
	runnerContainerName = "runner"
	runnerEntrypoint    = "/home/runner/run.sh"

	// EnvJITConfig is the variable the actions-runner image reads its one-shot
	// registration from.
	EnvJITConfig = "ACTIONS_RUNNER_INPUT_JITCONFIG"

	tagManagedBy = "managed-by"
	tagPool      = "pool"
	tagRepo      = "repo"
	tagJob       = "job-id"
	tagTarget    = "target"

	managedByValue = "eci-actions-runner"
)

// Instance states, grouped by whether the container group can still take a job.
var (
	activeStates = map[string]struct{}{
		"Pending": {}, "Scheduling": {}, "Restarting": {}, "Running": {},
	}
	terminalStates = map[string]struct{}{
		"Succeeded": {}, "Failed": {}, "ScheduleFailed": {}, "Expired": {},
	}
)

// Active reports whether the container group may still take a job.
func (i Instance) Active() bool {
	_, ok := activeStates[i.Status]
	return ok
}

// Terminal reports whether the container group will never run again.
func (i Instance) Terminal() bool {
	_, ok := terminalStates[i.Status]
	return ok
}

// Launch creates one container group and returns its id. Each switch is tried in
// turn, because they sit in different zones of the same region and a stock out in
// one zone should not fail the launch.
func (c *Client) Launch(ctx context.Context, spec Spec) (string, error) {
	if spec.Name == "" {
		return "", errors.New("eci: container group name is required")
	}
	if spec.Image == "" {
		return "", errors.New("eci: image is required")
	}

	var lastErr error
	for _, vswitch := range c.cfg.VSwitchIDs {
		id, err := c.launchIn(ctx, spec, vswitch)
		if err == nil {
			return id, nil
		}
		if !IsCapacityError(err) {
			return "", err
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("eci %q: no vswitch configured", c.cfg.Name)
	}
	return "", lastErr
}

func (c *Client) launchIn(ctx context.Context, spec Spec, vswitch string) (string, error) {
	request := &eciclient.CreateContainerGroupRequest{
		// The client token makes the call idempotent, so a retried launch returns
		// the same container group instead of creating a second one.
		ClientToken:         tea.String(clientToken(spec.Name)),
		ContainerGroupName:  tea.String(spec.Name),
		VSwitchId:           tea.String(vswitch),
		SecurityGroupId:     tea.String(c.cfg.SecurityGroupID),
		Cpu:                 tea.Float32(spec.VCPU),
		Memory:              tea.Float32(spec.MemoryGiB),
		RestartPolicy:       tea.String("Never"),
		AutoMatchImageCache: tea.Bool(true),
		Container: []*eciclient.CreateContainerGroupRequestContainer{{
			Name:            tea.String(runnerContainerName),
			Image:           tea.String(spec.Image),
			ImagePullPolicy: tea.String("IfNotPresent"),
			Command:         []*string{tea.String(runnerEntrypoint)},
			EnvironmentVar:  environment(spec),
		}},
		Tag: tags(spec, c.cfg.Name),
	}
	if c.cfg.RAMRoleName != "" {
		request.RamRoleName = tea.String(c.cfg.RAMRoleName)
	}
	if spec.TTL > 0 {
		request.ActiveDeadlineSeconds = tea.Int64(int64(spec.TTL.Seconds()))
	}
	if spec.Spot {
		request.SpotStrategy = tea.String("SpotAsPriceGo")
	}

	response, err := c.api.CreateContainerGroup(request)
	if err != nil {
		return "", fmt.Errorf("eci %s: create %s: %w", c.cfg.Name, spec.Name, err)
	}
	if response.Body == nil || response.Body.ContainerGroupId == nil {
		return "", fmt.Errorf("eci %s: create %s returned no container group id", c.cfg.Name, spec.Name)
	}
	return *response.Body.ContainerGroupId, nil
}

// Describe lists the container groups matching sel, following pagination.
func (c *Client) Describe(ctx context.Context, sel Selector) ([]Instance, error) {
	var instances []Instance
	var nextToken *string
	for {
		response, err := c.api.DescribeContainerGroups(&eciclient.DescribeContainerGroupsRequest{
			Limit:     tea.Int32(100),
			NextToken: nextToken,
			Tag:       selectorTags(sel, c.cfg.Name),
		})
		if err != nil {
			return nil, fmt.Errorf("eci %s: describe: %w", c.cfg.Name, err)
		}
		if response.Body == nil {
			return instances, nil
		}

		for _, group := range response.Body.ContainerGroups {
			instances = append(instances, toInstance(group, c.cfg.Name))
		}

		// A repeated token would loop forever, so stop unless it advances.
		token := response.Body.NextToken
		if token == nil || *token == "" || (nextToken != nil && *token == *nextToken) {
			return instances, nil
		}
		nextToken = token
	}
}

// Terminate deletes one container group.
func (c *Client) Terminate(ctx context.Context, id string) error {
	_, err := c.api.DeleteContainerGroup(&eciclient.DeleteContainerGroupRequest{
		ContainerGroupId: tea.String(id),
		ClientToken:      tea.String(clientToken("delete:" + id)),
	})
	if err != nil {
		return fmt.Errorf("eci %s: delete %s: %w", c.cfg.Name, id, err)
	}
	return nil
}

// clientToken derives a stable token from name so that retrying the same launch
// is idempotent. ECI accepts at most 64 ASCII characters.
func clientToken(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:16])
}

func environment(spec Spec) []*eciclient.CreateContainerGroupRequestContainerEnvironmentVar {
	vars := make([]*eciclient.CreateContainerGroupRequestContainerEnvironmentVar, 0, len(spec.Env))
	for _, key := range slices.Sorted(maps.Keys(spec.Env)) {
		vars = append(vars, &eciclient.CreateContainerGroupRequestContainerEnvironmentVar{
			Key:   tea.String(key),
			Value: tea.String(spec.Env[key]),
		})
	}
	return vars
}

func tags(spec Spec, target string) []*eciclient.CreateContainerGroupRequestTag {
	list := []struct{ key, value string }{
		{tagManagedBy, managedByValue},
		{tagPool, spec.Pool},
		{tagRepo, spec.Repo},
		{tagJob, spec.JobID},
		{tagTarget, target},
	}
	out := make([]*eciclient.CreateContainerGroupRequestTag, 0, len(list))
	for _, entry := range list {
		if entry.value == "" {
			continue
		}
		out = append(out, &eciclient.CreateContainerGroupRequestTag{
			Key:   tea.String(entry.key),
			Value: tea.String(entry.value),
		})
	}
	return out
}

func selectorTags(sel Selector, target string) []*eciclient.DescribeContainerGroupsRequestTag {
	list := []struct{ key, value string }{
		{tagManagedBy, managedByValue},
		{tagPool, sel.Pool},
		{tagRepo, sel.Repo},
		{tagJob, sel.JobID},
		{tagTarget, target},
	}
	out := make([]*eciclient.DescribeContainerGroupsRequestTag, 0, len(list))
	for _, entry := range list {
		if entry.value == "" {
			continue
		}
		out = append(out, &eciclient.DescribeContainerGroupsRequestTag{
			Key:   tea.String(entry.key),
			Value: tea.String(entry.value),
		})
	}
	return out
}

func toInstance(group *eciclient.DescribeContainerGroupsResponseBodyContainerGroups, target string) Instance {
	instance := Instance{Target: target}
	if group == nil {
		return instance
	}
	if group.ContainerGroupId != nil {
		instance.ID = *group.ContainerGroupId
	}
	if group.ContainerGroupName != nil {
		instance.Name = *group.ContainerGroupName
	}
	if group.Status != nil {
		instance.Status = *group.Status
	}
	if group.CreationTime != nil {
		if created, err := time.Parse(time.RFC3339, *group.CreationTime); err == nil {
			instance.CreatedAt = created
		}
	}
	for _, tag := range group.Tags {
		if tag == nil || tag.Key == nil || tag.Value == nil {
			continue
		}
		switch *tag.Key {
		case tagPool:
			instance.Pool = *tag.Value
		case tagRepo:
			instance.Repo = *tag.Value
		case tagJob:
			instance.JobID = *tag.Value
		}
	}
	if instance.Pool == "" {
		instance.Pool = instance.Name
	}
	return instance
}

var _ Backend = (*Client)(nil)
