package scheduler

import (
	"testing"
)

// When a send fails, the scheduler undoes its own tracking and re-queues. But
// if the client died in the window after TrackJob, the hub's disconnect sweep
// has already taken the record and released the request. Untracking is what
// says which of the two happened: pushing after losing that race puts a second
// copy of the same request ID in the queue behind the hub's.
func TestDispatch_DoesNotRequeueWhenHubAlreadyTookTheRecord(t *testing.T) {
	s, q, spy := orderSpyFixture(false)
	spy.untrackLost = true

	s.drainQueue()

	if !spy.untracked {
		t.Fatal("scheduler did not attempt to untrack after a failed send")
	}
	if got := q.Len(); got != 0 {
		t.Errorf("queue depth = %d, want 0 — the scheduler re-queued a request the hub had already released", got)
	}
}

// The hub only decrements the in-flight count for a record it actually removed,
// so the scheduler must not decrement one it lost.
func TestDispatch_DoesNotDecrementWhenHubAlreadyTookTheRecord(t *testing.T) {
	s, _, spy := orderSpyFixture(false)
	spy.untrackLost = true

	s.drainQueue()

	if spy.inFlight != 1 {
		t.Errorf("in-flight = %d, want 1 — the count was decremented twice for one record", spy.inFlight)
	}
}

// Winning the untrack is the ordinary case and must still re-queue, or a
// request whose send merely hit a full buffer would be dropped silently.
func TestDispatch_RequeuesWhenItOwnsTheRecord(t *testing.T) {
	s, q, spy := orderSpyFixture(false)

	s.drainQueue()

	if !spy.untracked {
		t.Fatal("scheduler did not untrack after a failed send")
	}
	if got := q.Len(); got != 1 {
		t.Errorf("queue depth = %d, want 1 — the request was lost", got)
	}
	if spy.inFlight != 0 {
		t.Errorf("in-flight = %d, want 0", spy.inFlight)
	}
}
