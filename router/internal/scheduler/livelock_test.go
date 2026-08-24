package scheduler

import (
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"llmesh/pkg/types"
)

// zombieQueue models a queue whose selection and removal disagree: it always
// offers the same request and never lets it be popped. That is the shape a
// duplicate request ID used to leave the real queue in — two entries sharing an
// ID, byID pointing at only one, so the stranded copy stays visible to
// PeekBestForClient while PopByID can never remove it.
type zombieQueue struct {
	req   types.InferenceRequest
	peeks atomic.Int64
}

func (z *zombieQueue) PeekBestForClient(map[string]bool, map[string][]string, string, func(string) bool) *types.InferenceRequest {
	z.peeks.Add(1)
	cp := z.req
	return &cp
}

func (z *zombieQueue) PopByID(string) *types.InferenceRequest { return nil }
func (z *zombieQueue) Push(types.InferenceRequest)            {}

// countingDispatcher reports one always-free client and records dispatches.
type countingDispatcher struct{ sent atomic.Int64 }

func (d *countingDispatcher) AvailableClientList() []types.ClientSummary {
	return []types.ClientSummary{{
		ID:            "c1",
		Owner:         "alice",
		Models:        map[string]bool{"llama3": true},
		MaxConcurrent: 4,
	}}
}

func (d *countingDispatcher) SendToClient(string, any) bool {
	d.sent.Add(1)
	return true
}
func (d *countingDispatcher) IncrInFlight(string)                          {}
func (d *countingDispatcher) DecrInFlight(string)                          {}
func (d *countingDispatcher) TrackJob(string, types.InferenceRequest) bool { return true }
func (d *countingDispatcher) UntrackJob(string, string)                    {}
func (d *countingDispatcher) NonOwnerInFlight(string, string, string) int  { return 0 }

// A queue that offers a request it cannot remove must not spin the drain loop.
// The loop runs on the single scheduler goroutine, so spinning means no request
// ever dispatches again for the life of the process, and the burnt CPU starves
// the local inference workers the router schedules onto.
func TestDrainQueue_TerminatesOnUnremovableRequest(t *testing.T) {
	z := &zombieQueue{req: types.InferenceRequest{ID: "zombie", Model: "llama3", Owner: "alice"}}
	s := New(z, &countingDispatcher{}, noAlias{}, slog.Default())

	done := make(chan struct{})
	go func() {
		s.drainQueue()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("drainQueue did not terminate; it peeked %d times", z.peeks.Load())
	}

	// It should give up almost immediately rather than grinding through the
	// whole queue: one selection, one failed pop, one repeat, then out.
	if got := z.peeks.Load(); got > 4 {
		t.Errorf("drainQueue peeked %d times before giving up, want at most 4", got)
	}
}

// The ordinary case — a request consumed concurrently by someone else — must
// still let the drain carry on, since that request really is gone and the next
// selection returns something different.
func TestDrainQueue_ContinuesAfterConcurrentConsumption(t *testing.T) {
	q := &drainingQueue{reqs: []types.InferenceRequest{
		{ID: "gone", Model: "llama3", Owner: "alice"},
		{ID: "real", Model: "llama3", Owner: "alice"},
	}}
	d := &countingDispatcher{}
	s := New(q, d, noAlias{}, slog.Default())

	done := make(chan struct{})
	go func() {
		s.drainQueue()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("drainQueue did not terminate")
	}

	if got := d.sent.Load(); got != 1 {
		t.Errorf("dispatched %d jobs, want 1 — the drain stopped at the vanished request", got)
	}
}

// drainingQueue offers reqs in order. The first is unpoppable (as if consumed
// concurrently) but is dropped from the offer list once refused, so selection
// moves on — unlike zombieQueue, which never lets go.
type drainingQueue struct{ reqs []types.InferenceRequest }

func (d *drainingQueue) PeekBestForClient(map[string]bool, map[string][]string, string, func(string) bool) *types.InferenceRequest {
	if len(d.reqs) == 0 {
		return nil
	}
	cp := d.reqs[0]
	return &cp
}

func (d *drainingQueue) PopByID(id string) *types.InferenceRequest {
	if len(d.reqs) == 0 || d.reqs[0].ID != id {
		return nil
	}
	req := d.reqs[0]
	d.reqs = d.reqs[1:]
	if id == "gone" {
		return nil // vanished between selection and removal
	}
	return &req
}

func (d *drainingQueue) Push(types.InferenceRequest) {}
