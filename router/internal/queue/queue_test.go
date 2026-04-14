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
	result := q.PopBest(map[string]bool{"llama": true})
	if result != nil {
		t.Fatalf("expected nil from empty queue, got %+v", result)
	}
}

func TestPopBest_HighBeforeLow(t *testing.T) {
	q := New()
	q.Push(req("llama", types.PriorityLow, 0))
	q.Push(req("llama", types.PriorityHigh, 0))
	result := q.PopBest(map[string]bool{"llama": true})
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
	result := q.PopBest(map[string]bool{"llama": true})
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
	result := q.PopBest(map[string]bool{"model-b": true})
	if result == nil {
		t.Fatal("expected a result")
	}
	if result.Model != "model-b" {
		t.Fatalf("expected model-b, got %s", result.Model)
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
