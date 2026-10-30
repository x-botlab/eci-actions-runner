// Package github holds the small slice of the GitHub API the scheduler needs:
// the queued jobs, the just-in-time runner configs and the runner registry.
package github

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	gh "github.com/google/go-github/v93/github"
)

// ErrNotImplemented marks behaviour that is still missing.
var ErrNotImplemented = errors.New("github: not implemented")

const (
	// defaultRunnerGroupID is the Default runner group of a repository.
	defaultRunnerGroupID int64 = 1
	// tokenRefreshMargin renews an installation token well before it expires so
	// that a reconcile never signs a request with a dead token.
	tokenRefreshMargin = 5 * time.Minute
	// maxRunsPerPass bounds the run listing: a repo with a long history must not
	// turn one reconcile into hundreds of API calls.
	maxRunsPerPass = 50
)

// queuedRunStatuses are the run states that can still hold a queued job. A
// matrix run reports in_progress while some of its jobs are still queued, so
// listing only queued runs would miss them.
var queuedRunStatuses = []string{"queued", "in_progress"}

// Repo identifies one repository together with the App installation that is
// allowed to mint tokens for it. A repository scoped runner only ever serves its
// own repository, so every call carries the repo and its installation.
type Repo struct {
	Owner          string
	Name           string
	InstallationID int64
}

// String returns owner/name.
func (r Repo) String() string { return r.Owner + "/" + r.Name }

// Config is the app-level identity. Repository scope arrives with each call, so
// one client serves every installation the App is installed on.
type Config struct {
	// ClientID is the OAuth client ID, which GitHub recommends as the JWT
	// issuer. It is a public identifier: the signature is what proves identity.
	ClientID       string
	PrivateKeyPath string
	// BaseURL overrides the API root, for tests and GitHub Enterprise.
	BaseURL string
}

// Job is a queued workflow job that still needs a runner.
type Job struct {
	ID        int64
	RunID     int64
	Status    string
	Labels    []string
	CreatedAt time.Time
}

// Runner is a registered self-hosted runner.
type Runner struct {
	ID     int64
	Name   string
	Status string
	Busy   bool
	Labels []string
}

// JITConfig is one-shot runner registration material. The encoded value is a
// credential: never log it, never store it.
type JITConfig struct {
	Encoded    string
	RunnerID   int64
	RunnerName string
}

// Token describes the installation token currently in use for one installation.
type Token struct {
	Expires     time.Time
	Permissions map[string]string
}

// Forge is everything the scheduler needs from GitHub. Every call is scoped to
// one repository because a repository scoped runner cannot serve another repo.
type Forge interface {
	QueuedJobs(ctx context.Context, repo Repo) ([]Job, error)
	CreateJITConfig(ctx context.Context, repo Repo, name string, labels []string) (JITConfig, error)
	Runners(ctx context.Context, repo Repo, namePrefix string) ([]Runner, error)
	RemoveRunner(ctx context.Context, repo Repo, id int64) error
}

// cachedToken is one installation token together with the client that uses it
// and the permissions GitHub reported for it.
type cachedToken struct {
	client      *gh.Client
	expires     time.Time
	permissions map[string]string
}

// AppClient authenticates as a GitHub App installation.
type AppClient struct {
	cfg Config
	key *rsa.PrivateKey
	now func() time.Time

	mu     sync.Mutex
	tokens map[int64]cachedToken
}

// NewAppClient loads the App private key and prepares the API client.
func NewAppClient(cfg Config) (*AppClient, error) {
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, errors.New("github: client id is required")
	}
	key, err := LoadPrivateKey(cfg.PrivateKeyPath)
	if err != nil {
		return nil, err
	}
	return &AppClient{
		cfg:    cfg,
		key:    key,
		now:    time.Now,
		tokens: make(map[int64]cachedToken),
	}, nil
}

// newClient builds an API client. A fresh client per token keeps the auth
// header immutable instead of mutating shared state between installations.
func (c *AppClient) newClient(token string) (*gh.Client, error) {
	opts := []gh.ClientOptionsFunc{
		gh.WithAuthToken(token),
		gh.WithTimeout(30 * time.Second),
	}
	if c.cfg.BaseURL != "" {
		// BaseURL is the exact API root, so a GitHub Enterprise deployment passes
		// https://ghe.example.com/api/v3/ here.
		base := c.cfg.BaseURL
		upload := c.cfg.BaseURL
		opts = append(opts, gh.WithURLs(&base, &upload))
	}
	client, err := gh.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("github: new client: %w", err)
	}
	return client, nil
}

// installationClient returns a client signed with an installation token,
// minting a new token only when the cached one is close to expiry.
func (c *AppClient) installationClient(ctx context.Context, installationID int64) (*gh.Client, error) {
	if installationID <= 0 {
		return nil, errors.New("github: installation id is required")
	}

	c.mu.Lock()
	cached, ok := c.tokens[installationID]
	c.mu.Unlock()
	if ok && c.now().Add(tokenRefreshMargin).Before(cached.expires) {
		return cached.client, nil
	}

	appJWT, err := c.AppJWT(c.now())
	if err != nil {
		return nil, err
	}
	appClient, err := c.newClient(appJWT)
	if err != nil {
		return nil, err
	}
	token, _, err := appClient.Apps.CreateInstallationToken(ctx, installationID, nil)
	if err != nil {
		return nil, fmt.Errorf("github: create installation token for %d: %w", installationID, err)
	}
	client, err := c.newClient(token.GetToken())
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.tokens[installationID] = cachedToken{
		client:      client,
		expires:     token.GetExpiresAt().Time,
		permissions: permissionMap(token.Permissions),
	}
	c.mu.Unlock()
	return client, nil
}

// Token returns the installation token currently in use, including the
// permissions GitHub granted. It is what a startup check reports: a missing
// permission then shows up before a reconcile fails on it.
func (c *AppClient) Token(ctx context.Context, installationID int64) (Token, error) {
	if _, err := c.installationClient(ctx, installationID); err != nil {
		return Token{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	cached := c.tokens[installationID]
	permissions := make(map[string]string, len(cached.permissions))
	maps.Copy(permissions, cached.permissions)
	return Token{Expires: cached.expires, Permissions: permissions}, nil
}

// permissionMap flattens the SDK permission struct, whose fields are all
// optional strings tagged with the permission name.
func permissionMap(permissions *gh.InstallationPermissions) map[string]string {
	out := make(map[string]string)
	if permissions == nil {
		return out
	}

	value := reflect.ValueOf(permissions).Elem()
	kind := value.Type()
	for i := range value.NumField() {
		field := value.Field(i)
		if field.Kind() != reflect.Pointer || field.IsNil() {
			continue
		}
		name, _, _ := strings.Cut(kind.Field(i).Tag.Get("json"), ",")
		if name == "" {
			continue
		}
		if text, ok := field.Elem().Interface().(string); ok {
			out[name] = text
		}
	}
	return out
}

// LoadPrivateKey reads a PEM encoded RSA key as generated by GitHub.
func LoadPrivateKey(path string) (*rsa.PrivateKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("github: read private key: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, fmt.Errorf("github: %s does not contain a PEM block", path)
	}
	if block.Type == "RSA PRIVATE KEY" {
		key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("github: parse PKCS1 key: %w", err)
		}
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github: parse PKCS8 key: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github: private key is not an RSA key")
	}
	return key, nil
}

// AppJWT returns a signed App JWT. GitHub rejects tokens valid for more than ten
// minutes, so the claim is kept at nine to absorb clock skew.
func (c *AppClient) AppJWT(now time.Time) (string, error) {
	claims := jwt.RegisteredClaims{
		Issuer:    strings.TrimSpace(c.cfg.ClientID),
		IssuedAt:  jwt.NewNumericDate(now.Add(-60 * time.Second)),
		ExpiresAt: jwt.NewNumericDate(now.Add(9 * time.Minute)),
	}
	token, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(c.key)
	if err != nil {
		return "", fmt.Errorf("github: sign app jwt: %w", err)
	}
	return token, nil
}

// QueuedJobs returns every job still waiting for a runner, across all of the
// repository's runs that can hold one. Label matching is deliberately left to
// the caller: one repo is polled once per pass and matched against every pool it
// binds, which halves the API calls.
func (c *AppClient) QueuedJobs(ctx context.Context, repo Repo) ([]Job, error) {
	client, err := c.installationClient(ctx, repo.InstallationID)
	if err != nil {
		return nil, err
	}

	var jobs []Job
	seen := make(map[int64]struct{})
	for _, status := range queuedRunStatuses {
		runs, _, err := client.Actions.ListRepositoryWorkflowRuns(ctx, repo.Owner, repo.Name,
			&gh.ListWorkflowRunsOptions{
				Status:      status,
				ListOptions: gh.ListOptions{PerPage: maxRunsPerPass},
			})
		if err != nil {
			return nil, fmt.Errorf("github: list %s runs for %s: %w", status, repo, err)
		}

		for _, run := range runs.WorkflowRuns {
			runJobs, _, err := client.Actions.ListWorkflowJobs(ctx, repo.Owner, repo.Name, run.GetID(),
				&gh.ListWorkflowJobsOptions{Filter: "latest"})
			if err != nil {
				return nil, fmt.Errorf("github: list jobs of run %d in %s: %w", run.GetID(), repo, err)
			}
			for _, job := range runJobs.Jobs {
				if job.GetStatus() != "queued" {
					continue
				}
				// Overlapping pages or a retried call must not count a job twice:
				// the count drives how many runners are created.
				if _, dup := seen[job.GetID()]; dup {
					continue
				}
				seen[job.GetID()] = struct{}{}
				jobs = append(jobs, Job{
					ID:        job.GetID(),
					RunID:     job.GetRunID(),
					Status:    job.GetStatus(),
					Labels:    job.Labels,
					CreatedAt: job.GetCreatedAt().Time,
				})
			}
		}
	}
	return jobs, nil
}

// CreateJITConfig mints a one-shot runner configuration for a single job.
func (c *AppClient) CreateJITConfig(ctx context.Context, repo Repo, name string, labels []string) (JITConfig, error) {
	client, err := c.installationClient(ctx, repo.InstallationID)
	if err != nil {
		return JITConfig{}, err
	}

	config, _, err := client.Actions.CreateRepoJITConfig(ctx, repo.Owner, repo.Name, gh.CreateJITConfigRequest{
		Name:          name,
		RunnerGroupID: defaultRunnerGroupID,
		Labels:        labels,
	})
	if err != nil {
		return JITConfig{}, fmt.Errorf("github: create jit config %q for %s: %w", name, repo, err)
	}
	if config.GetEncodedJITConfig() == "" {
		return JITConfig{}, fmt.Errorf("github: jit config for %s came back empty", repo)
	}
	return JITConfig{
		Encoded:    config.GetEncodedJITConfig(),
		RunnerID:   config.GetRunner().GetID(),
		RunnerName: config.GetRunner().GetName(),
	}, nil
}

// Runners lists registered runners whose name starts with namePrefix.
func (c *AppClient) Runners(ctx context.Context, repo Repo, namePrefix string) ([]Runner, error) {
	client, err := c.installationClient(ctx, repo.InstallationID)
	if err != nil {
		return nil, err
	}

	var runners []Runner
	opts := &gh.ListRunnersOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		page, resp, err := client.Actions.ListRunners(ctx, repo.Owner, repo.Name, opts)
		if err != nil {
			return nil, fmt.Errorf("github: list runners for %s: %w", repo, err)
		}
		for _, runner := range page.Runners {
			name := runner.GetName()
			if !strings.HasPrefix(name, namePrefix) {
				continue
			}
			labels := make([]string, 0, len(runner.Labels))
			for _, label := range runner.Labels {
				labels = append(labels, label.GetName())
			}
			runners = append(runners, Runner{
				ID:     runner.GetID(),
				Name:   name,
				Status: runner.GetStatus(),
				Busy:   runner.GetBusy(),
				Labels: labels,
			})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return runners, nil
}

// RemoveRunner unregisters a runner that never came online.
func (c *AppClient) RemoveRunner(ctx context.Context, repo Repo, id int64) error {
	client, err := c.installationClient(ctx, repo.InstallationID)
	if err != nil {
		return err
	}
	if _, err := client.Actions.RemoveRunner(ctx, repo.Owner, repo.Name, id); err != nil {
		return fmt.Errorf("github: remove runner %d from %s: %w", id, repo, err)
	}
	return nil
}

var _ Forge = (*AppClient)(nil)
