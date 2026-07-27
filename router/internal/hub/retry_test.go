package hub

import (
	"testing"

	"llmesh/pkg/types"
)

// The scheduler rewrites Model to a concrete name at dispatch. A retry that kept
// that name could never reach a lower-preference tier and would spend every
// remaining attempt on the model that just failed.
func TestRetryRequest_RestoresAliasSoRetriesCanFallBack(t *testing.T) {
	req := retryRequest(types.InferenceRequest{
		ID:             "r1",
		Model:          "local-llama", // resolved at dispatch
		RequestedModel: "chat",        // what the caller asked for
	})
	if req.Model != "chat" {
		t.Errorf("Model: got %q, want the alias chat restored", req.Model)
	}
	if req.RequestedModel != "chat" {
		t.Errorf("RequestedModel should survive for the next attempt, got %q", req.RequestedModel)
	}
	if req.Attempts != 1 {
		t.Errorf("Attempts: got %d, want 1", req.Attempts)
	}
}

// Restoring must be idempotent: a request can go round the queue up to
// MaxAttempts times, and each pass must leave the alias resolvable again rather
// than degrading toward a concrete name.
func TestRetryRequest_IsIdempotentAcrossAttempts(t *testing.T) {
	req := types.InferenceRequest{ID: "r1", Model: "chat", RequestedModel: "chat"}
	for i := 1; i < types.MaxAttempts; i++ {
		req = retryRequest(req)
		if req.Model != "chat" {
			t.Fatalf("attempt %d: Model got %q, want chat", i, req.Model)
		}
		if req.Attempts != i {
			t.Fatalf("attempt %d: Attempts got %d", i, req.Attempts)
		}
		req.Model = "local-llama" // simulate the next dispatch's rewrite
	}
}

// Requests already in flight when the router restarts carry no RequestedModel.
// They must keep their concrete model rather than being blanked.
func TestRetryRequest_WithoutRequestedModelKeepsConcreteName(t *testing.T) {
	req := retryRequest(types.InferenceRequest{ID: "r1", Model: "local-llama"})
	if req.Model != "local-llama" {
		t.Errorf("Model: got %q, want local-llama preserved", req.Model)
	}
	if req.Attempts != 1 {
		t.Errorf("Attempts: got %d, want 1", req.Attempts)
	}
}
