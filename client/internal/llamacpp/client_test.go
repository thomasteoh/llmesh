package llamacpp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
