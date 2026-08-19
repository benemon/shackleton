package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func validConfig(t *testing.T, extra string) string {
	t.Helper()
	t.Setenv("CONFIG_API_KEY", "environment-secret")
	t.Setenv("CONFIG_PROM_AUTH", "Bearer prometheus")
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
gated_tools: [run_host_command]
agent:
  max_rounds: 4
  call_timeout: 17s
  investigation_timeout: 3m
` + extra
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadValidatesRequiredFieldsAndAppliesDefaults(t *testing.T) {
	required := []struct {
		field string
		old   string
		new   string
	}{
		{"state_dir", "state_dir: /tmp/shackleton\n", ""},
		{"model.base_url", "  base_url: https://model.example/v1\n", ""},
		{"model.name", "  name: test-model\n", ""},
		{"model.api_key", "  api_key: {env: CONFIG_API_KEY}\n", ""},
		{"mcp_servers", "mcp_servers:\n  - name: remediation\n    url: http://127.0.0.1:8100/mcp\n", ""},
		{"mcp_servers[0].name", "  - name: remediation\n", "  - name: \"\"\n"},
		{"mcp_servers[0].url", "    url: http://127.0.0.1:8100/mcp\n", ""},
		{"prometheus.url", "  url: https://prometheus.example\n", ""},
		{"prometheus.auth_header", "  auth_header: {env: CONFIG_PROM_AUTH}\n", ""},
	}
	for _, test := range required {
		t.Run(test.field, func(t *testing.T) {
			path := validConfig(t, "")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = []byte(strings.Replace(string(data), test.old, test.new, 1))
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), test.field) {
				t.Fatalf("missing %s error = %v", test.field, err)
			}
		})
	}

	defaultPath := validConfig(t, "")
	data, err := os.ReadFile(defaultPath)
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(data), "agent:\n")
	if err := os.WriteFile(defaultPath, data[:start], 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(defaultPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.MaxRounds != 8 || cfg.Agent.CallTimeout.Duration() != 30*time.Second || cfg.Agent.InvestigationTimeout.Duration() != 10*time.Minute {
		t.Fatalf("defaults = %+v", cfg.Agent)
	}
}

func TestLoadResolvesEnvironmentAndFileSecretsFromTheirSources(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "mcp-secret")
	if err := os.WriteFile(secretPath, []byte("Bearer file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := validConfig(t, fmt.Sprintf("mcp_servers:\n  - name: remediation\n    url: http://127.0.0.1:8100/mcp\n    auth_header: {file: %q}\n", secretPath))
	// Remove the first mcp_servers block so the replacement is not a duplicate key.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	first := "mcp_servers:\n  - name: remediation\n    url: http://127.0.0.1:8100/mcp\n"
	text = strings.Replace(text, first, "", 1)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Model.APIKey.Value(); got != "environment-secret" {
		t.Fatalf("environment secret = %q", got)
	}
	if got := cfg.MCPServers[0].AuthHeader.Value(); got != "Bearer file-secret" {
		t.Fatalf("file secret = %q", got)
	}
	if got := cfg.Model.APIKey.Ref(); got.Env != "CONFIG_API_KEY" || got.File != "" {
		t.Fatalf("environment secret ref = %+v", got)
	}
	if got := cfg.MCPServers[0].AuthHeader.Ref(); got.File != secretPath || got.Env != "" {
		t.Fatalf("file secret ref = %+v", got)
	}
}

func TestAPITokenIsOptionalAndResolvedWhenSet(t *testing.T) {
	cfg, err := Load(validConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIToken.IsSet() || cfg.APIToken.Value() != "" {
		t.Fatalf("optional API token = ref %+v value %q", cfg.APIToken.Ref(), cfg.APIToken.Value())
	}
	t.Setenv("CONFIG_API_TOKEN", "api-secret")
	cfg, err = Load(validConfig(t, "api_token: {env: CONFIG_API_TOKEN}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIToken.Value() != "api-secret" || cfg.APIToken.Ref().Env != "CONFIG_API_TOKEN" {
		t.Fatalf("API token = ref %+v value %q", cfg.APIToken.Ref(), cfg.APIToken.Value())
	}
}

func TestLoadRejectsUnknownKeys(t *testing.T) {
	if _, err := Load(validConfig(t, "unknown_option: true\n")); err == nil || !strings.Contains(err.Error(), "unknown_option") {
		t.Fatalf("unknown key error = %v", err)
	}
}

func TestLoadRejectsInvalidListenAddress(t *testing.T) {
	path := validConfig(t, "")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `listen: "127.0.0.1:8420"`, `listen: "127.0.0.1:not-a-port"`, 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("invalid listen error = %v", err)
	}
}

func TestSecretsAreRedactedByFormatting(t *testing.T) {
	ref := SecretRef{Env: "VISIBLE_REF"}
	secret := Secret{ref: ref, value: "VISIBLE_VALUE"}
	for _, value := range []any{ref, secret} {
		for _, format := range []string{"%v", "%s", "%#v"} {
			printed := fmt.Sprintf(format, value)
			if printed != "<redacted>" {
				t.Fatalf("format %s printed %q", format, printed)
			}
		}
	}
	cfg, err := Load(validConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	printed := fmt.Sprintf("%v %#v", cfg, cfg)
	for _, leaked := range []string{"environment-secret", "Bearer prometheus", "CONFIG_API_KEY", "CONFIG_PROM_AUTH"} {
		if strings.Contains(printed, leaked) {
			t.Fatalf("config formatting leaked %q in %q", leaked, printed)
		}
	}
}

func TestLoadParsesDurations(t *testing.T) {
	cfg, err := Load(validConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent.CallTimeout.Duration() != 17*time.Second || cfg.Agent.InvestigationTimeout.Duration() != 3*time.Minute {
		t.Fatalf("durations = %s, %s", cfg.Agent.CallTimeout.Duration(), cfg.Agent.InvestigationTimeout.Duration())
	}
	path := validConfig(t, "")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "call_timeout: 17s", "call_timeout: eventually", 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "call_timeout") {
		t.Fatalf("invalid duration error = %v", err)
	}
}

func TestLoadValidatesSweeps(t *testing.T) {
	good := "sweeps:\n  - name: node-fs\n    schedule: \"0 */4 * * *\"\n    question: check filesystems\n"
	cfg, err := Load(validConfig(t, good))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sweeps) != 1 || cfg.Sweeps[0].Parsed() == nil {
		t.Fatalf("sweeps = %+v", cfg.Sweeps)
	}
	next := cfg.Sweeps[0].Parsed().Next(time.Date(2026, 1, 1, 1, 30, 0, 0, time.UTC))
	if want := time.Date(2026, 1, 1, 4, 0, 0, 0, time.UTC); !next.Equal(want) {
		t.Fatalf("next firing = %v, want %v", next, want)
	}
	bad := []struct {
		label string
		yaml  string
	}{
		{"sweeps[0].schedule", "sweeps:\n  - name: x\n    schedule: \"not cron\"\n    question: q\n"},
		{"sweeps[0].schedule is required", "sweeps:\n  - name: x\n    question: q\n"},
		{"sweeps[0].name is required", "sweeps:\n  - schedule: \"* * * * *\"\n    question: q\n"},
		{"sweeps[0].question is required", "sweeps:\n  - name: x\n    schedule: \"* * * * *\"\n"},
		{"sweeps[1].name \"x\" is duplicated", "sweeps:\n  - name: x\n    schedule: \"* * * * *\"\n    question: q\n  - name: x\n    schedule: \"* * * * *\"\n    question: q\n"},
	}
	for _, test := range bad {
		if _, err := Load(validConfig(t, test.yaml)); err == nil || !strings.Contains(err.Error(), test.label) {
			t.Fatalf("%s: error = %v", test.label, err)
		}
	}
}
