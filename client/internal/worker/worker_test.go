package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	clientPkg "llmesh/client"
	"llmesh/pkg/types"
)

// fakeLlamaCpp returns a test HTTP server that always responds with the given
// status code and body.
func fakeLlamaCpp(t *testing.T, statusCode int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(statusCode)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func configWith(model, endpoint string) *clientPkg.Config {
	return &clientPkg.Config{
		Models: []clientPkg.ModelConfig{
			{Name: model, Endpoint: endpoint},
		},
	}
}

func TestHandle_InferError_SendsReleaseMsg(t *testing.T) {
	// llama.cpp returns 500 → Infer fails → worker must send ReleaseMsg.
	srv := fakeLlamaCpp(t, 500, `{"error":"model overloaded"}`)
	cfg := configWith("llama3", srv.URL)

	job := types.JobMsg{
		Type: "job",
		Request: types.InferenceRequest{
			ID:    "req-worker-1",
			Model: "llama3",
		},
	}

	var sent []any
	sendFn := SendFn(func(msg any) error {
		sent = append(sent, msg)
		return nil
	})

	Handle(context.Background(), job, cfg, sendFn)

	if len(sent) == 0 {
		t.Fatal("expected at least one message sent")
	}
	// Last message should be a ReleaseMsg.
	data, _ := json.Marshal(sent[len(sent)-1])
	var rel types.ReleaseMsg
	if err := json.Unmarshal(data, &rel); err != nil {
		t.Fatalf("last message is not JSON: %v", err)
	}
	if rel.Type != "release" {
		t.Errorf("expected type=release, got %q", rel.Type)
	}
	if rel.RequestID != "req-worker-1" {
		t.Errorf("expected request_id=req-worker-1, got %q", rel.RequestID)
	}
	if rel.Reason != "model_failed" {
		t.Errorf("expected reason=model_failed, got %q", rel.Reason)
	}
}

func TestHandle_NoEndpoint_SendsErrorMsg(t *testing.T) {
	// No endpoint configured for the requested model → must send ErrorMsg (not release).
	cfg := configWith("other-model", "http://localhost:9999")

	job := types.JobMsg{
		Type: "job",
		Request: types.InferenceRequest{
			ID:    "req-worker-2",
			Model: "llama3", // not in cfg
		},
	}

	var sent []any
	sendFn := SendFn(func(msg any) error {
		sent = append(sent, msg)
		return nil
	})

	Handle(context.Background(), job, cfg, sendFn)

	if len(sent) != 1 {
		t.Fatalf("expected 1 message, got %d", len(sent))
	}
	data, _ := json.Marshal(sent[0])
	var errMsg types.ErrorMsg
	if err := json.Unmarshal(data, &errMsg); err != nil {
		t.Fatalf("message is not ErrorMsg JSON: %v", err)
	}
	if errMsg.Type != "error" {
		t.Errorf("expected type=error, got %q", errMsg.Type)
	}
}
