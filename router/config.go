package router

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config holds the router's runtime configuration.
type Config struct {
	Server struct {
		Port int `yaml:"port"`
		// TrustProxyHeaders controls whether X-Forwarded-For / X-Forwarded-Proto
		// are honoured. Leave false unless the router sits behind a trusted
		// reverse proxy that sets them — otherwise a client can spoof its IP and
		// bypass per-IP rate limiting. Default: false.
		TrustProxyHeaders bool `yaml:"trust_proxy_headers"`
		// MaxRequestMB caps the inbound API request body size, in mebibytes.
		// Raise it to accept multimodal requests (base64 images/audio), which
		// easily exceed a text-only budget. Default: 8. Clamped to 15 so a body
		// that clears ingress still fits the 16 MiB client WebSocket frame limit.
		MaxRequestMB int `yaml:"max_request_mb"`
	} `yaml:"server"`
	Name     string `yaml:"name"` // brand name shown on landing page
	Host     string `yaml:"host"` // hostname clients use to connect
	Timeouts struct {
		// TTFTMinutes is the time allowed from enqueue to first token (covers queue wait +
		// prompt eval on slow hardware). Default: 15.
		TTFTMinutes int `yaml:"ttft_minutes"`
		// ActivityMinutes is the maximum silence between tokens before a stream is aborted.
		// Default: 5.
		ActivityMinutes int `yaml:"activity_minutes"`
		// BatchMinutes is the total timeout for non-streaming requests per attempt. Default: 10.
		BatchMinutes int `yaml:"batch_minutes"`
		// KeepAliveSeconds is the SSE keep-alive comment interval. Default: 15.
		KeepAliveSeconds int `yaml:"keep_alive_seconds"`
		// LeaseMinutes is how long a dispatched job may remain in-flight before the slot
		// is reclaimed. Should be >= TTFTMinutes + ActivityMinutes. Default: 20.
		LeaseMinutes int `yaml:"lease_minutes"`
		// QueueMaxDepth is the maximum number of requests held in the queue before new
		// requests are rejected with HTTP 429. 0 means unlimited. Default: 0.
		QueueMaxDepth int `yaml:"queue_max_depth"`
	} `yaml:"timeouts"`
}

// Timeouts returns resolved time.Duration values, applying defaults for any zero fields.
func (c *Config) ResolvedTimeouts() Timeouts {
	t := Timeouts{
		TTFT:          15 * time.Minute,
		Activity:      5 * time.Minute,
		Batch:         10 * time.Minute,
		KeepAlive:     15 * time.Second,
		Lease:         20 * time.Minute,
		QueueMaxDepth: 0,
	}
	if v := c.Timeouts.TTFTMinutes; v > 0 {
		t.TTFT = time.Duration(v) * time.Minute
	}
	if v := c.Timeouts.ActivityMinutes; v > 0 {
		t.Activity = time.Duration(v) * time.Minute
	}
	if v := c.Timeouts.BatchMinutes; v > 0 {
		t.Batch = time.Duration(v) * time.Minute
	}
	if v := c.Timeouts.KeepAliveSeconds; v > 0 {
		t.KeepAlive = time.Duration(v) * time.Second
	}
	if v := c.Timeouts.LeaseMinutes; v > 0 {
		t.Lease = time.Duration(v) * time.Minute
	}
	if v := c.Timeouts.QueueMaxDepth; v > 0 {
		t.QueueMaxDepth = v
	}
	return t
}

// Timeouts holds resolved duration values for use by handler and hub.
type Timeouts struct {
	TTFT          time.Duration
	Activity      time.Duration
	Batch         time.Duration
	KeepAlive     time.Duration
	Lease         time.Duration
	QueueMaxDepth int
}

// MaxRequestBytes returns the resolved inbound request body limit in bytes,
// applying the default (8 MiB) when unset. The api.Handler clamps the value to
// its ceiling; returning 0 here means "use the handler default".
func (c *Config) MaxRequestBytes() int {
	if c.Server.MaxRequestMB <= 0 {
		return 0
	}
	return c.Server.MaxRequestMB << 20
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
	if cfg.Name == "" {
		cfg.Name = "llmesh"
	}
	if cfg.Host == "" {
		cfg.Host = "llmesh.example.com"
	}
	return &cfg, nil
}
