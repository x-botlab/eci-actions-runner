package eci

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	eciclient "github.com/alibabacloud-go/eci-20180808/v3/client"
	"github.com/alibabacloud-go/tea/tea"
)

func TestClientTokenIsStableAndShort(t *testing.T) {
	first := clientToken("eci-pool-short-abc")
	if first != clientToken("eci-pool-short-abc") {
		t.Fatal("expected the same name to produce the same token")
	}
	if first == clientToken("eci-pool-short-abd") {
		t.Fatal("expected different names to produce different tokens")
	}
	if len(first) > 64 {
		t.Fatalf("token length %d exceeds the 64 character limit", len(first))
	}
}

// DescribeContainerGroups takes ContainerGroupIds as a JSON array string. A comma
// separated list is rejected once it holds two or more ids, which silently stops
// the whole reconcile loop, so the encoding is pinned here.
func TestContainerGroupIDEncodingIsJSONArray(t *testing.T) {
	got, err := encodeContainerGroupIDs([]string{"eci-1", "eci-2"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if want := `["eci-1","eci-2"]`; got != want {
		t.Fatalf("encoded ids = %s, want %s", got, want)
	}

	if _, err := encodeContainerGroupIDs(nil); err == nil {
		t.Fatal("expected an error for an empty id list")
	}
}

func TestTagsSkipEmptyValues(t *testing.T) {
	tags := tags(Spec{Pool: "pool-short", Repo: "o/r"}, "hangzhou")

	got := map[string]string{}
	for _, tag := range tags {
		got[*tag.Key] = *tag.Value
	}
	if got[tagManagedBy] != managedByValue {
		t.Fatalf("managed-by = %q", got[tagManagedBy])
	}
	if got[tagPool] != "pool-short" || got[tagRepo] != "o/r" || got[tagTarget] != "hangzhou" {
		t.Fatalf("tags = %v", got)
	}
	if _, ok := got[tagJob]; ok {
		t.Fatal("an empty job id must not be tagged")
	}
}

func TestSelectorTagsAlwaysScopeToManaged(t *testing.T) {
	tags := selectorTags(Selector{Pool: "pool-short", Repo: "o/r"}, "hangzhou")

	got := map[string]string{}
	for _, tag := range tags {
		got[*tag.Key] = *tag.Value
	}
	// Without this the scanner could adopt container groups it did not create.
	if got[tagManagedBy] != managedByValue {
		t.Fatalf("managed-by missing from the selector: %v", got)
	}
	if len(got) != 4 {
		t.Fatalf("selector tags = %v, want the four configured ones", got)
	}
}

func TestToInstanceReadsTagsAndTime(t *testing.T) {
	group := &eciclient.DescribeContainerGroupsResponseBodyContainerGroups{
		ContainerGroupId:   tea.String("eci-123"),
		ContainerGroupName: tea.String("eci-pool-short-abc"),
		Status:             tea.String("Running"),
		CreationTime:       tea.String("2026-01-02T03:04:05Z"),
		Tags: []*eciclient.DescribeContainerGroupsResponseBodyContainerGroupsTags{
			{Key: tea.String(tagPool), Value: tea.String("pool-short")},
			{Key: tea.String(tagRepo), Value: tea.String("o/r")},
			{Key: tea.String(tagJob), Value: tea.String("99")},
		},
	}

	instance := toInstance(group, "hangzhou")
	if instance.ID != "eci-123" || instance.Name != "eci-pool-short-abc" || instance.Status != "Running" {
		t.Fatalf("instance = %+v", instance)
	}
	if instance.Pool != "pool-short" || instance.Repo != "o/r" || instance.JobID != "99" {
		t.Fatalf("tags lost: %+v", instance)
	}
	if instance.Target != "hangzhou" {
		t.Fatalf("target = %q", instance.Target)
	}
	if !instance.CreatedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("created at = %s", instance.CreatedAt)
	}
}

func TestInstanceStateClassification(t *testing.T) {
	tests := []struct {
		status   string
		active   bool
		terminal bool
	}{
		{status: "Pending", active: true},
		{status: "Running", active: true},
		{status: "Succeeded", terminal: true},
		{status: "Failed", terminal: true},
		{status: "Stopping"}, // neither: cleanup waits for the age backstop
	}

	for _, tt := range tests {
		instance := Instance{Status: tt.status}
		if instance.Active() != tt.active {
			t.Fatalf("%s: Active = %t, want %t", tt.status, instance.Active(), tt.active)
		}
		if instance.Terminal() != tt.terminal {
			t.Fatalf("%s: Terminal = %t, want %t", tt.status, instance.Terminal(), tt.terminal)
		}
	}
}

func TestRegistryMatchesImage(t *testing.T) {
	registry := Registry{Server: "registry.cn-hangzhou.aliyuncs.com"}

	if !registry.Matches("registry.cn-hangzhou.aliyuncs.com/xbotlab/actions-runner:2.338.2") {
		t.Fatal("expected an image from the registry to match")
	}
	if registry.Matches("docker.io/library/ubuntu:24.04") {
		t.Fatal("a public image must not match a private registry")
	}
	if registry.Matches("registry.cn-hangzhou.aliyuncs.com.evil/x/runner:1") {
		t.Fatal("a look-alike host must not match")
	}
	if (Registry{}).Matches("anything") {
		t.Fatal("an empty server must never match")
	}
}

func TestRegistryCredentialsOnlyForMatchingImages(t *testing.T) {
	t.Setenv("TEST_ACR_USER", "u")
	t.Setenv("TEST_ACR_PASS", "p")

	client := &Client{cfg: Config{
		Name: "hangzhou",
		Registries: []Registry{
			{Server: "registry.cn-hangzhou.aliyuncs.com", UsernameEnv: "TEST_ACR_USER", PasswordEnv: "TEST_ACR_PASS"},
			{Server: "registry.cn-beijing.aliyuncs.com", UsernameEnv: "TEST_MISSING_USER", PasswordEnv: "TEST_MISSING_PASS"},
		},
	}}

	creds, err := client.registryCredentials("registry.cn-hangzhou.aliyuncs.com/xbotlab/actions-runner:2.338.2")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if len(creds) != 1 {
		t.Fatalf("got %d credentials, want 1", len(creds))
	}
	if *creds[0].Server != "registry.cn-hangzhou.aliyuncs.com" {
		t.Fatalf("server = %q", *creds[0].Server)
	}
	if *creds[0].UserName != "u" || *creds[0].Password != "p" {
		t.Fatalf("credential = %q/%q", *creds[0].UserName, *creds[0].Password)
	}

	// Matching the beijing registry exposes the missing variable, and only then.
	if _, err := client.registryCredentials("registry.cn-beijing.aliyuncs.com/x/y:1"); err == nil {
		t.Fatal("expected a missing environment variable error")
	}

	if creds, err := client.registryCredentials("docker.io/library/ubuntu:24.04"); err != nil || len(creds) != 0 {
		t.Fatalf("a public image must not receive credentials: %d, %v", len(creds), err)
	}
}

func TestValidateReportsMissingCredential(t *testing.T) {
	ok := &Client{cfg: Config{
		Name:       "hangzhou",
		Registries: []Registry{{Server: "a.example.com", UsernameEnv: "TEST_ACR_USER", PasswordEnv: "TEST_ACR_PASS"}},
	}}
	t.Setenv("TEST_ACR_USER", "u")
	t.Setenv("TEST_ACR_PASS", "p")
	if err := ok.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	missing := &Client{cfg: Config{
		Name:       "hangzhou",
		Registries: []Registry{{Server: "a.example.com", UsernameEnv: "TEST_ABSENT_USER", PasswordEnv: "TEST_ABSENT_PASS"}},
	}}
	if err := missing.Validate(); err == nil {
		t.Fatal("expected validate to fail when the credential is absent")
	}
}

// Regression: RegionId belongs in the request body. Sending it only on the client
// config makes the API answer MissingRegionId.
func TestLaunchRequestCarriesRegionAndShape(t *testing.T) {
	t.Setenv("TEST_ACR_USER", "u")
	t.Setenv("TEST_ACR_PASS", "p")

	client := &Client{cfg: Config{
		Name:            "hongkong",
		RegionID:        "cn-hongkong",
		VSwitchIDs:      []string{"vsw-1"},
		SecurityGroupID: "sg-1",
		RAMRoleName:     "eci-runner-self-delete",
		Registries: []Registry{{
			Server: "registry.cn-hangzhou.aliyuncs.com", UsernameEnv: "TEST_ACR_USER", PasswordEnv: "TEST_ACR_PASS",
		}},
	}}

	spec := Spec{
		Name:      "eci-pool-short-abc",
		Pool:      "pool-short",
		Repo:      "o/r",
		JobID:     "42",
		VCPU:      4,
		MemoryGiB: 8,
		Image:     "registry.cn-hangzhou.aliyuncs.com/xbotlab/actions-runner:2.338.2",
		Env:       map[string]string{EnvJITConfig: "encoded"},
		TTL:       90 * time.Minute,
	}

	request, err := client.launchRequest(spec, "vsw-1")
	if err != nil {
		t.Fatalf("launch request: %v", err)
	}

	if got := *request.RegionId; got != "cn-hongkong" {
		t.Fatalf("RegionId = %q, want cn-hongkong", got)
	}
	if got := *request.VSwitchId; got != "vsw-1" {
		t.Fatalf("VSwitchId = %q", got)
	}
	if got := *request.SecurityGroupId; got != "sg-1" {
		t.Fatalf("SecurityGroupId = %q", got)
	}
	if *request.Cpu != 4 || *request.Memory != 8 {
		t.Fatalf("shape = %v/%v", *request.Cpu, *request.Memory)
	}
	if got := *request.RestartPolicy; got != "Never" {
		t.Fatalf("RestartPolicy = %q", got)
	}
	// Image cache matching keeps the cold start short once the image is cached.
	if request.AutoMatchImageCache == nil || !*request.AutoMatchImageCache {
		t.Fatal("AutoMatchImageCache must be true")
	}
	if got := *request.ActiveDeadlineSeconds; got != 5400 {
		t.Fatalf("ActiveDeadlineSeconds = %d", got)
	}
	if len(request.ImageRegistryCredential) != 1 {
		t.Fatalf("credentials = %d, want 1", len(request.ImageRegistryCredential))
	}
	if len(request.Container) != 1 {
		t.Fatalf("containers = %d, want 1", len(request.Container))
	}
	container := request.Container[0]
	if *container.Image != spec.Image || *container.Command[0] != runnerEntrypoint {
		t.Fatalf("container = %q cmd %q", *container.Image, *container.Command[0])
	}
	if len(container.EnvironmentVar) != 1 || *container.EnvironmentVar[0].Key != EnvJITConfig {
		t.Fatalf("env = %+v", container.EnvironmentVar)
	}
}

func TestRetryableClassification(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "eof", err: io.EOF, want: true},
		{name: "unexpected eof", err: io.ErrUnexpectedEOF, want: true},
		{name: "wrapped eof text", err: errors.New("Post url: EOF"), want: true},
		{name: "connection reset", err: errors.New("read: connection reset by peer"), want: true},
		{name: "bad parameter", err: &tea.SDKError{StatusCode: tea.Int(400)}, want: false},
		{name: "forbidden", err: &tea.SDKError{StatusCode: tea.Int(403)}, want: false},
		{name: "server error", err: &tea.SDKError{StatusCode: tea.Int(503)}, want: true},
		{name: "throttled", err: &tea.SDKError{StatusCode: tea.Int(429)}, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := retryable(tt.err); got != tt.want {
				t.Fatalf("retryable = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestWithRetryStopsOnNonRetryable(t *testing.T) {
	calls := 0
	_, err := withRetry(context.Background(), "op", func() (string, error) {
		calls++
		return "", &tea.SDKError{StatusCode: tea.Int(400)}
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1: a client error must not be retried", calls)
	}
}

func TestWithRetryRecoversFromTransientError(t *testing.T) {
	calls := 0
	value, err := withRetry(context.Background(), "op", func() (string, error) {
		calls++
		if calls < 3 {
			return "", io.EOF
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if value != "ok" || calls != 3 {
		t.Fatalf("value %q after %d calls", value, calls)
	}
}

func TestLaunchRequestSetsEIPWhenTargetAsks(t *testing.T) {
	client := &Client{cfg: Config{RegionID: "cn-hongkong", AutoCreateEIP: true, EIPBandwidth: 200, EIPISP: "BGP_PRO"}}
	spec := Spec{Name: "n", Image: "docker.io/library/ubuntu:24.04"}

	request, err := client.launchRequest(spec, "vsw-1")
	if err != nil {
		t.Fatalf("launch request: %v", err)
	}
	if request.AutoCreateEip == nil || !*request.AutoCreateEip {
		t.Fatal("expected AutoCreateEip to be set")
	}
	if request.EipBandwidth == nil || *request.EipBandwidth != 200 {
		t.Fatalf("EipBandwidth = %v, want 200", request.EipBandwidth)
	}
	if request.EipISP == nil || *request.EipISP != "BGP_PRO" {
		t.Fatalf("EipISP = %v, want BGP_PRO", request.EipISP)
	}

	plain := &Client{cfg: Config{RegionID: "cn-hongkong"}}
	request, err = plain.launchRequest(spec, "vsw-1")
	if err != nil {
		t.Fatalf("launch request: %v", err)
	}
	if request.AutoCreateEip != nil || request.EipISP != nil {
		t.Fatal("a target without auto_create_eip must not request a public IP")
	}
}

func TestLaunchRequestRejectsIncompleteSpec(t *testing.T) {
	client := &Client{cfg: Config{RegionID: "cn-hongkong"}}

	if _, err := client.launchRequest(Spec{Image: "x"}, "vsw-1"); err == nil {
		t.Fatal("expected a missing name error")
	}
	if _, err := client.launchRequest(Spec{Name: "x"}, "vsw-1"); err == nil {
		t.Fatal("expected a missing image error")
	}
}

func TestNewRejectsIncompleteTarget(t *testing.T) {
	if _, err := New(Config{Name: "hangzhou"}); err == nil {
		t.Fatal("expected a region error")
	}
	if _, err := New(Config{Name: "hangzhou", RegionID: "cn-hangzhou"}); err == nil {
		t.Fatal("expected a vswitch error")
	}
}
