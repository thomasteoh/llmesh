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

	got := New(srv.URL).ProbeModelID(context.Background())
	if got != "deepseek-v4-flash" {
		t.Errorf("ProbeModelID = %q, want deepseek-v4-flash", got)
	}
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
			if got := New(srv.URL).ProbeModelID(context.Background()); got != "" {
				t.Errorf("expected empty model id, got %q", got)
			}
		})
	}
}
