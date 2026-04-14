// router/config.go
package router

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
	"llmesh/pkg/types"
)

// Config holds the router's runtime configuration.
type Config struct {
	Server struct {
		Port        int    `yaml:"port"`
		ClientToken string `yaml:"client_token"`
	} `yaml:"server"`
	APIKeys []APIKeyConfig `yaml:"api_keys"`
}

// APIKeyConfig describes one API key and its priority tier.
type APIKeyConfig struct {
	Key      string `yaml:"key"`
	Label    string `yaml:"label"`
	Priority string `yaml:"priority"` // "high", "normal", "low"
}

// LoadConfig reads a YAML config file at path.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 53002
	}
	return &cfg, nil
}

// APIKeyMap returns a map from API key string to its config entry.
func (c *Config) APIKeyMap() map[string]APIKeyConfig {
	m := make(map[string]APIKeyConfig, len(c.APIKeys))
	for _, k := range c.APIKeys {
		m[k.Key] = k
	}
	return m
}

// PriorityFor returns the Priority for the given API key. Defaults to Normal.
func (c *Config) PriorityFor(key string) types.Priority {
	for _, k := range c.APIKeys {
		if k.Key == key {
			return types.PriorityFromString(k.Priority)
		}
	}
	return types.PriorityNormal
}
