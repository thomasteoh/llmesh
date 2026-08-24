package queue

import (
	"testing"
	"time"
)

// Requests with no ID are appended rather than collapsed, since an empty ID is
// not an identity. Removing one must still leave the index consistent for the
// rest: an upstream router forwards a peer-assigned ID verbatim, so a buggy or
// hostile peer can put blank IDs in the queue, and an index that disagrees with
// items is what wedges the scheduler's drain loop.
func TestRemoveAt_BlankIDsStayConsistent(t *testing.T) {
	q := New()
	for i := 0; i < 3; i++ {
		q.Push(idReq("", time.Duration(3-i)*time.Second))
	}
	if got := q.Len(); got != 3 {
		t.Fatalf("Len = %d, want 3 — blank IDs were collapsed", got)
	}

	for i := 3; i > 0; i-- {
		got := q.PopBest(dupModels, nil)
		if got == nil {
			t.Fatalf("PopBest returned nil with %d items left", i)
		}
		if q.Len() != i-1 {
			t.Fatalf("Len = %d after pop, want %d", q.Len(), i-1)
		}
	}
	if got := q.PeekBestForClient(dupModels, nil, "", nil); got != nil {
		t.Errorf("PeekBestForClient still offers a request from an empty queue")
	}
}

// Removing an item must not strand the item swapped into its slot: that item
// has to remain findable by ID, or it becomes a request the scheduler can
// select but never dispatch.
func TestRemoveAt_KeepsSwappedItemIndexed(t *testing.T) {
	q := New()
	q.Push(idReq("first", 3*time.Second))
	q.Push(idReq("second", 2*time.Second))
	q.Push(idReq("third", time.Second))

	// Removing the head swaps the tail into slot 0.
	if q.PopByID("first") == nil {
		t.Fatal("could not pop first")
	}
	for _, id := range []string{"second", "third"} {
		if q.PopByID(id) == nil {
			t.Fatalf("PopByID(%q) returned nil — the swap lost its index entry", id)
		}
	}
	if got := q.Len(); got != 0 {
		t.Errorf("Len = %d, want 0", got)
	}
}
