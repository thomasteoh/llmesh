package admin

import (
	"testing"

	"llmesh/pkg/types"
)

func TestFilterQueueForUser(t *testing.T) {
	items := []types.InferenceRequest{
		{ID: "req-alice", Owner: "alice"},
		{ID: "req-bob", Owner: "bob"},
	}

	// Admin sees all.
	got := filterQueueForUser(items, User{Username: "admin", Role: "admin"})
	if len(got) != 2 {
		t.Errorf("admin: expected 2, got %d", len(got))
	}

	// Member sees only own.
	got = filterQueueForUser(items, User{Username: "alice", Role: "member"})
	if len(got) != 1 {
		t.Fatalf("alice: expected 1, got %d", len(got))
	}
	if got[0].Owner != "alice" {
		t.Errorf("alice: expected own item, got owner=%q", got[0].Owner)
	}

	// Member with no items sees nothing.
	got = filterQueueForUser(items, User{Username: "carol", Role: "member"})
	if len(got) != 0 {
		t.Errorf("carol: expected 0, got %d", len(got))
	}
}
