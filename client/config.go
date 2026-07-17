package client

import (
	"fmt"
	"net/http"
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

	// APIKey, when set, is sent to the endpoint as "Authorization: Bearer <key>"
	// on every request. Use for backends that require authentication (vLLM,
	// LM Studio with an API key, llama.cpp --api-key, hosted OpenAI-compatible
	// servers). Empty means no Authorization header is added.
	APIKey string `yaml:"api_key,omitempty"`

	// Headers are extra HTTP headers sent to the endpoint on every request. Use
	// for gateways that need custom headers (e.g. "x-api-key", tenant routing).
	// A header set here overrides one llmesh would otherwise send (including
	// Authorization, so a non-standard auth header can replace the bearer scheme).
	Headers map[string]string `yaml:"headers,omitempty"`
}

// RequestHeaders returns the HTTP headers to attach to every request sent to
// this model's endpoint: an Authorization bearer header derived from APIKey (if
// set), plus any configured custom Headers. Custom headers are applied last so
// they take precedence over the derived Authorization header. Returns nil when
// nothing is configured, so callers can add it without allocating.
func (m ModelConfig) RequestHeaders() http.Header {
	if m.APIKey == "" && len(m.Headers) == 0 {
		return nil
	}
	h := http.Header{}
	if m.APIKey != "" {
		h.Set("Authorization", "Bearer "+m.APIKey)
	}
	for k, v := range m.Headers {
		h.Set(k, v)
	}
	return h
}

type Config struct {
	RouterURL   string `yaml:"router_url"`
	RouterToken string `yaml:"router_token"`
	// MaxConcurrent caps the number of simultaneous jobs accepted from the router.
	// When 0 (omitted), auto-detected from the llama.cpp total_slots field (min 1).
	MaxConcurrent int           `yaml:"max_concurrent"`
	Models        []ModelConfig `yaml:"models"`
	MetricsAddr   string        `yaml:"metrics_addr"`   // e.g. ":9091"; empty = disabled
	LocalAPIAddr  string        `yaml:"local_api_addr"` // e.g. "127.0.0.1:8089"; empty = disabled
	// LocalAPIToken, when set, requires callers of the local endpoint to present
	// it as "Authorization: Bearer <token>". Empty means the local endpoint is
	// unauthenticated — only safe on a loopback bind.
	LocalAPIToken         string        `yaml:"local_api_token"`
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
	return &cfg, nil
}

// Validate checks that all required fields are present and well-formed.
func (c *Config) Validate() error {
	if c.RouterURL == "" {
		return fmt.Errorf("router_url is required")
	}
	u, err := url.ParseRequestURI(c.RouterURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "ws" && u.Scheme != "wss") {
		return fmt.Errorf("router_url must start with http://, https://, ws:// or wss://")
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
		for name := range m.Headers {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("models[%d]: header name must not be empty", i)
			}
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

// HeadersFor returns the per-request HTTP headers configured for the given model
// (bearer api_key and/or custom headers). Returns nil when the model is unknown
// or has no headers configured.
func (c *Config) HeadersFor(model string) http.Header {
	for _, m := range c.Models {
		if en := c.effectiveName(m); en != "" && en == model {
			return m.RequestHeaders()
		}
	}
	return nil
}

// AvailableModels returns the resolved names of all configured models.
// Models whose names have not yet been auto-detected are omitted.
func (c *Config) AvailableModels() []string {
	names := make([]string, 0, len(c.Models))
	for _, m := range c.Models {
		if en := c.effectiveName(m); en != "" {
			names = append(names, en)
		}
	}
	return names
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
