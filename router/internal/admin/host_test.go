package admin

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestNormalizePortalHost(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},    // blank clears the override
		{"   ", "", false}, // whitespace only clears too
		{"llm.example.com", "llm.example.com", false},             // plain hostname
		{"llm.example.com:8443", "llm.example.com:8443", false},   // host:port
		{"https://llm.example.com", "llm.example.com", false},     // scheme stripped
		{"https://llm.example.com/v1/", "llm.example.com", false}, // scheme+path stripped
		{"10.0.0.5:53002", "10.0.0.5:53002", false},               // IP:port
		{"bad host", "", true},                                    // space inside is invalid
		{"llm.example.com/path", "llm.example.com", false},        // trailing path stripped
		{"has\"quote", "", true},                                  // quote would corrupt YAML/URL
	}
	for _, c := range cases {
		got, err := normalizePortalHost(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("normalizePortalHost(%q): expected error, got %q", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("normalizePortalHost(%q): unexpected error %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("normalizePortalHost(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPortalHost_SetGetClear(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	if h := s.PortalHost(); h != "" {
		t.Fatalf("expected empty by default, got %q", h)
	}
	if err := s.SetPortalHost("llm.example.com"); err != nil {
		t.Fatal(err)
	}
	if h := s.PortalHost(); h != "llm.example.com" {
		t.Fatalf("got %q after set", h)
	}
	// Empty clears the override.
	if err := s.SetPortalHost(""); err != nil {
		t.Fatal(err)
	}
	if h := s.PortalHost(); h != "" {
		t.Fatalf("expected cleared, got %q", h)
	}
}

func TestEffectiveHost_Precedence(t *testing.T) {
	s, _ := LoadState(filepath.Join(t.TempDir(), "state.json"))
	a := &Admin{state: s, host: defaultConfiguredHost}

	req := httptest.NewRequest("GET", "https://auto.detected.example/portal/", nil)
	req.Host = "auto.detected.example"

	// 1. With only the placeholder configured, auto-detect from the request wins
	//    over showing the placeholder.
	if got := a.effectiveHost(req); got != "auto.detected.example" {
		t.Fatalf("auto-detect: got %q", got)
	}

	// 2. A real configured host beats auto-detection.
	a.host = "configured.example.com"
	if got := a.effectiveHost(req); got != "configured.example.com" {
		t.Fatalf("config host: got %q", got)
	}

	// 3. An admin override beats everything.
	if err := s.SetPortalHost("override.example.com"); err != nil {
		t.Fatal(err)
	}
	if got := a.effectiveHost(req); got != "override.example.com" {
		t.Fatalf("override: got %q", got)
	}
}

func TestRequestHost_ForwardedHeader(t *testing.T) {
	req := httptest.NewRequest("GET", "http://direct.example/portal/", nil)
	req.Host = "direct.example"
	req.Header.Set("X-Forwarded-Host", "public.example.com, internal.example")

	// Not trusted: the direct Host header is used, the forwarded header ignored.
	if got := requestHost(req, false); got != "direct.example" {
		t.Fatalf("untrusted: got %q", got)
	}
	// Trusted: the first hop of X-Forwarded-Host is used.
	if got := requestHost(req, true); got != "public.example.com" {
		t.Fatalf("trusted: got %q", got)
	}
}
