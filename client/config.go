package client

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type ModelConfig struct {
	Name         string `yaml:"name"`
	Endpoint     string `yaml:"endpoint"`
	ChatTemplate string `yaml:"chat_template,omitempty"` // overrides model's built-in Jinja template
}

type Config struct {
	RouterURL              string        `yaml:"router_url"`
	RouterToken            string        `yaml:"router_token"`
	MaxConcurrent          int           `yaml:"max_concurrent"`
	Models                 []ModelConfig `yaml:"models"`
	MetricsAddr            string        `yaml:"metrics_addr"`             // e.g. ":9091"; empty = disabled
	RouterActivityTimeout  time.Duration `yaml:"router_activity_timeout"`  // derive keep-alive interval; 0 = use 60s default
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
	if cfg.MaxConcurrent < 1 {
		cfg.MaxConcurrent = 4
	}
	return &cfg, nil
}

// EndpointFor returns the llama.cpp endpoint for the given model name.
// Returns "" if the model is not configured.
func (c *Config) EndpointFor(model string) string {
	for _, m := range c.Models {
		if m.Name == model {
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

// ChatTemplateFor returns the configured chat template override for the given model.
// Returns "" if none is set (llama.cpp will use the model's built-in template).
func (c *Config) ChatTemplateFor(model string) string {
	for _, m := range c.Models {
		if m.Name == model {
			return m.ChatTemplate
		}
	}
	return ""
}
