package config

import (
	"os"
	"path/filepath"
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
  installation_id: 2
  private_key_path: /tmp/app.pem
  owner: o
  repo: r
eci:
  region_id: cn-hangzhou
  vswitch_ids: [vsw-1]
  security_group_id: sg-1
pools:
  - name: p
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
