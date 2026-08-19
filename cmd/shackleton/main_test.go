package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/benemon/shackleton/internal/config"
)

func TestLoadEnvFilePreservesExistingEnvironment(t *testing.T) {
	t.Setenv("SHACKLETON_EXISTING", "from-environment")
	t.Setenv("SHACKLETON_LOADED", "")
	path := filepath.Join(t.TempDir(), "test.env")
	if err := os.WriteFile(path, []byte("# comment\nSHACKLETON_EXISTING=from-file\nSHACKLETON_LOADED='value with spaces'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := config.LoadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("SHACKLETON_EXISTING"); got != "from-environment" {
		t.Fatalf("existing environment was overwritten: %q", got)
	}
	if got := os.Getenv("SHACKLETON_LOADED"); got != "value with spaces" {
		t.Fatalf("dotenv value was not loaded: %q", got)
	}
}

func TestValidateServeConfigRequiresListenAndAPIToken(t *testing.T) {
	t.Setenv("CONFIG_API_KEY", "model-secret")
	t.Setenv("CONFIG_PROM_AUTH", "prom-secret")
	t.Setenv("CONFIG_API_TOKEN", "api-secret")
	path := filepath.Join(t.TempDir(), "shackleton.yaml")
	contents := `
listen: "127.0.0.1:8420"
state_dir: /tmp/shackleton
model:
  base_url: https://model.example/v1
  name: test-model
  api_key: {env: CONFIG_API_KEY}
mcp_servers:
  - name: remediation
    url: http://127.0.0.1:8100/mcp
prometheus:
  url: https://prometheus.example
  auth_header: {env: CONFIG_PROM_AUTH}
gated_tools: []
api_token: {env: CONFIG_API_TOKEN}
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateServeConfig(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Listen = ""
	if err := validateServeConfig(cfg); err == nil || err.Error() != "listen is required for serve" {
		t.Fatalf("empty listen error = %v", err)
	}
	cfg.Listen = "127.0.0.1:8420"
	cfg.APIToken = config.Secret{}
	if err := validateServeConfig(cfg); err == nil || err.Error() != "api_token is required for serve" {
		t.Fatalf("empty API token error = %v", err)
	}
}
