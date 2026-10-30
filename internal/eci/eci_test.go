package eci

import (
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

func TestNewRejectsIncompleteTarget(t *testing.T) {
	if _, err := New(Config{Name: "hangzhou"}); err == nil {
		t.Fatal("expected a region error")
	}
	if _, err := New(Config{Name: "hangzhou", RegionID: "cn-hangzhou"}); err == nil {
		t.Fatal("expected a vswitch error")
	}
}
