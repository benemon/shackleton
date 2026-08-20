package config

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"gopkg.in/yaml.v3"
)

type SecretRef struct {
	Env  string `json:"env,omitempty" yaml:"env,omitempty"`
	File string `json:"file,omitempty" yaml:"file,omitempty"`
}

func (SecretRef) String() string            { return "<redacted>" }
func (SecretRef) GoString() string          { return "<redacted>" }
func (SecretRef) MarshalYAML() (any, error) { return "<redacted>", nil }
func (r *SecretRef) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("must be a mapping with exactly one of env or file")
	}
	for i := 0; i < len(n.Content); i += 2 {
		key := n.Content[i].Value
		if key != "env" && key != "file" {
			return fmt.Errorf("field %s not found in type config.SecretRef", key)
		}
	}
	type plain SecretRef
	if err := n.Decode((*plain)(r)); err != nil {
		return err
	}
	return nil
}

type Secret struct {
	ref   SecretRef
	value string
}

func (Secret) String() string            { return "<redacted>" }
func (Secret) GoString() string          { return "<redacted>" }
func (Secret) MarshalYAML() (any, error) { return "<redacted>", nil }
func (s *Secret) UnmarshalYAML(n *yaml.Node) error {
	return n.Decode(&s.ref)
}
func (s Secret) Value() string  { return s.value }
func (s Secret) Ref() SecretRef { return s.ref }
func (s Secret) IsSet() bool    { return s.ref.Env != "" || s.ref.File != "" }

// Fresh re-reads a file-backed secret so short-TTL credentials re-rendered by
// an external agent are picked up without a daemon restart. Env-backed and
// unset refs return the startup value; so does a file that is momentarily
// missing or empty mid-rotation.
func (s Secret) Fresh() string {
	if s.ref.File == "" {
		return s.value
	}
	data, err := os.ReadFile(s.ref.File)
	if err != nil {
		return s.value
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return s.value
	}
	return value
}

type Duration struct {
	value time.Duration
	err   error
}

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var value string
	if err := n.Decode(&value); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		d.err = err
		return nil
	}
	d.value = parsed
	return nil
}

func (d Duration) Duration() time.Duration { return d.value }

type Model struct {
	BaseURL string `yaml:"base_url"`
	Name    string `yaml:"name"`
	APIKey  Secret `yaml:"api_key"`
}

type MCPServer struct {
	Name       string `yaml:"name"`
	URL        string `yaml:"url"`
	AuthHeader Secret `yaml:"auth_header,omitempty"`
}

// MetricsSource is a named metrics query API instance. Type prometheus is
// the only implemented type; each source registers native query tools named
// after it (query_<name>_instant, query_<name>_range).
type MetricsSource struct {
	Name       string `yaml:"name"`
	Type       string `yaml:"type"`
	URL        string `yaml:"url"`
	AuthHeader Secret `yaml:"auth_header,omitempty"`
}

// LogsSource is a named log query API instance. Type loki is the only
// implemented type; each source registers query_<name>_logs.
type LogsSource struct {
	Name       string `yaml:"name"`
	Type       string `yaml:"type"`
	URL        string `yaml:"url"`
	AuthHeader Secret `yaml:"auth_header,omitempty"`
}

// Channel is a named delivery channel instance, used by both the
// notifications list (unprivileged: sweep verdicts, answers, the Q&A
// trigger) and the approvals list (privileged: Approve/Deny). The same type
// — telegram is the only implemented one — may appear in both with
// different credentials or audiences.
type Channel struct {
	Name     string `yaml:"name"`
	Type     string `yaml:"type"`
	BotToken Secret `yaml:"bot_token,omitempty"`
	ChatID   Secret `yaml:"chat_id,omitempty"`
}

type TLS struct {
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

type Agent struct {
	Prompt               string   `yaml:"prompt,omitempty"`
	MaxRounds            int      `yaml:"max_rounds"`
	MaxToolResultChars   int      `yaml:"max_tool_result_chars,omitempty"`
	CallTimeout          Duration `yaml:"call_timeout"`
	InvestigationTimeout Duration `yaml:"investigation_timeout"`
}

type Sweep struct {
	Name     string `yaml:"name"`
	Schedule string `yaml:"schedule"`
	Question string `yaml:"question"`
	schedule cron.Schedule
}

func (s Sweep) Parsed() cron.Schedule { return s.schedule }

type Config struct {
	Listen         string          `yaml:"listen"`
	TLS            TLS             `yaml:"tls,omitempty"`
	StateDir       string          `yaml:"state_dir"`
	EnvFiles       []string        `yaml:"env_files,omitempty"`
	Model          Model           `yaml:"model"`
	MCPServers     []MCPServer     `yaml:"mcp_servers"`
	MetricsSources []MetricsSource `yaml:"metrics_sources,omitempty"`
	LogsSources    []LogsSource    `yaml:"logs_sources,omitempty"`
	GatedTools     []string        `yaml:"gated_tools"`
	Notifications  []Channel       `yaml:"notifications,omitempty"`
	Approvals      []Channel       `yaml:"approvals,omitempty"`
	Agent          Agent           `yaml:"agent"`
	Sweeps         []Sweep         `yaml:"sweeps,omitempty"`
	APIToken       Secret          `yaml:"api_token"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	for _, envFile := range cfg.EnvFiles {
		if err := LoadEnvFile(envFile); err != nil {
			return nil, fmt.Errorf("env_files %q: %w", envFile, err)
		}
	}
	if err := cfg.applyDefaultsAndValidate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaultsAndValidate() error {
	if c.StateDir == "" {
		return fmt.Errorf("state_dir is required")
	}
	if c.Listen != "" {
		_, port, err := net.SplitHostPort(c.Listen)
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return fmt.Errorf("listen: invalid port %q", port)
		}
	}
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		return fmt.Errorf("tls requires both cert_file and key_file")
	}
	if c.Model.BaseURL == "" {
		return fmt.Errorf("model.base_url is required")
	}
	if c.Model.Name == "" {
		return fmt.Errorf("model.name is required")
	}
	if err := c.Model.APIKey.resolve("model.api_key", true); err != nil {
		return err
	}
	if len(c.MCPServers) == 0 {
		return fmt.Errorf("mcp_servers requires at least one server")
	}
	for i := range c.MCPServers {
		server := &c.MCPServers[i]
		prefix := fmt.Sprintf("mcp_servers[%d]", i)
		if server.Name == "" {
			return fmt.Errorf("%s.name is required", prefix)
		}
		if server.URL == "" {
			return fmt.Errorf("%s.url is required", prefix)
		}
		if err := server.AuthHeader.resolve(prefix+".auth_header", false); err != nil {
			return err
		}
	}
	metricsNames := make(map[string]bool, len(c.MetricsSources))
	for i := range c.MetricsSources {
		source := &c.MetricsSources[i]
		prefix := fmt.Sprintf("metrics_sources[%d]", i)
		if err := validateSource(prefix, source.Name, source.Type, source.URL, "prometheus", metricsNames); err != nil {
			return err
		}
		if err := source.AuthHeader.resolve(prefix+".auth_header", false); err != nil {
			return err
		}
	}
	logsNames := make(map[string]bool, len(c.LogsSources))
	for i := range c.LogsSources {
		source := &c.LogsSources[i]
		prefix := fmt.Sprintf("logs_sources[%d]", i)
		if err := validateSource(prefix, source.Name, source.Type, source.URL, "loki", logsNames); err != nil {
			return err
		}
		if err := source.AuthHeader.resolve(prefix+".auth_header", false); err != nil {
			return err
		}
	}
	if err := validateChannels("notifications", c.Notifications); err != nil {
		return err
	}
	if err := validateChannels("approvals", c.Approvals); err != nil {
		return err
	}
	if c.Agent.MaxRounds == 0 {
		c.Agent.MaxRounds = 8
	}
	if c.Agent.MaxRounds < 1 {
		return fmt.Errorf("agent.max_rounds must be at least 1")
	}
	if c.Agent.MaxToolResultChars == 0 {
		c.Agent.MaxToolResultChars = 30000
	}
	if c.Agent.MaxToolResultChars < 1 {
		return fmt.Errorf("agent.max_tool_result_chars must be at least 1")
	}
	if c.Agent.CallTimeout.err != nil {
		return fmt.Errorf("agent.call_timeout: %w", c.Agent.CallTimeout.err)
	}
	if c.Agent.CallTimeout.value == 0 {
		c.Agent.CallTimeout.value = 30 * time.Second
	}
	if c.Agent.CallTimeout.value < 0 {
		return fmt.Errorf("agent.call_timeout must be positive")
	}
	if c.Agent.InvestigationTimeout.err != nil {
		return fmt.Errorf("agent.investigation_timeout: %w", c.Agent.InvestigationTimeout.err)
	}
	if c.Agent.InvestigationTimeout.value == 0 {
		c.Agent.InvestigationTimeout.value = 10 * time.Minute
	}
	if c.Agent.InvestigationTimeout.value < 0 {
		return fmt.Errorf("agent.investigation_timeout must be positive")
	}
	names := make(map[string]bool, len(c.Sweeps))
	for i := range c.Sweeps {
		sweep := &c.Sweeps[i]
		prefix := fmt.Sprintf("sweeps[%d]", i)
		if sweep.Name == "" {
			return fmt.Errorf("%s.name is required", prefix)
		}
		if names[sweep.Name] {
			return fmt.Errorf("%s.name %q is duplicated", prefix, sweep.Name)
		}
		names[sweep.Name] = true
		if sweep.Question == "" {
			return fmt.Errorf("%s.question is required", prefix)
		}
		if sweep.Schedule == "" {
			return fmt.Errorf("%s.schedule is required", prefix)
		}
		parsed, err := cron.ParseStandard(sweep.Schedule)
		if err != nil {
			return fmt.Errorf("%s.schedule: %w", prefix, err)
		}
		sweep.schedule = parsed
	}
	if err := c.APIToken.resolve("api_token", false); err != nil {
		return err
	}
	return nil
}

func validateChannels(list string, channels []Channel) error {
	names := make(map[string]bool, len(channels))
	for i := range channels {
		channel := &channels[i]
		prefix := fmt.Sprintf("%s[%d]", list, i)
		if channel.Name == "" {
			return fmt.Errorf("%s.name is required", prefix)
		}
		if names[channel.Name] {
			return fmt.Errorf("%s.name %q is duplicated", prefix, channel.Name)
		}
		names[channel.Name] = true
		if channel.Type != "telegram" {
			return fmt.Errorf("%s.type %q is not supported (want telegram)", prefix, channel.Type)
		}
		if err := channel.BotToken.resolve(prefix+".bot_token", true); err != nil {
			return err
		}
		if err := channel.ChatID.resolve(prefix+".chat_id", true); err != nil {
			return err
		}
	}
	return nil
}

func validateSource(prefix, name, sourceType, url, wantType string, seen map[string]bool) error {
	if name == "" {
		return fmt.Errorf("%s.name is required", prefix)
	}
	if seen[name] {
		return fmt.Errorf("%s.name %q is duplicated", prefix, name)
	}
	seen[name] = true
	if sourceType != wantType {
		return fmt.Errorf("%s.type %q is not supported (want %s)", prefix, sourceType, wantType)
	}
	if url == "" {
		return fmt.Errorf("%s.url is required", prefix)
	}
	return nil
}

func (s *Secret) resolve(field string, required bool) error {
	if s.ref.Env == "" && s.ref.File == "" {
		if required {
			return fmt.Errorf("%s is required", field)
		}
		return nil
	}
	if s.ref.Env != "" && s.ref.File != "" {
		return fmt.Errorf("%s must contain exactly one of env or file", field)
	}
	if s.ref.Env != "" {
		value, ok := os.LookupEnv(s.ref.Env)
		if !ok || value == "" {
			return fmt.Errorf("%s: environment variable %s is not set", field, s.ref.Env)
		}
		s.value = value
		return nil
	}
	value, err := os.ReadFile(s.ref.File)
	if err != nil {
		return fmt.Errorf("%s: file %s: %w", field, s.ref.File, err)
	}
	s.value = strings.TrimSpace(string(value))
	if s.value == "" {
		return fmt.Errorf("%s: file %s is empty", field, s.ref.File)
	}
	return nil
}

// LoadEnvFile parses KEY=value lines and preserves non-empty environment values.
func LoadEnvFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for line := range strings.Lines(string(data)) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok || os.Getenv(key) != "" {
			continue
		}
		value = strings.Trim(value, `"'`)
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	return nil
}
