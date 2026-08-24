package queue

import (
	"testing"
	"time"

	"llmesh/pkg/types"
)

// idReq builds a request with an explicit ID; the package's req helper leaves
// ID empty, which is exactly the field these tests are about.
func idReq(id string, age time.Duration) types.InferenceRequest {
	r := req("llama3", types.PriorityNormal, age)
	r.ID = id
	return r
}

var dupModels = map[string]bool{"llama3": true}

// Pushing an ID that is already queued must supersede it, not add a second
// entry. Two entries sharing an ID leave byID pointing at only one; removing
// that one deletes the sole index entry and strands the other where
// PeekBestForClient can still see it but PopByID can never remove it.
func TestPush_DuplicateIDSupersedes(t *testing.T) {
	q := New()
	q.Push(idReq("r1", time.Second))
	q.Push(idReq("r1", 0))

	if got := q.Len(); got != 1 {
		t.Fatalf("Len = %d, want 1 — the duplicate was appended instead of replacing", got)
	}
	if q.PopByID("r1") == nil {
		t.Fatal("PopByID could not remove the request")
	}
	if got := q.Len(); got != 0 {
		t.Fatalf("Len = %d after popping the only ID, want 0 — a stranded copy remains", got)
	}
	if got := q.PeekBestForClient(dupModels, nil, "", nil); got != nil {
		t.Fatalf("PeekBestForClient still offers %q after it was popped", got.ID)
	}
}

// The superseding copy must be the one that survives: a re-queue after a
// disconnect carries the current attempt count, and keeping the stale copy
// would reset it and let a request retry more times than MaxAttempts allows.
func TestPush_DuplicateIDKeepsNewest(t *testing.T) {
	q := New()
	first := idReq("r1", time.Second)
	second := idReq("r1", time.Second)
	second.Attempts = 2
	q.Push(first)
	q.Push(second)

	got := q.PopByID("r1")
	if got == nil {
		t.Fatal("PopByID returned nil")
	}
	if got.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 — the stale copy won", got.Attempts)
	}
}

// Every selection path must agree with PopByID about what is in the queue.
func TestPush_DuplicateIDLeavesNoZombieForPopBest(t *testing.T) {
	q := New()
	q.Push(idReq("r1", time.Second))
	q.Push(idReq("r1", time.Second))

	if got := q.PopBest(dupModels, nil); got == nil {
		t.Fatal("PopBest returned nil for a queued request")
	}
	if got := q.PopBest(dupModels, nil); got != nil {
		t.Fatalf("PopBest returned a second copy of %q", got.ID)
	}
	if got := q.Len(); got != 0 {
		t.Errorf("Len = %d, want 0", got)
	}
}

// Distinct IDs must still queue independently.
func TestPush_DistinctIDsUnaffected(t *testing.T) {
	q := New()
	q.Push(idReq("r1", time.Second))
	q.Push(idReq("r2", time.Second))

	if got := q.Len(); got != 2 {
		t.Fatalf("Len = %d, want 2", got)
	}
	if q.PopByID("r1") == nil || q.PopByID("r2") == nil {
		t.Fatal("a distinct ID could not be popped")
	}
	if got := q.Len(); got != 0 {
		t.Errorf("Len = %d, want 0", got)
	}
}
