package client

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type ModelConfig struct {
	Name     string `yaml:"name"`
	Endpoint string `yaml:"endpoint"`
}

type Config struct {
	RouterURL     string        `yaml:"router_url"`
	RouterToken   string        `yaml:"router_token"`
	MaxConcurrent int           `yaml:"max_concurrent"`
	Models        []ModelConfig `yaml:"models"`
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
	if cfg.MaxConcurrent == 0 {
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
