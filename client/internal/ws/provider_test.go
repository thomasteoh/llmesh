package ws

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	clientPkg "llmesh/client"
	"llmesh/client/internal/health"
)

// fakeBackend answers like llama.cpp, with /health switchable.
func fakeBackend(t *testing.T, healthy *atomic.Bool) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			if !healthy.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Write([]byte(`{"status":"ok"}`))
		case "/v1/models":
			json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"id": "fake-7b"}}})
		case "/props":
			w.Write([]byte(`{"n_ctx":8192,"total_slots":2}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func names(p *clientModelProvider) []string {
	ms, _ := p.Models(context.Background())
	var out []string
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}

func TestProviderTracksBackendHealth(t *testing.T) {
	var healthy atomic.Bool
	url := fakeBackend(t, &healthy)
	br := health.NewBreaker(nil)
	p := &clientModelProvider{cfg: &clientPkg.Config{Models: []clientPkg.ModelConfig{{Endpoint: url}}},
		breaker: br, backends: map[string]*backendState{}}

	if got := names(p); len(got) != 0 {
		t.Fatalf("a backend still loading was advertised: %v", got)
	}
	healthy.Store(true)
	if got := names(p); len(got) != 1 || got[0] != "fake-7b" {
		t.Fatalf("a ready backend was not advertised: %v", got)
	}

	// One or two missed checks are tolerated; the third withdraws it.
	healthy.Store(false)
	for i := 1; i < downAfter; i++ {
		if got := names(p); len(got) != 1 {
			t.Fatalf("withdrawn after %d missed check(s)", i)
		}
	}
	if got := names(p); len(got) != 0 {
		t.Fatalf("still advertised after %d missed checks: %v", downAfter, got)
	}
	healthy.Store(true)
	if got := names(p); len(got) != 1 {
		t.Fatal("not advertised again once healthy")
	}

	// A backend busy with requests is not withdrawn for missing checks.
	br.Start("fake-7b")
	healthy.Store(false)
	for i := 0; i < downAfter+2; i++ {
		if got := names(p); len(got) != 1 {
			t.Fatal("withdrawn while serving requests")
		}
	}
	br.Done("fake-7b")
	healthy.Store(true)

	// A model the breaker has tripped is not advertised.
	for i := 0; i < health.TripAfter; i++ {
		br.Failure("fake-7b")
	}
	if got := names(p); len(got) != 0 {
		t.Fatalf("a tripped model was advertised: %v", got)
	}
}
