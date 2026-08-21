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
metrics_sources:
  - name: prometheus
    type: prometheus
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
		{"metrics_sources[0].name", "  - name: prometheus\n", "  - name: \"\"\n"},
		{"metrics_sources[0].type", "    type: prometheus\n", "    type: influx\n"},
		{"metrics_sources[0].url", "    url: https://prometheus.example\n", ""},
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
	if cfg.KBDir != "/tmp/shackleton/kb" || cfg.InventoryDir != "/tmp/shackleton/inventory" {
		t.Fatalf("directory defaults = %q, %q", cfg.KBDir, cfg.InventoryDir)
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

func TestFreshRereadsFileSecretsAndKeepsEnvSecretsStatic(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "prom-auth")
	if err := os.WriteFile(secretPath, []byte("Bearer first\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := validConfig(t, "")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(data), "auth_header: {env: CONFIG_PROM_AUTH}",
		fmt.Sprintf("auth_header: {file: %q}", secretPath), 1)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.MetricsSources[0].AuthHeader.Fresh(); got != "Bearer first" {
		t.Fatalf("initial fresh value = %q", got)
	}
	if err := os.WriteFile(secretPath, []byte("Bearer rotated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := cfg.MetricsSources[0].AuthHeader.Fresh(); got != "Bearer rotated" {
		t.Fatalf("rotated fresh value = %q", got)
	}
	if got := cfg.MetricsSources[0].AuthHeader.Value(); got != "Bearer first" {
		t.Fatalf("cached value changed to %q", got)
	}
	if err := os.WriteFile(secretPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := cfg.MetricsSources[0].AuthHeader.Fresh(); got != "Bearer first" {
		t.Fatalf("empty file should fall back to startup value, got %q", got)
	}
	t.Setenv("CONFIG_API_KEY", "changed-after-load")
	if got := cfg.Model.APIKey.Fresh(); got != "environment-secret" {
		t.Fatalf("env-backed fresh value = %q", got)
	}
}

func TestNotificationChannelValidation(t *testing.T) {
	t.Setenv("CONFIG_BOT_TOKEN", "bot-secret")
	t.Setenv("CONFIG_CHAT_ID", "12345")
	valid := "notifications:\n  - name: ops\n    type: telegram\n    bot_token: {env: CONFIG_BOT_TOKEN}\n    chat_id: {env: CONFIG_CHAT_ID}\n"
	cfg, err := Load(validConfig(t, valid))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Notifications) != 1 || cfg.Notifications[0].BotToken.Value() != "bot-secret" || cfg.Notifications[0].ChatID.Value() != "12345" {
		t.Fatalf("channel = %+v", cfg.Notifications)
	}
	for _, test := range []struct {
		wantErr string
		extra   string
	}{
		{"notifications[1].name \"ops\" is duplicated", valid + "  - name: ops\n    type: telegram\n    bot_token: {env: CONFIG_BOT_TOKEN}\n    chat_id: {env: CONFIG_CHAT_ID}\n"},
		{"notifications[0].type \"pager\" is not supported", "notifications:\n  - name: ops\n    type: pager\n"},
		{"notifications[0].bot_token is required", "notifications:\n  - name: ops\n    type: telegram\n    chat_id: {env: CONFIG_CHAT_ID}\n"},
		{"approvals[0].type \"pager\" is not supported", "approvals:\n  - name: approvers\n    type: pager\n"},
	} {
		if _, err := Load(validConfig(t, test.extra)); err == nil || !strings.Contains(err.Error(), test.wantErr) {
			t.Errorf("want %q, got %v", test.wantErr, err)
		}
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

func TestLoadValidatesTLS(t *testing.T) {
	cfg, err := Load(validConfig(t, "tls:\n  cert_file: /etc/shackleton/tls.crt\n  key_file: /etc/shackleton/tls.key\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLS.CertFile != "/etc/shackleton/tls.crt" || cfg.TLS.KeyFile != "/etc/shackleton/tls.key" {
		t.Fatalf("tls = %+v", cfg.TLS)
	}
	for _, partial := range []string{
		"tls:\n  cert_file: /etc/shackleton/tls.crt\n",
		"tls:\n  key_file: /etc/shackleton/tls.key\n",
	} {
		if _, err := Load(validConfig(t, partial)); err == nil || !strings.Contains(err.Error(), "tls requires both cert_file and key_file") {
			t.Fatalf("partial tls error = %v", err)
		}
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
