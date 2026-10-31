// Package eci wraps the Alibaba Cloud ECI OpenAPI behind a small interface so
// the scheduler can be exercised against a fake during tests. One Client is one
// ECI target: a region, a set of switches and one security group.
package eci

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/client"
	eciclient "github.com/alibabacloud-go/eci-20180808/v3/client"
	"github.com/alibabacloud-go/tea/tea"
	"github.com/aliyun/credentials-go/credentials"
)

// ErrNotImplemented marks behaviour that is still missing.
var ErrNotImplemented = errors.New("eci: not implemented")

// Registry is one container registry ECI may need to authenticate against. The
// credentials are read from the environment at launch time, so a diagnostic
// command still works when a registry password has rotated or is missing.
type Registry struct {
	Server      string
	UsernameEnv string
	PasswordEnv string
}

// Matches reports whether image is pulled from this registry. The trailing slash
// keeps "registry.example.com" from matching "registry.example.com.evil/...".
func (r Registry) Matches(image string) bool {
	return r.Server != "" && strings.HasPrefix(image, r.Server+"/")
}

// credentials reads the username and password for this registry.
func (r Registry) credentials() (username, password string, err error) {
	username = strings.TrimSpace(os.Getenv(r.UsernameEnv))
	if username == "" {
		return "", "", fmt.Errorf("environment variable %s is required for %s credentials", r.UsernameEnv, r.Server)
	}
	password = strings.TrimSpace(os.Getenv(r.PasswordEnv))
	if password == "" {
		return "", "", fmt.Errorf("environment variable %s is required for %s credentials", r.PasswordEnv, r.Server)
	}
	return username, password, nil
}

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
	// AutoCreateEIP gives every container group its own public IP. A runner needs
	// egress to reach GitHub, and a VPC without a NAT gateway has no other way out.
	AutoCreateEIP bool
	// EIPBandwidth is the public bandwidth in Mbit/s. ECI defaults to 5.
	EIPBandwidth int32
	// EIPISP selects the line type: BGP or BGP_PRO (premium). Empty keeps the
	// ECI default.
	EIPISP string
	// OSSBucket and OSSEndpoint point the runner at the object store that holds
	// image cache archives. They are only read when a pool names an image cache
	// and are handed to the container, which authenticates with RAMRoleName
	// through the metadata service instead of a key pair.
	OSSBucket   string
	OSSEndpoint string
	// Registries holds the credentials for private images. ECI picks the one
	// whose server matches the image, so unrelated registries are never sent.
	Registries []Registry
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
	Env map[string]string
	// Privileged creates the container privileged and tells the image to start a
	// docker daemon before the runner. ECI only allows that for whitelisted
	// accounts.
	Privileged bool
	// ImageCache names the archive the job hook restores before the daemon
	// starts. Empty leaves the store empty.
	ImageCache string
	TTL        time.Duration
	Spot       bool
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
	Target string
	// InternetIP is the public address, populated only when the target creates
	// an EIP for each container group.
	InternetIP string
	// Events explains a failure, for example why scheduling was refused.
	Events    []Event
	CreatedAt time.Time
}

// Event is one platform event attached to a container group.
type Event struct {
	Reason  string
	Message string
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
	// Validate reports configuration that is only discoverable at call time,
	// such as a registry credential that is not in the environment. It lets the
	// server fail at start-up instead of during the first launch.
	Validate() error
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

// Container group layout. The container starts through the image's own entry
// point, which reads the one-shot JIT config from the environment (so no
// registration token is ever baked in) and, in a privileged pool, starts the
// docker daemon before handing over to the runner.
const (
	runnerContainerName = "runner"
	runnerEntrypoint    = "/usr/local/bin/eci-entrypoint.sh"

	// EnvJITConfig is the variable the actions-runner image reads its one-shot
	// registration from.
	EnvJITConfig = "ACTIONS_RUNNER_INPUT_JITCONFIG"
	// EnvPrivileged tells the image that it was created privileged and has to
	// run a docker daemon.
	EnvPrivileged = "ECI_RUNNER_PRIVILEGED"
	// EnvImageCache names the archive family the job hook restores. The image
	// stays inert without it, so a pool opt in by setting image_cache.
	EnvImageCache = "ECI_RUNNER_IMAGE_CACHE"
	// EnvOSSRole / EnvOSSBucket / EnvOSSEndpoint describe the object store the
	// hook reads the archive from. The role is an instance role: the container
	// fetches its STS token from the metadata service, so no key pair is ever
	// put in the container spec.
	EnvOSSRole     = "ECI_OSS_ROLE"
	EnvOSSBucket   = "ECI_OSS_BUCKET"
	EnvOSSEndpoint = "ECI_OSS_ENDPOINT"
	// EnvOSSRegion is the bucket's region. ossutil needs it for signature v4
	// whenever the credentials are an STS token.
	EnvOSSRegion = "ECI_OSS_REGION"

	// The daemon needs a data root that is not the container's own filesystem:
	// ECI's root is overlayfs and overlayfs cannot stack on itself, so every
	// nested container fails with "mount ... invalid argument". An EmptyDir is
	// ext3 (30 GiB on a 8c16g instance), where overlayfs works.
	dindVolumeName = "docker-data"
	dindDataPath   = "/var/lib/docker"

	tagManagedBy = "managed-by"
	tagPool      = "pool"
	tagRepo      = "repo"
	tagJob       = "job-id"
	tagTarget    = "target"

	managedByValue = "eci-actions-runner"

	resourceTypeContainerGroup = "ContainerGroup"

	// maxDescribeIDs bounds one DescribeContainerGroups call.
	maxDescribeIDs = 20
	// maxTagPageSize is the largest page ListTagResources accepts.
	maxTagPageSize = 20
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
	request, err := c.launchRequest(spec, vswitch)
	if err != nil {
		return "", err
	}

	response, err := withRetry(ctx, "create container group", func() (*eciclient.CreateContainerGroupResponse, error) {
		return c.api.CreateContainerGroup(request)
	})
	if err != nil {
		return "", fmt.Errorf("eci %s: create %s: %w", c.cfg.Name, spec.Name, err)
	}
	if response.Body == nil || response.Body.ContainerGroupId == nil {
		return "", fmt.Errorf("eci %s: create %s returned no container group id", c.cfg.Name, spec.Name)
	}
	return *response.Body.ContainerGroupId, nil
}

// launchRequest builds the create request. It is separate from the call so the
// request can be asserted without a network round trip: RegionId in particular
// has to be in the request body, not only in the client config, or the API
// answers MissingRegionId.
func (c *Client) launchRequest(spec Spec, vswitch string) (*eciclient.CreateContainerGroupRequest, error) {
	if spec.Name == "" {
		return nil, errors.New("eci: container group name is required")
	}
	if spec.Image == "" {
		return nil, errors.New("eci: image is required")
	}

	creds, err := c.registryCredentials(spec.Image)
	if err != nil {
		return nil, err
	}

	env := maps.Clone(spec.Env)
	if spec.Privileged {
		if env == nil {
			env = make(map[string]string, 4)
		}
		// The image only runs dockerd when the container was created privileged,
		// so the flag travels with the launch instead of the image.
		env[EnvPrivileged] = "1"
	}
	if spec.ImageCache != "" && c.cfg.OSSBucket != "" {
		if env == nil {
			env = make(map[string]string, 4)
		}
		env[EnvImageCache] = spec.ImageCache
		env[EnvOSSRole] = c.cfg.RAMRoleName
		env[EnvOSSBucket] = c.cfg.OSSBucket
		env[EnvOSSEndpoint] = c.cfg.OSSEndpoint
		env[EnvOSSRegion] = c.cfg.RegionID
	}

	container := &eciclient.CreateContainerGroupRequestContainer{
		Name:            tea.String(runnerContainerName),
		Image:           tea.String(spec.Image),
		ImagePullPolicy: tea.String("IfNotPresent"),
		Command:         []*string{tea.String(runnerEntrypoint)},
		EnvironmentVar:  environment(env),
	}
	var volumes []*eciclient.CreateContainerGroupRequestVolume
	if spec.Privileged {
		// A privileged container is the only place dockerd can run: ECI offers no
		// docker socket to mount, and the daemon has to share the container's
		// lifecycle or the group never reaches a terminal state.
		container.SecurityContextPrivileged = tea.Bool(true)
		container.VolumeMount = []*eciclient.CreateContainerGroupRequestContainerVolumeMount{{
			Name:      tea.String(dindVolumeName),
			MountPath: tea.String(dindDataPath),
		}}
		volumes = append(volumes, &eciclient.CreateContainerGroupRequestVolume{
			Name:           tea.String(dindVolumeName),
			Type:           tea.String("EmptyDirVolume"),
			EmptyDirVolume: &eciclient.CreateContainerGroupRequestVolumeEmptyDirVolume{},
		})
	}

	request := &eciclient.CreateContainerGroupRequest{
		RegionId: tea.String(c.cfg.RegionID),
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
		Container:           []*eciclient.CreateContainerGroupRequestContainer{container},
		Volume:              volumes,
		Tag:                 tags(spec, c.cfg.Name),
	}
	if c.cfg.RAMRoleName != "" {
		request.RamRoleName = tea.String(c.cfg.RAMRoleName)
	}
	if len(creds) > 0 {
		request.ImageRegistryCredential = creds
	}
	if spec.TTL > 0 {
		request.ActiveDeadlineSeconds = tea.Int64(int64(spec.TTL.Seconds()))
	}
	if spec.Spot {
		request.SpotStrategy = tea.String("SpotAsPriceGo")
	}
	if c.cfg.AutoCreateEIP {
		request.AutoCreateEip = tea.Bool(true)
		if c.cfg.EIPBandwidth > 0 {
			request.EipBandwidth = tea.Int32(c.cfg.EIPBandwidth)
		}
		if c.cfg.EIPISP != "" {
			request.EipISP = tea.String(c.cfg.EIPISP)
		}
	}
	return request, nil
}

// Describe lists the container groups matching sel. ECI has no "list by tag"
// call: DescribeContainerGroups insists on explicit ids, so the ids come from
// ListTagResources first. Both steps are needed, and both are read-only.
func (c *Client) Describe(ctx context.Context, sel Selector) ([]Instance, error) {
	ids, err := c.listTagged(ctx, sel)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	var instances []Instance
	// The describe call accepts a bounded id list per request.
	for chunk := range slices.Chunk(ids, maxDescribeIDs) {
		groups, err := c.describeByIDs(ctx, chunk)
		if err != nil {
			return nil, err
		}
		instances = append(instances, groups...)
	}
	return instances, nil
}

// listTagged returns the ids of the container groups carrying our tags. The
// managed-by tag is always applied, so a container group this tool did not create
// is never adopted.
func (c *Client) listTagged(ctx context.Context, sel Selector) ([]string, error) {
	var ids []string
	seen := make(map[string]struct{})
	var nextToken *string

	for {
		response, err := withRetry(ctx, "list tagged resources", func() (*eciclient.ListTagResourcesResponse, error) {
			return c.api.ListTagResources(&eciclient.ListTagResourcesRequest{
				RegionId:     tea.String(c.cfg.RegionID),
				ResourceType: tea.String(resourceTypeContainerGroup),
				Tag:          listTagFilter(sel, c.cfg.Name),
				Limit:        tea.String(strconv.Itoa(maxTagPageSize)),
				NextToken:    nextToken,
			})
		})
		if err != nil {
			return nil, fmt.Errorf("eci %s: list tagged resources: %w", c.cfg.Name, err)
		}
		if response.Body == nil {
			return ids, nil
		}

		for _, resource := range response.Body.TagResources {
			if resource == nil || resource.ResourceId == nil {
				continue
			}
			if _, dup := seen[*resource.ResourceId]; dup {
				continue
			}
			seen[*resource.ResourceId] = struct{}{}
			ids = append(ids, *resource.ResourceId)
		}

		// A repeated token would loop forever, so stop unless it advances.
		token := response.Body.NextToken
		if token == nil || *token == "" || (nextToken != nil && *token == *nextToken) {
			return ids, nil
		}
		nextToken = token
	}
}

// describeByIDs fetches the detail for one bounded batch of ids.
func (c *Client) describeByIDs(ctx context.Context, ids []string) ([]Instance, error) {
	encoded, err := encodeContainerGroupIDs(ids)
	if err != nil {
		return nil, fmt.Errorf("eci %s: describe: %w", c.cfg.Name, err)
	}
	response, err := withRetry(ctx, "describe container groups", func() (*eciclient.DescribeContainerGroupsResponse, error) {
		return c.api.DescribeContainerGroups(&eciclient.DescribeContainerGroupsRequest{
			RegionId:          tea.String(c.cfg.RegionID),
			ContainerGroupIds: tea.String(encoded),
			Limit:             tea.Int32(int32(maxDescribeIDs)),
		})
	})
	if err != nil {
		return nil, fmt.Errorf("eci %s: describe: %w", c.cfg.Name, err)
	}
	if response.Body == nil {
		return nil, nil
	}

	instances := make([]Instance, 0, len(response.Body.ContainerGroups))
	for _, group := range response.Body.ContainerGroups {
		instances = append(instances, toInstance(group, c.cfg.Name))
	}
	return instances, nil
}

// encodeContainerGroupIDs renders the id list the way ECI expects it: a JSON
// array string. A comma separated list is rejected (InvalidParameter, "JSON
// string format is required here") as soon as it holds more than one id.
func encodeContainerGroupIDs(ids []string) (string, error) {
	if len(ids) == 0 {
		return "", errors.New("no container group ids")
	}
	encoded, err := json.Marshal(ids)
	if err != nil {
		return "", fmt.Errorf("encode container group ids: %w", err)
	}
	return string(encoded), nil
}

// Terminate deletes one container group.
func (c *Client) Terminate(ctx context.Context, id string) error {
	_, err := withRetry(ctx, "delete container group", func() (*eciclient.DeleteContainerGroupResponse, error) {
		return c.api.DeleteContainerGroup(&eciclient.DeleteContainerGroupRequest{
			RegionId:         tea.String(c.cfg.RegionID),
			ContainerGroupId: tea.String(id),
			ClientToken:      tea.String(clientToken("delete:" + id)),
		})
	})
	if err != nil {
		return fmt.Errorf("eci %s: delete %s: %w", c.cfg.Name, id, err)
	}
	return nil
}

// registryCredentials returns the credentials for the registry that hosts image.
// A public image matches nothing and is pulled anonymously.
func (c *Client) registryCredentials(image string) ([]*eciclient.CreateContainerGroupRequestImageRegistryCredential, error) {
	var creds []*eciclient.CreateContainerGroupRequestImageRegistryCredential
	for _, registry := range c.cfg.Registries {
		if !registry.Matches(image) {
			continue
		}
		username, password, err := registry.credentials()
		if err != nil {
			return nil, err
		}
		creds = append(creds, &eciclient.CreateContainerGroupRequestImageRegistryCredential{
			Server:   tea.String(registry.Server),
			UserName: tea.String(username),
			Password: tea.String(password),
		})
	}
	return creds, nil
}

// Validate checks that every configured registry has credentials in the
// environment, so a missing variable fails at start-up rather than mid-launch.
func (c *Client) Validate() error {
	for _, registry := range c.cfg.Registries {
		if _, _, err := registry.credentials(); err != nil {
			return fmt.Errorf("eci %q: %w", c.cfg.Name, err)
		}
	}
	return nil
}

// retryable reports whether an error is worth another attempt: a transport drop
// (EOF, reset, timeout) or a server-side failure. Client errors are not retried,
// because a bad parameter fails the same way every time.
func retryable(err error) bool {
	if err == nil {
		return false
	}

	var sdkErr *tea.SDKError
	if errors.As(err, &sdkErr) {
		if sdkErr.StatusCode == nil {
			return false
		}
		return *sdkErr.StatusCode >= 500 || *sdkErr.StatusCode == 429
	}

	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		strings.Contains(err.Error(), "EOF") ||
		strings.Contains(err.Error(), "connection reset")
}

// withRetry runs fn until it succeeds, the error is not retryable, or attempts run
// out. Every call it wraps is either a read or carries a client token, so a repeat
// is safe.
func withRetry[T any](ctx context.Context, op string, fn func() (T, error)) (T, error) {
	var zero T

	const attempts = 4
	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			delay := time.Duration(200<<attempt) * time.Millisecond
			select {
			case <-ctx.Done():
				return zero, ctx.Err()
			case <-time.After(delay):
			}
		}

		value, err := fn()
		if err == nil {
			return value, nil
		}
		lastErr = err
		if !retryable(err) {
			return zero, err
		}
	}
	return zero, fmt.Errorf("%s: %w (gave up after %d attempts)", op, lastErr, attempts)
}

// clientToken derives a stable token from name so that retrying the same launch
// is idempotent. ECI accepts at most 64 ASCII characters.
func clientToken(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:16])
}

func environment(env map[string]string) []*eciclient.CreateContainerGroupRequestContainerEnvironmentVar {
	vars := make([]*eciclient.CreateContainerGroupRequestContainerEnvironmentVar, 0, len(env))
	for _, key := range slices.Sorted(maps.Keys(env)) {
		vars = append(vars, &eciclient.CreateContainerGroupRequestContainerEnvironmentVar{
			Key:   tea.String(key),
			Value: tea.String(env[key]),
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

// selectorTags is the tag filter for DescribeContainerGroups.
func selectorTags(sel Selector, target string) []*eciclient.DescribeContainerGroupsRequestTag {
	out := make([]*eciclient.DescribeContainerGroupsRequestTag, 0, 5)
	for _, entry := range selectorEntries(sel, target) {
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

// listTagFilter is the same filter for ListTagResources.
func listTagFilter(sel Selector, target string) []*eciclient.ListTagResourcesRequestTag {
	out := make([]*eciclient.ListTagResourcesRequestTag, 0, 5)
	for _, entry := range selectorEntries(sel, target) {
		if entry.value == "" {
			continue
		}
		out = append(out, &eciclient.ListTagResourcesRequestTag{
			Key:   tea.String(entry.key),
			Value: tea.String(entry.value),
		})
	}
	return out
}

// selectorEntries is the one definition of "which container groups are ours".
func selectorEntries(sel Selector, target string) []struct{ key, value string } {
	return []struct{ key, value string }{
		{tagManagedBy, managedByValue},
		{tagPool, sel.Pool},
		{tagRepo, sel.Repo},
		{tagJob, sel.JobID},
		{tagTarget, target},
	}
}

func valueOr(value *string, fallback string) string {
	if value == nil {
		return fallback
	}
	return *value
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
	if group.InternetIp != nil {
		instance.InternetIP = *group.InternetIp
	}
	for _, event := range group.Events {
		if event == nil {
			continue
		}
		instance.Events = append(instance.Events, Event{
			Reason:  valueOr(event.Reason, ""),
			Message: valueOr(event.Message, ""),
		})
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
