package llamacpp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmesh/pkg/types"
)

func TestBackendErrorCarriesReason(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   string
		final  bool
	}{
		{400, `{"error":{"code":400,"message":"the request exceeds the available context size","type":"exceed_context_size_error"}}`, "the request exceeds the available context size", true},
		{422, `{"error":"bad tool schema"}`, "bad tool schema", true},
		{413, `{"message":"too big"}`, "too big", true},
		{401, `{"error":{"message":"invalid api key"}}`, "invalid api key", false},
		{503, `Loading model`, "Loading model", false},
		{500, ``, "", false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			w.Write([]byte(tc.body))
		}))
		err := New(srv.URL, nil).Infer(context.Background(), types.InferenceRequest{Model: "m"}, "", func(Chunk) {})
		srv.Close()
		var be *BackendError
		if !errors.As(err, &be) {
			t.Fatalf("%d: error %v is not a BackendError", tc.status, err)
		}
		if be.Status != tc.status || be.Message != tc.want || be.Final() != tc.final {
			t.Errorf("%d: got status %d message %q final %v; want %q final %v", tc.status, be.Status, be.Message, be.Final(), tc.want, tc.final)
		}
		if tc.want != "" && !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%d: error text %q lacks the reason", tc.status, err)
		}
	}
}

func TestReady(t *testing.T) {
	status := http.StatusServiceUnavailable
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
	}))
	defer srv.Close()
	c := New(srv.URL, nil)
	if c.Ready(context.Background()) {
		t.Error("a backend still loading (503) reported ready")
	}
	status = http.StatusOK
	if !c.Ready(context.Background()) {
		t.Error("a healthy backend reported not ready")
	}
	status = http.StatusNotFound // a server with no /health answered, so it is up
	if !c.Ready(context.Background()) {
		t.Error("a backend without /health reported not ready")
	}
	if New("http://127.0.0.1:1", nil).Ready(context.Background()) {
		t.Error("an unreachable backend reported ready")
	}
}
