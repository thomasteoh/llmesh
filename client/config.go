package client

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type ModelConfig struct {
	// Name is optional. When omitted, the client auto-detects the model name
	// from the endpoint's /v1/models response at registration time.
	Name         string `yaml:"name,omitempty"`
	Endpoint     string `yaml:"endpoint"`
	ChatTemplate string `yaml:"chat_template,omitempty"` // overrides model's built-in Jinja template
}

type Config struct {
	RouterURL             string        `yaml:"router_url"`
	RouterToken           string        `yaml:"router_token"`
	// MaxConcurrent caps the number of simultaneous jobs accepted from the router.
	// Defaults to 1 when omitted; set a higher value to allow more parallel jobs.
	MaxConcurrent         int           `yaml:"max_concurrent"`
	Models                []ModelConfig `yaml:"models"`
	MetricsAddr           string        `yaml:"metrics_addr"`            // e.g. ":9091"; empty = disabled
	RouterActivityTimeout time.Duration `yaml:"router_activity_timeout"` // derive keep-alive interval; 0 = use 60s default

	// AutoUpdate enables periodic hourly checks for a new binary. The manifest URL
	// is derived automatically from router_url. When a new version is found and the
	// client is idle (no active jobs), the binary is replaced and the process
	// re-executes itself. Has no effect on dev builds.
	// Portal-triggered updates (via the admin UI) work regardless of this setting.
	AutoUpdate bool `yaml:"auto_update"`

	detectedTemplates sync.Map // model name → chat_template detected from /props; not from config file
	resolvedNames     sync.Map // endpoint → model name auto-detected from /v1/models when config name is omitted
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}
	// max_concurrent defaults to 1 when omitted; override in config.
	if cfg.MaxConcurrent == 0 {
		cfg.MaxConcurrent = 1
	}
	return &cfg, nil
}

// Validate checks that all required fields are present and well-formed.
func (c *Config) Validate() error {
	if c.RouterURL == "" {
		return fmt.Errorf("router_url is required")
	}
	u, err := url.ParseRequestURI(c.RouterURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("router_url must start with http:// or https://")
	}
	if c.RouterToken == "" {
		return fmt.Errorf("router_token is required")
	}
	if c.MaxConcurrent < 0 {
		return fmt.Errorf("max_concurrent must be >= 0")
	}
	for i, m := range c.Models {
		// Name is optional — when omitted it is auto-detected from the endpoint.
		if strings.TrimSpace(m.Endpoint) == "" {
			return fmt.Errorf("models[%d]: endpoint is required", i)
		}
		eu, err := url.ParseRequestURI(m.Endpoint)
		if err != nil || (eu.Scheme != "http" && eu.Scheme != "https") {
			return fmt.Errorf("models[%d]: endpoint must start with http:// or https://", i)
		}
	}
	return nil
}

// effectiveName returns the model's configured name, or the name auto-detected
// from its endpoint when no name was configured. Returns "" if neither is known.
func (c *Config) effectiveName(m ModelConfig) string {
	if m.Name != "" {
		return m.Name
	}
	if v, ok := c.resolvedNames.Load(m.Endpoint); ok {
		return v.(string)
	}
	return ""
}

// SetResolvedName records a model name auto-detected from the given endpoint.
func (c *Config) SetResolvedName(endpoint, name string) {
	if name != "" {
		c.resolvedNames.Store(endpoint, name)
	}
}

// SetDetectedTemplate stores a chat template auto-detected from llama.cpp's /props
// for the given model. Used as a fallback when no manual override is configured.
func (c *Config) SetDetectedTemplate(model, template string) {
	if template != "" {
		c.detectedTemplates.Store(model, template)
	}
}

// EndpointFor returns the llama.cpp endpoint for the given model name.
// Returns "" if the model is not configured.
func (c *Config) EndpointFor(model string) string {
	for _, m := range c.Models {
		if en := c.effectiveName(m); en != "" && en == model {
			return m.Endpoint
		}
	}
	return ""
}

// KeepAliveInterval returns the worker keep-alive interval derived from
// RouterActivityTimeout. The keep-alive is sent to prevent the router's
// activity timer from firing during long prompt evaluation.
// Defaults to 60s (safe for the default 5-minute router activity timeout).
// Caps at 60s to avoid unexpectedly long silences on low-latency setups.
func (c *Config) KeepAliveInterval() time.Duration {
	if c.RouterActivityTimeout <= 0 {
		return 60 * time.Second
	}
	half := c.RouterActivityTimeout / 2
	if half > 60*time.Second {
		return 60 * time.Second
	}
	return half
}

// ChatTemplateFor returns the chat template for the given model.
// Manual config override takes priority; falls back to auto-detected template from /props.
// Returns "" if neither is set (llama.cpp will use the model's built-in template).
func (c *Config) ChatTemplateFor(model string) string {
	for _, m := range c.Models {
		if en := c.effectiveName(m); en != "" && en == model && m.ChatTemplate != "" {
			return m.ChatTemplate
		}
	}
	if t, ok := c.detectedTemplates.Load(model); ok {
		return t.(string)
	}
	return ""
}
