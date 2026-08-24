package scheduler

import (
	"log/slog"
	"testing"
	"time"

	"llmesh/pkg/types"
	"llmesh/router/internal/queue"
)

// orderSpy is a Dispatcher that records what had already happened at the moment
// the job went on the wire. A client cannot answer before the send, but it can
// answer immediately after, so whatever the completion path expects to find must
// already be in place by then.
type orderSpy struct {
	client types.ClientSummary

	inFlight  int
	tracked   bool
	untracked bool
	// untrackLost makes UntrackJob report that someone else already took the
	// record, as the hub's disconnect sweep does when it wins the race.
	untrackLost   bool
	sendOK        bool
	sends         int
	countAtSend   int
	trackedAtSend bool
}

func (s *orderSpy) AvailableClientList() []types.ClientSummary {
	return []types.ClientSummary{s.client}
}

func (s *orderSpy) SendToClient(clientID string, msg any) bool {
	s.sends++
	s.countAtSend = s.inFlight
	s.trackedAtSend = s.tracked
	return s.sendOK
}

func (s *orderSpy) IncrInFlight(clientID string) { s.inFlight++ }
func (s *orderSpy) DecrInFlight(clientID string) { s.inFlight-- }

func (s *orderSpy) TrackJob(clientID string, req types.InferenceRequest) bool {
	s.tracked = true
	return true
}

func (s *orderSpy) UntrackJob(clientID, requestID string) bool {
	s.untracked = true
	s.tracked = false
	return !s.untrackLost
}

func (s *orderSpy) NonOwnerInFlight(clientID, owner, model string) int { return 0 }

func orderSpyFixture(sendOK bool) (*Scheduler, *queue.Queue, *orderSpy) {
	spy := &orderSpy{
		client: types.ClientSummary{
			ID:            "c1",
			Owner:         "alice",
			Models:        map[string]bool{"llama3": true},
			MaxConcurrent: 2,
		},
		sendOK: sendOK,
	}
	q := queue.New()
	q.Push(types.InferenceRequest{
		ID: "req-1", Model: "llama3", Owner: "alice", EnqueuedAt: time.Now(),
	})
	return New(q, spy, noAlias{}, slog.Default()), q, spy
}

// TestDispatch_JobIsTrackedAndCountedBeforeItGoesOnTheWire pins the ordering that
// makes a fast reply safe. A client that answers the instant it receives the job —
// a prompt-cache hit, or a shim relaying an API response it already has — races the
// dispatching goroutine. If the record is written after the send, the completion
// finds no job to untrack, so the slot is never released and the request's tokens
// and timings are never recorded; if the count is raised after the record exists,
// the reply's decrement can land first and the count leaks upward instead.
func TestDispatch_JobIsTrackedAndCountedBeforeItGoesOnTheWire(t *testing.T) {
	s, _, spy := orderSpyFixture(true)
	s.drainQueue()

	if spy.sends != 1 {
		t.Fatalf("sends: got %d, want 1", spy.sends)
	}
	if !spy.trackedAtSend {
		t.Error("job was sent before it was tracked: a reply arriving immediately " +
			"would find no record, leaking the slot and losing its perf sample")
	}
	if spy.countAtSend != 1 {
		t.Errorf("in-flight count at send: got %d, want 1 — a reply's DecrInFlight "+
			"could land before the IncrInFlight and leak the count upward", spy.countAtSend)
	}
}

// TestDispatch_FailedSendLeavesNoPhantomJob covers the cost of tracking earlier:
// the record now exists before the send can fail, so the failure path has to undo
// it. Left behind, it holds a slot until the lease reaper clears it minutes later
// and shows a job the client never received.
func TestDispatch_FailedSendLeavesNoPhantomJob(t *testing.T) {
	s, q, spy := orderSpyFixture(false)
	s.drainQueue()

	if !spy.untracked {
		t.Error("send failed but the job stayed tracked: a phantom in-flight job " +
			"holds the slot until its lease expires")
	}
	if spy.inFlight != 0 {
		t.Errorf("in-flight count after failed send: got %d, want 0", spy.inFlight)
	}
	if q.Len() != 1 {
		t.Errorf("queue length after failed send: got %d, want 1 (requeued)", q.Len())
	}
}
