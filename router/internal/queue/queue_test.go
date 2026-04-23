package queue

import (
	"testing"
	"time"
	"llmesh/pkg/types"
)

func req(model string, priority types.Priority, age time.Duration) types.InferenceRequest {
	return types.InferenceRequest{
		Model:      model,
		Priority:   priority,
		EnqueuedAt: time.Now().Add(-age),
	}
}

func TestPopBest_EmptyQueue(t *testing.T) {
	q := New()
	result := q.PopBest(map[string]bool{"llama": true}, nil)
	if result != nil {
		t.Fatalf("expected nil from empty queue, got %+v", result)
	}
}

func TestPopBest_HighBeforeLow(t *testing.T) {
	q := New()
	q.Push(req("llama", types.PriorityLow, 0))
	q.Push(req("llama", types.PriorityHigh, 0))
	result := q.PopBest(map[string]bool{"llama": true}, nil)
	if result == nil {
		t.Fatal("expected a result")
	}
	if result.Priority != types.PriorityHigh {
		t.Fatalf("expected high priority, got %d", result.Priority)
	}
}

func TestPopBest_FIFOWithinPriority(t *testing.T) {
	q := New()
	q.Push(req("llama", types.PriorityNormal, 2*time.Second))
	q.Push(req("llama", types.PriorityNormal, 0))
	result := q.PopBest(map[string]bool{"llama": true}, nil)
	if result == nil {
		t.Fatal("expected a result")
	}
	if result.EnqueuedAt.After(time.Now().Add(-1 * time.Second)) {
		t.Fatal("expected older request first")
	}
}

func TestPopBest_SkipsUnavailableModel(t *testing.T) {
	q := New()
	q.Push(req("model-a", types.PriorityHigh, 0))
	q.Push(req("model-b", types.PriorityLow, 0))
	result := q.PopBest(map[string]bool{"model-b": true}, nil)
	if result == nil {
		t.Fatal("expected a result")
	}
	if result.Model != "model-b" {
		t.Fatalf("expected model-b, got %s", result.Model)
	}
}

func TestPopBest_MatchesByAlias(t *testing.T) {
	q := New()
	q.Push(req("unsloth/qwen3-30b-a3b", types.PriorityHigh, 0))
	// Client has no direct model match but alias "qwen" → "unsloth/qwen3-30b-a3b".
	// Request model stored as alias "qwen" should NOT match — alias lookup is
	// req.Model in aliases, not model name in aliases. So this test verifies
	// that a request FOR the canonical name is matched when aliases map is provided.
	result := q.PopBest(map[string]bool{"unsloth/qwen3-30b-a3b": true}, map[string][]string{"qwen": {"unsloth/qwen3-30b-a3b"}})
	if result == nil || result.Model != "unsloth/qwen3-30b-a3b" {
		t.Fatalf("expected canonical model match, got %+v", result)
	}
}

func TestPopBest_RequestByAlias(t *testing.T) {
	q := New()
	// Request comes in as alias "qwen".
	r := req("qwen", types.PriorityHigh, 0)
	q.Push(r)
	// Client has model "unsloth/qwen3-30b-a3b" with alias "qwen".
	result := q.PopBest(map[string]bool{"unsloth/qwen3-30b-a3b": true}, map[string][]string{"qwen": {"unsloth/qwen3-30b-a3b"}})
	if result == nil {
		t.Fatal("expected alias match, got nil")
	}
	if result.Model != "qwen" {
		t.Fatalf("queue should preserve alias in model field, got %s", result.Model)
	}
}

func TestLen(t *testing.T) {
	q := New()
	if q.Len() != 0 {
		t.Fatal("expected 0")
	}
	q.Push(req("llama", types.PriorityNormal, 0))
	if q.Len() != 1 {
		t.Fatal("expected 1")
	}
}
