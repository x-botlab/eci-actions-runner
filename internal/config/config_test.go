package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pools.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const minimal = `
github:
  app_id: 1
  private_key_path: /tmp/app.pem
  installations:
    - id: 2
      repos:
        - owner: o
          name: r
eci:
  targets:
    - name: t
      region_id: cn-hangzhou
      vswitch_ids: [vsw-1]
      security_group_id: sg-1
pools:
  - name: p
    eci: t
    vcpu: 2
    memory_gib: 4
    max: 1
    labels: [eci-p]
    image: reg/runner:v1
    ttl_minutes: 380
`

func TestLoadRepositoryConfig(t *testing.T) {
	cfg, err := Load(filepath.Join("..", "..", "pools.yaml"))
	if err != nil {
		t.Fatalf("load shipped pools.yaml: %v", err)
	}
	if len(cfg.Pools) != 2 {
		t.Fatalf("got %d pools, want 2", len(cfg.Pools))
	}
	if cfg.Runner.PollIntervalSeconds != 30 {
		t.Fatalf("default poll interval not applied: %d", cfg.Runner.PollIntervalSeconds)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimal))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Runner.WorkFolder != "_work" {
		t.Fatalf("work folder default = %q, want _work", cfg.Runner.WorkFolder)
	}
	if cfg.Runner.MaxConcurrentLaunches != 5 {
		t.Fatalf("max concurrent launches default = %d, want 5", cfg.Runner.MaxConcurrentLaunches)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	body := strings.Replace(minimal, "ttl_minutes: 380", "ttl_minute: 380", 1)
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("expected a typo in a key to fail the load")
	}
}

func TestValidateRejectsDuplicateLabel(t *testing.T) {
	body := minimal + `
  - name: q
    eci: t
    vcpu: 4
    memory_gib: 8
    max: 1
    labels: [eci-p]
    image: reg/runner:v1
    ttl_minutes: 380
`
	_, err := Load(writeConfig(t, body))
	if err == nil || !strings.Contains(err.Error(), "eci-p") {
		t.Fatalf("expected a duplicate label error, got %v", err)
	}
}

func TestValidateRequiresPool(t *testing.T) {
	body := minimal[:strings.Index(minimal, "pools:")]
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("expected a config without pools to fail")
	}
}

func TestPoolLookup(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimal))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, ok := cfg.Pool("p"); !ok {
		t.Fatal("expected pool p to exist")
	}
	if _, ok := cfg.Pool("missing"); ok {
		t.Fatal("did not expect pool missing to exist")
	}
}

const multi = `
github:
  app_id: 1
  private_key_path: /tmp/app.pem
  installations:
    - id: 2
      repos:
        - owner: o
          name: r
          pools: [p1]
        - owner: o
          name: r2
    - id: 3
      repos:
        - owner: o2
          name: r3
          pools: [p2]
eci:
  targets:
    - name: t1
      region_id: cn-hangzhou
      vswitch_ids: [vsw-1]
      security_group_id: sg-1
    - name: t2
      region_id: cn-beijing
      vswitch_ids: [vsw-2]
      security_group_id: sg-2
pools:
  - name: p1
    eci: t1
    vcpu: 2
    memory_gib: 4
    max: 1
    labels: [eci-p1]
    image: reg/runner:v1
    ttl_minutes: 380
  - name: p2
    eci: t2
    vcpu: 8
    memory_gib: 16
    max: 1
    labels: [eci-p2]
    image: reg/runner:v1
    ttl_minutes: 380
`

func TestBindings(t *testing.T) {
	cfg, err := Load(writeConfig(t, multi))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	got := make(map[string][]string)
	for _, b := range cfg.Bindings() {
		key := b.Repo.String()
		if b.Repo.InstallationID == 0 {
			t.Fatalf("%s: binding without an installation id", key)
		}
		if b.Target.Name != b.Pool.ECI {
			t.Fatalf("%s/%s: target %q does not match the pool's eci %q",
				key, b.Pool.Name, b.Target.Name, b.Pool.ECI)
		}
		got[key] = append(got[key], b.Pool.Name+"@"+b.Target.Name)
	}

	want := map[string][]string{
		"o/r":   {"p1@t1"},
		"o/r2":  {"p1@t1", "p2@t2"},
		"o2/r3": {"p2@t2"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d repos with bindings, want %d", len(got), len(want))
	}
	for repo, wantPools := range want {
		if !slices.Equal(got[repo], wantPools) {
			t.Fatalf("%s: bindings %v, want %v", repo, got[repo], wantPools)
		}
	}
}

func TestBindingsCarryInstallation(t *testing.T) {
	cfg, err := Load(writeConfig(t, multi))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, b := range cfg.Bindings() {
		switch b.Repo.String() {
		case "o/r", "o/r2":
			if b.Repo.InstallationID != 2 {
				t.Fatalf("%s: installation %d, want 2", b.Repo, b.Repo.InstallationID)
			}
		case "o2/r3":
			if b.Repo.InstallationID != 3 {
				t.Fatalf("%s: installation %d, want 3", b.Repo, b.Repo.InstallationID)
			}
		}
	}
}

func TestValidateRejectsUnknownTarget(t *testing.T) {
	body := strings.Replace(minimal, "eci: t", "eci: missing", 1)
	if _, err := Load(writeConfig(t, body)); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("expected an unknown eci target error, got %v", err)
	}
}

func TestValidateRejectsUnknownPoolInRepo(t *testing.T) {
	body := strings.Replace(multi, "pools: [p1]", "pools: [nope]", 1)
	if _, err := Load(writeConfig(t, body)); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("expected an unknown pool error, got %v", err)
	}
}

func TestValidateRejectsRepoInTwoInstallations(t *testing.T) {
	body := strings.Replace(multi, "owner: o2\n          name: r3", "owner: o\n          name: r", 1)
	if _, err := Load(writeConfig(t, body)); err == nil || !strings.Contains(err.Error(), "listed by installation") {
		t.Fatalf("expected a duplicate repo error, got %v", err)
	}
}

func TestValidateRejectsDuplicateInstallation(t *testing.T) {
	body := strings.Replace(multi, "    - id: 3", "    - id: 2", 1)
	if _, err := Load(writeConfig(t, body)); err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Fatalf("expected a duplicate installation error, got %v", err)
	}
}

// The same label may name a different shape in a different repo, because
// runners are per-repository; it only has to be unique inside one repo.
func TestValidateAllowsSameLabelInUnrelatedRepos(t *testing.T) {
	body := strings.Replace(multi, "          name: r2\n", "          name: r2\n          pools: [p1]\n", 1)
	body = strings.Replace(body, "labels: [eci-p2]", "labels: [eci-p1]", 1)

	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("expected two pools with the same label in unrelated repos to load: %v", err)
	}
	for _, b := range cfg.Bindings() {
		if b.Repo.String() == "o/r2" && len(b.Pool.Labels) != 1 {
			t.Fatalf("o/r2 unexpectedly bound to pool %q", b.Pool.Name)
		}
	}
}

// Inside one repo the label must be unambiguous, because routing is by label.
func TestValidateRejectsDuplicateLabelInOneRepo(t *testing.T) {
	body := strings.Replace(multi, "labels: [eci-p2]", "labels: [eci-p1]", 1)
	// o/r2 takes every pool, so both p1 and p2 land there with the same label.
	_, err := Load(writeConfig(t, body))
	if err == nil || !strings.Contains(err.Error(), "claimed by both") {
		t.Fatalf("expected a duplicate label error, got %v", err)
	}
}
