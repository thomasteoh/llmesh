package router

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Config holds the router's runtime configuration.
type Config struct {
	Server struct {
		Port int `yaml:"port"`
	} `yaml:"server"`
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
