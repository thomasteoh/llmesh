package client

import (
	"os"
	"path/filepath"
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
		{"model empty header name", Config{
			RouterURL: "http://x", RouterToken: "tok",
			Models: []ModelConfig{{Name: "m", Endpoint: "http://localhost:8081", Headers: map[string]string{"  ": "v"}}},
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

func TestLoadConfig_MaxConcurrent(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	base := "router_url: http://localhost:8080\nrouter_token: tok\nmodels:\n  - endpoint: http://localhost:8081\n"

	// Omitted → 0 (auto-detect from total_slots at connect time).
	cfg, err := LoadConfig(write(t, base))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.MaxConcurrent != 0 {
		t.Errorf("default max_concurrent = %d, want 0 (auto-detect)", cfg.MaxConcurrent)
	}

	// Explicit value is preserved.
	cfg, err = LoadConfig(write(t, base+"max_concurrent: 8\n"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.MaxConcurrent != 8 {
		t.Errorf("explicit max_concurrent = %d, want 8", cfg.MaxConcurrent)
	}
}

func TestRequestHeaders(t *testing.T) {
	// Nothing configured → nil (no headers added).
	if h := (ModelConfig{Endpoint: "http://x"}).RequestHeaders(); h != nil {
		t.Errorf("RequestHeaders with no auth = %v, want nil", h)
	}

	// api_key → bearer Authorization.
	h := (ModelConfig{APIKey: "sk-123"}).RequestHeaders()
	if got := h.Get("Authorization"); got != "Bearer sk-123" {
		t.Errorf("Authorization = %q, want %q", got, "Bearer sk-123")
	}

	// Custom headers are included, and override the derived Authorization.
	h = (ModelConfig{
		APIKey:  "sk-123",
		Headers: map[string]string{"X-Api-Key": "gw", "Authorization": "Custom xyz"},
	}).RequestHeaders()
	if got := h.Get("X-Api-Key"); got != "gw" {
		t.Errorf("X-Api-Key = %q, want gw", got)
	}
	if got := h.Get("Authorization"); got != "Custom xyz" {
		t.Errorf("Authorization override = %q, want %q", got, "Custom xyz")
	}
}

func TestHeadersFor(t *testing.T) {
	cfg := Config{Models: []ModelConfig{
		{Name: "auth", Endpoint: "http://localhost:8081", APIKey: "sk-1"},
		{Name: "open", Endpoint: "http://localhost:8082"},
	}}
	if got := cfg.HeadersFor("auth").Get("Authorization"); got != "Bearer sk-1" {
		t.Errorf("HeadersFor(auth) Authorization = %q, want Bearer sk-1", got)
	}
	if h := cfg.HeadersFor("open"); h != nil {
		t.Errorf("HeadersFor(open) = %v, want nil", h)
	}
	if h := cfg.HeadersFor("unknown"); h != nil {
		t.Errorf("HeadersFor(unknown) = %v, want nil", h)
	}
}

func TestRemoteUpdateEnabled(t *testing.T) {
	// Omitted → enabled (preserves existing behaviour).
	if !(&Config{}).RemoteUpdateEnabled() {
		t.Error("RemoteUpdateEnabled with field omitted = false, want true")
	}
	tru, fls := true, false
	if !(&Config{RemoteUpdate: &tru}).RemoteUpdateEnabled() {
		t.Error("RemoteUpdateEnabled(true) = false, want true")
	}
	if (&Config{RemoteUpdate: &fls}).RemoteUpdateEnabled() {
		t.Error("RemoteUpdateEnabled(false) = true, want false")
	}
}

func TestLoadConfig_RemoteUpdate(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(p, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := "router_url: http://localhost:8080\nrouter_token: tok\nmodels:\n  - endpoint: http://localhost:8081\n"

	// Omitted → nil → enabled.
	cfg, err := LoadConfig(write(t, base))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.RemoteUpdate != nil {
		t.Errorf("remote_update omitted = %v, want nil", cfg.RemoteUpdate)
	}
	if !cfg.RemoteUpdateEnabled() {
		t.Error("remote_update omitted should be enabled")
	}

	// Explicit false → disabled.
	cfg, err = LoadConfig(write(t, base+"remote_update: false\n"))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.RemoteUpdateEnabled() {
		t.Error("remote_update: false should be disabled")
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

func TestEffectiveModalities(t *testing.T) {
	detected := []string{"text", "vision"}

	// No explicit config → detected passed through unchanged (including nil).
	if got := (ModelConfig{}).EffectiveModalities(detected); len(got) != 2 || got[1] != "vision" {
		t.Errorf("no config = %v, want detected %v", got, detected)
	}
	if got := (ModelConfig{}).EffectiveModalities(nil); got != nil {
		t.Errorf("no config, nil detected = %v, want nil (unknown)", got)
	}

	// Explicit config wins and is normalised to include the text sentinel.
	got := (ModelConfig{Modalities: []string{"vision"}}).EffectiveModalities(nil)
	want := []string{"text", "vision"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("explicit config = %v, want %v", got, want)
	}

	// An explicit "text" is not duplicated.
	got = (ModelConfig{Modalities: []string{"text", "audio"}}).EffectiveModalities(detected)
	if len(got) != 2 || got[0] != "text" || got[1] != "audio" {
		t.Errorf("explicit with text = %v, want [text audio]", got)
	}
}
