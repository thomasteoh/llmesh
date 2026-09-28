package localapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	clientPkg "llmesh/client"
)

func newTestServer(token string) *Server {
	cfg := &clientPkg.Config{
		LocalAPIToken: token,
		Models:        []clientPkg.ModelConfig{{Name: "m", Endpoint: "http://127.0.0.1:1"}},
	}
	return New(cfg, nil, nil)
}

func TestHostIsAddress(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1:8089":           true,
		"localhost:8089":           true,
		"LOCALHOST":                true,
		"[::1]:8089":               true,
		"::1":                      true,
		"192.168.1.20:8089":        true,
		"rebind.evil.example:8089": false,
		"evil.example":             false,
		"":                         false,
	} {
		if got := hostIsAddress(host); got != want {
			t.Errorf("hostIsAddress(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestAdmit(t *testing.T) {
	cases := []struct {
		name   string
		token  string
		host   string
		origin string
		auth   string
		want   int
	}{
		{"loopback, no token", "", "127.0.0.1:8089", "", "", http.StatusOK},
		{"LAN IP, no token", "", "192.168.1.20:8089", "", "", http.StatusOK},
		{"rebound hostname, no token", "", "rebind.evil.example:8089", "", "", http.StatusForbidden},
		{"browser origin, no token", "", "127.0.0.1:8089", "https://evil.example", "", http.StatusForbidden},
		{"browser origin, valid token", "tok", "127.0.0.1:8089", "https://evil.example", "Bearer tok", http.StatusForbidden},
		{"hostname with valid token", "tok", "gpu.lan:8089", "", "Bearer tok", http.StatusOK},
		{"hostname with wrong token", "tok", "gpu.lan:8089", "", "Bearer nope", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestServer(tc.token)
			r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.auth != "" {
				r.Header.Set("Authorization", tc.auth)
			}
			w := httptest.NewRecorder()
			s.handleModels(w, r)
			if w.Code != tc.want {
				t.Errorf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body)
			}
		})
	}
}

func TestChatCompletionsRequiresJSON(t *testing.T) {
	s := newTestServer("")
	body := `{"model":"m","messages":[]}`
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x"} {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		r.Host = "127.0.0.1:8089"
		if ct != "" {
			r.Header.Set("Content-Type", ct)
		}
		w := httptest.NewRecorder()
		s.handleChatCompletions(w, r)
		if w.Code != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type %q: status = %d, want %d", ct, w.Code, http.StatusUnsupportedMediaType)
		}
	}
}
