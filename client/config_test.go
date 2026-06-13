package client

import (
	"testing"
)

func TestConfigValidate(t *testing.T) {
	valid := []struct {
		name string
		cfg  Config
	}{
		{"explicit name", Config{
			RouterURL: "http://localhost:8080", RouterToken: "tok",
			Models: []ModelConfig{{Name: "llama3", Endpoint: "http://localhost:8081"}},
		}},
		{"name omitted (auto-detected)", Config{
			RouterURL: "http://localhost:8080", RouterToken: "tok",
			Models: []ModelConfig{{Endpoint: "http://localhost:8081"}},
		}},
	}
	for i := range valid {
		tc := &valid[i]
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); err != nil {
				t.Errorf("expected valid config to pass: %v", err)
			}
		})
	}

	cases := []struct {
		name string
		cfg  Config
	}{
		{"missing router_url", Config{RouterToken: "tok"}},
		{"bad router_url scheme", Config{RouterURL: "ftp://x", RouterToken: "tok"}},
		{"missing router_token", Config{RouterURL: "http://x"}},
		{"negative max_concurrent", Config{RouterURL: "http://x", RouterToken: "tok", MaxConcurrent: -1}},
		{"model missing endpoint", Config{
			RouterURL: "http://x", RouterToken: "tok",
			Models: []ModelConfig{{Name: "m"}},
		}},
		{"model bad endpoint scheme", Config{
			RouterURL: "http://x", RouterToken: "tok",
			Models: []ModelConfig{{Name: "m", Endpoint: "not-a-url"}},
		}},
	}

	for i := range cases {
		tc := &cases[i]
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); err == nil {
				t.Error("expected error, got nil")
			}
		})
	}
}

func TestEffectiveNameResolution(t *testing.T) {
	cfg := Config{
		Models: []ModelConfig{
			{Name: "explicit", Endpoint: "http://localhost:8081"},
			{Endpoint: "http://localhost:8082"}, // name auto-detected
		},
	}

	// Explicit name resolves immediately.
	if got := cfg.EndpointFor("explicit"); got != "http://localhost:8081" {
		t.Errorf("EndpointFor(explicit) = %q, want http://localhost:8081", got)
	}

	// Auto-detected name is unknown until resolved.
	if got := cfg.EndpointFor("detected"); got != "" {
		t.Errorf("EndpointFor(detected) before resolution = %q, want empty", got)
	}

	cfg.SetResolvedName("http://localhost:8082", "detected")
	if got := cfg.EndpointFor("detected"); got != "http://localhost:8082" {
		t.Errorf("EndpointFor(detected) after resolution = %q, want http://localhost:8082", got)
	}
}
