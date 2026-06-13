package client

import (
	"testing"
)

func TestConfigValidate(t *testing.T) {
	good := Config{
		RouterURL:   "http://localhost:8080",
		RouterToken: "tok",
		Models: []ModelConfig{
			{Name: "llama3", Endpoint: "http://localhost:8081"},
		},
	}

	if err := good.Validate(); err != nil {
		t.Fatalf("expected valid config to pass: %v", err)
	}

	cases := []struct {
		name string
		cfg  Config
	}{
		{"missing router_url", Config{RouterToken: "tok"}},
		{"bad router_url scheme", Config{RouterURL: "ftp://x", RouterToken: "tok"}},
		{"missing router_token", Config{RouterURL: "http://x"}},
		{"negative max_concurrent", Config{RouterURL: "http://x", RouterToken: "tok", MaxConcurrent: -1}},
		{"model missing name", Config{
			RouterURL: "http://x", RouterToken: "tok",
			Models: []ModelConfig{{Endpoint: "http://localhost:8081"}},
		}},
		{"model missing endpoint", Config{
			RouterURL: "http://x", RouterToken: "tok",
			Models: []ModelConfig{{Name: "m"}},
		}},
		{"model bad endpoint scheme", Config{
			RouterURL: "http://x", RouterToken: "tok",
			Models: []ModelConfig{{Name: "m", Endpoint: "not-a-url"}},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}
