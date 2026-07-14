package shim

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the top-level shim configuration.
type Config struct {
	RouterURL     string        `yaml:"router_url"`
	RouterToken   string        `yaml:"router_token"`
	MaxConcurrent int           `yaml:"max_concurrent"`
	Models        []ModelConfig `yaml:"models"`
	MetricsAddr   string        `yaml:"metrics_addr"` // e.g. ":9092"; empty = disabled
}

// ModelConfig describes a single model and its backend.
type ModelConfig struct {
	Name        string        `yaml:"name"`
	ContextSize int           `yaml:"context_size"` // reported to router; 0 = unknown
	Backend     BackendConfig `yaml:"backend"`
}

// BackendConfig describes how to fulfil requests for a model.
type BackendConfig struct {
	Type       string `yaml:"type"`        // "http" | "command"
	URL        string `yaml:"url"`         // base URL; type=http only
	Format     string `yaml:"format"`      // "openai" | "anthropic"; type=http only
	AuthType   string `yaml:"auth_type"`   // "bearer" | "header" | "none"; type=http only
	AuthHeader string `yaml:"auth_header"` // header name when auth_type=header
	AuthValue  string `yaml:"auth_value"`  // value; ${VAR} expanded from environment
	Command    string `yaml:"command"`     // shell command; type=command only; ${VAR} expanded
}

// LoadConfig reads and validates the YAML config at path.
// ${VAR} references in auth_value and command fields are expanded from the environment.
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
	for i := range cfg.Models {
		rawAuth := cfg.Models[i].Backend.AuthValue
		cfg.Models[i].Backend.AuthValue = os.Expand(rawAuth, os.Getenv)
		// Warn when a ${VAR} reference expanded to empty — otherwise the shim
		// silently sends no credential and the failure only surfaces as an
		// upstream 401.
		if strings.Contains(rawAuth, "${") && cfg.Models[i].Backend.AuthValue == "" {
			fmt.Fprintf(os.Stderr, "warning: model %q: auth_value %q expanded to empty (unset environment variable?)\n",
				cfg.Models[i].Name, rawAuth)
		}
		cfg.Models[i].Backend.Command = os.Expand(cfg.Models[i].Backend.Command, os.Getenv)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) validate() error {
	if c.RouterURL == "" {
		return fmt.Errorf("config: router_url is required")
	}
	if c.RouterToken == "" {
		return fmt.Errorf("config: router_token is required")
	}
	if len(c.Models) == 0 {
		return fmt.Errorf("config: at least one model must be configured")
	}
	for i, m := range c.Models {
		if m.Name == "" {
			return fmt.Errorf("config: model[%d]: name is required", i)
		}
		switch m.Backend.Type {
		case "http":
			if m.Backend.URL == "" {
				return fmt.Errorf("config: model %q: backend.url is required for type=http", m.Name)
			}
			if m.Backend.Format != "openai" && m.Backend.Format != "anthropic" {
				return fmt.Errorf("config: model %q: backend.format must be \"openai\" or \"anthropic\"", m.Name)
			}
			switch m.Backend.AuthType {
			case "", "none", "bearer":
			case "header":
				if m.Backend.AuthHeader == "" {
					return fmt.Errorf("config: model %q: backend.auth_header is required when auth_type=header", m.Name)
				}
			default:
				return fmt.Errorf("config: model %q: backend.auth_type must be \"bearer\", \"header\", or \"none\"", m.Name)
			}
		case "command":
			if m.Backend.Command == "" {
				return fmt.Errorf("config: model %q: backend.command is required for type=command", m.Name)
			}
		default:
			return fmt.Errorf("config: model %q: backend.type must be \"http\" or \"command\"", m.Name)
		}
	}
	return nil
}

// BackendFor returns the BackendConfig for the given model name, or nil if not found.
func (c *Config) BackendFor(model string) *BackendConfig {
	for i := range c.Models {
		if c.Models[i].Name == model {
			return &c.Models[i].Backend
		}
	}
	return nil
}
