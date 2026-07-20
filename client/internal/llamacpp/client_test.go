package llamacpp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestInferUsageToUsageInfo(t *testing.T) {
	// A nil backend usage passes straight through as nil.
	if (*inferUsage)(nil).toUsageInfo() != nil {
		t.Error("nil inferUsage should map to nil UsageInfo")
	}

	// prompt_tokens_details.cached_tokens is carried onto CacheReadTokens.
	var u inferUsage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":100,"completion_tokens":20,"total_tokens":120,"prompt_tokens_details":{"cached_tokens":80}}`), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := u.toUsageInfo()
	if got.PromptTokens != 100 || got.CompletionTokens != 20 || got.TotalTokens != 120 {
		t.Errorf("base token counts wrong: %+v", got)
	}
	if got.CacheReadTokens != 80 {
		t.Errorf("CacheReadTokens = %d, want 80", got.CacheReadTokens)
	}

	// A backend that reports no cache details leaves CacheReadTokens zero.
	var noCache inferUsage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}`), &noCache); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if noCache.toUsageInfo().CacheReadTokens != 0 {
		t.Error("CacheReadTokens should be 0 when backend reports no cache details")
	}
}

func TestProbeModelID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"object":"list","data":[{"id":"deepseek-v4-flash","object":"model"}]}`))
	}))
	defer srv.Close()

	got := New(srv.URL, nil).ProbeModelID(context.Background())
	if got != "deepseek-v4-flash" {
		t.Errorf("ProbeModelID = %q, want deepseek-v4-flash", got)
	}
}

func TestApplyHeaders(t *testing.T) {
	var gotAuth, gotCustom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCustom = r.Header.Get("X-Api-Key")
		w.Write([]byte(`{"object":"list","data":[{"id":"m","object":"model"}]}`))
	}))
	defer srv.Close()

	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer secret-key")
	hdr.Set("X-Api-Key", "gateway-token")
	New(srv.URL, hdr).ProbeModelID(context.Background())

	if gotAuth != "Bearer secret-key" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer secret-key")
	}
	if gotCustom != "gateway-token" {
		t.Errorf("X-Api-Key = %q, want %q", gotCustom, "gateway-token")
	}
}

func TestApplyHeaders_NilNoAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h := r.Header.Get("Authorization"); h != "" {
			t.Errorf("unexpected Authorization header %q for nil headers", h)
		}
		w.Write([]byte(`{"object":"list","data":[{"id":"m","object":"model"}]}`))
	}))
	defer srv.Close()
	New(srv.URL, nil).ProbeModelID(context.Background())
}

func TestProbeModelID_Errors(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"empty data", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"object":"list","data":[]}`))
		}},
		{"server error", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}},
		{"malformed json", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`not json`))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			if got := New(srv.URL, nil).ProbeModelID(context.Background()); got != "" {
				t.Errorf("expected empty model id, got %q", got)
			}
		})
	}
}
